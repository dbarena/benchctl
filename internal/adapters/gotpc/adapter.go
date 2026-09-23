// Package gotpc implements a WorkloadAdapter for the go-tpc benchmark tool.
// See https://github.com/supabase/go-tpc
package gotpc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dbarena/benchctl/internal/engine"
	"github.com/dbarena/benchctl/internal/schema"
)

// cmdRunner executes a command, tees combined output to out (if non-nil),
// and returns the captured bytes for error reporting.
type cmdRunner func(ctx context.Context, out io.Writer, name string, args ...string) ([]byte, error)

// toolEnsurer resolves the path to a required binary, failing if it's missing.
type toolEnsurer func(ctx context.Context, out io.Writer) (string, error)

// Adapter implements engine.WorkloadAdapter for go-tpc.
type Adapter struct {
	run     cmdRunner
	out     io.Writer
	ensurer toolEnsurer
}

// New returns an Adapter that streams go-tpc output to stdout.
func New() *Adapter {
	return &Adapter{run: execRun, out: os.Stdout, ensurer: ensureGoTPC}
}

// newWithRunner returns an Adapter with an injected runner and no output
// streaming, for testing. The ensurer is stubbed to return "go-tpc" directly
// so tests never attempt a download.
func newWithRunner(r cmdRunner) *Adapter {
	return &Adapter{
		run: r,
		out: io.Discard,
		ensurer: func(_ context.Context, _ io.Writer) (string, error) {
			return "go-tpc", nil
		},
	}
}

// isPrepare reports whether the step is go-tpc's data-load command, which
// creates the schema and populates it instead of measuring anything.
func isPrepare(step schema.SuiteStep) bool {
	return step.Command == "prepare"
}

// Run executes the step and returns parsed metrics. The go-tpc "prepare"
// subcommand does not measure anything thus returning empty Metrics.
//
// A measured step always passes go-tpc's --summary-file/--raw-samples-file
// (see https://github.com/supabase/go-tpc's pkg/measurement), pointed at
// adapter-owned scratch paths. The adapter reads the summary
// file and translates it directly into engine.MetricPoints. It also reads
// the raw-samples file back verbatim into metrics["raw_samples_csv"].
func (a *Adapter) Run(ctx context.Context, outputs engine.Outputs, step schema.SuiteStep) (engine.Metrics, error) {
	if isPrepare(step) {
		return a.prepare(ctx, outputs, step)
	}
	bin, err := a.ensurer(ctx, a.out)
	if err != nil {
		return nil, fmt.Errorf("go-tpc run: %w", err)
	}
	_, args, err := buildCmd(outputs, step)
	if err != nil {
		return nil, fmt.Errorf("go-tpc run: %w", err)
	}

	summaryFile := scratchPath("summary", "json")
	rawSamplesFile := scratchPath("raw-samples", "csv")
	args = append(args, "--summary-file="+summaryFile, "--raw-samples-file="+rawSamplesFile)

	out, err := a.run(ctx, a.out, bin, args...)
	if err != nil {
		return nil, fmt.Errorf("go-tpc run: %w\noutput: %s", err, out)
	}

	metrics, err := readSummaryFile(summaryFile)
	if err != nil {
		return nil, fmt.Errorf("go-tpc run: %w", err)
	}
	metrics["raw_output"] = string(out)
	removeScratchFile(a.out, summaryFile)

	data, err := os.ReadFile(rawSamplesFile)
	if err != nil {
		return nil, fmt.Errorf("go-tpc run: read raw samples file %s: %w", rawSamplesFile, err)
	}
	metrics["raw_samples_csv"] = string(data)
	removeScratchFile(a.out, rawSamplesFile)

	return metrics, nil
}

// prepare runs go-tpc's schema creation and data load. It writes no summary or
// raw-samples file, so there is nothing to read back and no metrics to return.
func (a *Adapter) prepare(ctx context.Context, outputs engine.Outputs, step schema.SuiteStep) (engine.Metrics, error) {
	bin, err := a.ensurer(ctx, a.out)
	if err != nil {
		return nil, fmt.Errorf("go-tpc prepare: %w", err)
	}
	_, args, err := buildCmd(outputs, step)
	if err != nil {
		return nil, fmt.Errorf("go-tpc prepare: %w", err)
	}
	out, err := a.run(ctx, a.out, bin, args...)
	if err != nil {
		return nil, fmt.Errorf("go-tpc prepare: %w\noutput: %s", err, out)
	}
	return engine.Metrics{}, nil
}

