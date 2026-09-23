// Package k6 implements a WorkloadAdapter for the k6 load testing tool.
// See https://grafana.com/docs/k6/latest/
package k6

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/dbarena/benchctl/internal/engine"
	"github.com/dbarena/benchctl/internal/schema"
)

type cmdRunner func(ctx context.Context, out io.Writer, name string, args ...string) ([]byte, error)

// Adapter implements engine.WorkloadAdapter for k6.
type Adapter struct {
	run cmdRunner
	out io.Writer
}

// New returns an Adapter that streams k6 output to stdout.
func New() *Adapter {
	return &Adapter{run: execRun, out: os.Stdout}
}

// newWithRunner returns an Adapter with an injected runner, for testing.
func newWithRunner(r cmdRunner) *Adapter {
	return &Adapter{
		run: r,
		out: io.Discard,
	}
}

// Run executes the k6 benchmark and returns parsed metrics.
func (a *Adapter) Run(ctx context.Context, outputs engine.Outputs, step schema.SuiteStep) (engine.Metrics, error) {
	args := buildCmd(outputs, step)
	out, err := a.run(ctx, a.out, "k6", args...)
	if err != nil {
		return nil, fmt.Errorf("k6 run: %w\noutput: %s", err, out)
	}

	metrics, err := parseMetrics()
	if err != nil {
		return nil, fmt.Errorf("k6 parse metrics: %w", err)
	}

	return metrics, nil
}

// VersionInfo runs "k6 version --json" and returns its version field.
// Best-effort: any failure to run or parse the command logs a warning to
// a.out and returns nil rather than failing the run.
func (a *Adapter) VersionInfo(ctx context.Context) map[string]string {
	out, err := a.run(ctx, io.Discard, "k6", "version", "--json")
	if err != nil {
		fmt.Fprintf(a.out, "warning: k6 version: %v\n", err)
		return nil
	}

	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		fmt.Fprintf(a.out, "warning: k6 version: unmarshal: %v\n", err)
		return nil
	}

	return map[string]string{
		"k6_version": v.Version,
	}
}

// buildCmd constructs the k6 invocation.
//
// step.Command is the script file path.
// step.Args are passed as --vus, --duration, --iterations, etc.
// Target outputs are forwarded to the k6 script as --env KEY=VALUE.
func buildCmd(outputs engine.Outputs, step schema.SuiteStep) []string {
	args := []string{"run", "--summary-export", "summary.json", "--new-machine-readable-summary"}

	// Pass target outputs to k6 as environment variables (sorted for determinism).
	keys := make([]string, 0, len(outputs))
	for k := range outputs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--env", k+"="+outputs[k])
	}

	for k, v := range step.Args {
		if v == "" {
			args = append(args, "--"+k)
		} else {
			args = append(args, "--"+k, v)
		}
	}

	if step.Command != "" {
		args = append(args, step.Command)
	}

	return args
}

// parseMetrics reads summary.json from the current working directory and
// extracts the metrics k6 wrote there.
func parseMetrics() (engine.Metrics, error) {
	data, err := os.ReadFile("summary.json")
	if err != nil {
		return nil, fmt.Errorf("read summary.json: %w", err)
	}

	m := engine.Metrics{
		"raw_output": string(data),
	}

	var summary map[string]any
	if err := json.Unmarshal(data, &summary); err != nil {
		return nil, fmt.Errorf("unmarshal k6 summary.json: %w", err)
	}

	structured := extractMetrics(summary)
	m[engine.StructuredKey] = structured

	return m, nil
}

// extractMetrics processes the k6 --new-machine-readable-summary JSON format.
// Top-level keys: metadata, config, results.metrics (array of metric objects).
func extractMetrics(summary map[string]any) engine.StructuredMetrics {
	var sm engine.StructuredMetrics
	sm.InfoLabels = make(map[string]string)

	if metadata, ok := summary["metadata"].(map[string]any); ok {
		if v, ok := metadata["k6_version"].(string); ok {
			sm.InfoLabels["k6_version"] = v
		}
		if v, ok := metadata["generated_at"].(string); ok {
			sm.InfoLabels["generated_at"] = v
		}
	}

	if config, ok := summary["config"].(map[string]any); ok {
		if v, ok := config["duration"].(float64); ok {
			sm.InfoLabels["duration"] = fmt.Sprintf("%v", v)
		}
		if v, ok := config["execution"].(string); ok {
			sm.InfoLabels["execution"] = v
		}
	}

	results, ok := summary["results"].(map[string]any)
	if !ok {
		return sm
	}
	metricsRaw, ok := results["metrics"].([]any)
	if !ok {
		return sm
	}

	for _, raw := range metricsRaw {
		metric, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := metric["name"].(string)
		typ, _ := metric["type"].(string)
		values, ok := metric["values"].(map[string]any)
		if name == "" || typ == "" || !ok {
			continue
		}
		labels := map[string]string{"type": typ}
		cleanName := name
		if strings.HasPrefix(name, "http_req_duration") {
			labels["expected_response"] = "null"
			if i := strings.IndexByte(name, '{'); i != -1 {
				cleanName = name[:i]
				inner := name[i+1 : len(name)-1] // e.g. "expected_response:true"
				if after, ok := strings.CutPrefix(inner, "expected_response:"); ok {
					if after == "true" || after == "false" {
						labels["expected_response"] = after
					}
				}
			}
		}
		processMetricValues(cleanName, typ, values, labels, &sm)
	}

	return sm
}

// processMetricValues emits MetricPoints for a single k6 metric.
func processMetricValues(name, typ string, values map[string]any, baseLabels map[string]string, sm *engine.StructuredMetrics) {
	family := mapK6MetricName(name)

	switch typ {
	case "counter":
		if v, ok := values["count"].(float64); ok {
			sm.Points = append(sm.Points, engine.MetricPoint{Family: family, Labels: baseLabels, Value: v})
		}
	case "gauge":
		if v, ok := values["value"].(float64); ok {
			sm.Points = append(sm.Points, engine.MetricPoint{Family: family, Labels: baseLabels, Value: v})
		}
	case "trend", "rate":
		stats := make([]string, 0, len(values))
		for k := range values {
			stats = append(stats, k)
		}
		sort.Strings(stats)
		for _, stat := range stats {
			v := values[stat]
			if v == nil {
				continue
			}
			val, ok := v.(float64)
			if !ok {
				continue
			}
			l := make(map[string]string, len(baseLabels)+1)
			for k, v := range baseLabels {
				l[k] = v
			}
			l["stat"] = stat
			sm.Points = append(sm.Points, engine.MetricPoint{Family: family, Labels: l, Value: val})
		}
	}
}

// mapK6MetricName converts k6 metric names to Prometheus-style metric family names.
func mapK6MetricName(k6Name string) string {
	return "k6_" + k6Name
}

func execRun(ctx context.Context, out io.Writer, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var buf bytes.Buffer
	w := io.Writer(&buf)
	if out != nil && out != io.Discard {
		fmt.Fprintf(out, "$ %s %s\n", name, strings.Join(args, " "))
		w = io.MultiWriter(out, &buf)
	}
	cmd.Stdout = w
	cmd.Stderr = w
	err := cmd.Run()
	return buf.Bytes(), err
}