// VersionInfo runs "<go-tpc> version" and parses its "Release version:"
// line (the rest of its output, e.g. the maxprocs log line, is ignored).
// Best-effort: any failure to resolve or run the binary logs a warning to
// a.out and returns nil rather than failing the run.
func (a *Adapter) VersionInfo(ctx context.Context) map[string]string {
	bin, err := a.ensurer(ctx, a.out)
	if err != nil {
		fmt.Fprintf(a.out, "warning: go-tpc version: %v\n", err)
		return nil
	}
	out, err := a.run(ctx, io.Discard, bin, "version")
	if err != nil {
		fmt.Fprintf(a.out, "warning: go-tpc version: %v\n", err)
		return nil
	}

	info := make(map[string]string)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "Release version:"); ok {
			info["gotpc_version"] = strings.TrimSpace(v)
		}
	}
	return info
}

// scratchPath returns a fixed path under the OS temp dir for one of the
// adapter's own --summary-file/--raw-samples-file scratch files, scoped by
// PID in case two benchctl processes share a host. A fixed path is safe
// because every step, across iterations, fixtures, and benchmarks, runs
// strictly sequentially: each Run call reads and removes its scratch file
// before the next call reuses the path.
func scratchPath(name, ext string) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("benchctl-gotpc-%s-%d.%s", name, os.Getpid(), ext))
}

// removeScratchFile deletes a --summary-file/--raw-samples-file scratch path
// once its content has been read into metrics. Best-effort: a leftover
// scratch file isn't worth failing the run over, but is worth a note since a
// lingering one previously got mistaken for a real result: it matched the
// stdout collector's own raw_samples_*.csv naming.
func removeScratchFile(out io.Writer, path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(out, "warning: could not remove scratch file %s: %v\n", path, err)
	}
}

// reservedGoTPCArgs are step.Args keys the adapter generates itself for every
// invocation (see Run). buildCmd rejects a scenario that sets either one
// rather than handing go-tpc two conflicting --raw-samples-file flags.
var reservedGoTPCArgs = []string{"raw-samples-file", "summary-file"}

// buildCmd constructs the go-tpc invocation.
//
// step.Command is the tpcc subcommand ("prepare" or "run").
// Connection flags (-H/-P/-U/-p/-D) come from targetOutputs; all five params
// (host, port, user, password, db) are required. A missing value is an error
// rather than a silent fallback to a default.
// step.Args are appended as --key value pairs in sorted order.
func buildCmd(outputs engine.Outputs, step schema.SuiteStep) (name string, args []string, err error) {
	name = "go-tpc"
	if step.Command == "" {
		return name, nil, nil
	}

	for _, reserved := range reservedGoTPCArgs {
		if _, ok := step.Args[reserved]; ok {
			return name, nil, fmt.Errorf(
				"step %q: %q is managed internally by the go-tpc adapter and must not be set in scenario args",
				step.Name, reserved)
		}
	}

	required := []string{"host", "port", "user", "password", "db"}
	var missing []string
	for _, k := range required {
		if outputs[k] == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return name, nil, fmt.Errorf("target connection params missing: %v (declare them in the scenario's service vars)", missing)
	}
	sslmode := outputs["sslmode"]
	if sslmode == "" {
		sslmode = "disable"
	}
	connParams := "sslmode=" + sslmode
	if sslnegotiation := outputs["sslnegotiation"]; sslnegotiation != "" {
		connParams += "&sslnegotiation=" + sslnegotiation
	}

	args = append(args, "tpcc", step.Command)
	// go-tpc defaults to MySQL; --driver postgres selects the pq driver.
	args = append(args,
		"--driver", "postgres",
		"-H", outputs["host"],
		"-P", outputs["port"],
		"-U", outputs["user"],
		"-p", outputs["password"],
		"-D", outputs["db"],
		"--conn-params", connParams,
	)

	// Workload flags from step.Args, sorted for determinism.
	// Empty value emits a bare flag (e.g. no-check: "" → --no-check).
	// Non-empty value uses --key=value, which works for all cobra flag types.
	keys := make([]string, 0, len(step.Args))
	for k := range step.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v := step.Args[k]; v == "" {
			args = append(args, "--"+k)
		} else {
			args = append(args, "--"+k+"="+v)
		}
	}

	return name, args, nil
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
