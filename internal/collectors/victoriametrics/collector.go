// Package victoriametrics implements a CollectorProvider that ships benchmark
// metrics to a VictoriaMetrics instance via the Prometheus remote-write import API.
package victoriametrics

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dbarena/benchctl/internal/config"
	"github.com/dbarena/benchctl/internal/engine"
)

// Collector implements engine.CollectorProvider by writing metrics to VictoriaMetrics.
type Collector struct {
	cfg *config.Config
}

// New returns a new Collector reading its endpoint and credentials from cfg.
func New(cfg *config.Config) *Collector { return &Collector{cfg: cfg} }

// Preflight implements engine.CollectorPreflight. It checks that the endpoint
// is configured, well-formed, and reachable before the benchmark lifecycle
// starts.
func (c *Collector) Preflight(_ map[string]any) error {
	endpoint := c.cfg.Metrics.Endpoint
	if endpoint == "" {
		return fmt.Errorf("victoriametrics collector: endpoint required; set metrics.endpoint or BENCHCTL_METRICS_ENDPOINT")
	}

	u, err := url.Parse(endpoint)
	if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("victoriametrics collector: metrics.endpoint %q is not a valid absolute http(s) URL", endpoint)
	}

	username := c.cfg.Metrics.Username
	password := c.cfg.Metrics.Password
	if password != "" && username == "" {
		return fmt.Errorf("victoriametrics collector: metrics.password (BENCHCTL_METRICS_PASSWORD) also needs metrics.username (BENCHCTL_METRICS_USERNAME)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := newImportRequest(ctx, endpoint, c.cfg.Metrics.Token, username, password, "")
	if err != nil {
		return fmt.Errorf("victoriametrics collector: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("victoriametrics collector: endpoint unreachable (%s): %w", endpoint, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("victoriametrics collector: credentials rejected (status %d): check BENCHCTL_METRICS_TOKEN or BENCHCTL_METRICS_USERNAME/BENCHCTL_METRICS_PASSWORD", resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("victoriametrics collector: import endpoint not found (status 404): metrics.endpoint (%s) must be the VictoriaMetrics base URL", endpoint)
	case resp.StatusCode >= 500:
		return fmt.Errorf("victoriametrics collector: endpoint unhealthy: status %d", resp.StatusCode)
	default:
		// 2xx and any other 4xx (notably 400) all pass. An empty body writes
		// nothing, so a 400 here confirms that the request cleared
		// authentication and reached VictoriaMetrics' line parser.
		return nil
	}
}

// Collect parses the benchmark metrics, converts them to Prometheus text format,
// and POSTs them to the VictoriaMetrics import endpoint.
//
// Expected cfg keys:
//
//	run_id          (string, injected by runner) - attached as a label on every metric
//	scenario        (string, injected by runner) - attached as a label on every metric
//	labels          (map[string]any) - user-defined labels from the scenario YAML
//	effective_date  (string, optional) - metric timestamp override:
//	                  "auto"     → midnight UTC of today (nightly runs: one dot per day on trend charts)
//	                  "YYYYMMDD" → midnight UTC of that date (backfill for a missed nightly, e.g. "20260427")
//	                  ""         → time.Now() (default; local dev, preserves exact push time)
//
// The endpoint and credentials come from cfg.Metrics only. Auth precedence:
// token (Bearer) > username/password (Basic) > none.
func (c *Collector) Collect(ctx context.Context, cfg map[string]any, metrics engine.Metrics) error {
	endpoint := c.cfg.Metrics.Endpoint
	if endpoint == "" {
		return fmt.Errorf("victoriametrics collector: endpoint required; set metrics.endpoint or BENCHCTL_METRICS_ENDPOINT")
	}

	runID, _ := cfg["run_id"].(string)
	scenario, _ := cfg["scenario"].(string)
	tsMs, err := resolveTimestamp(cfg)
	if err != nil {
		return fmt.Errorf("victoriametrics collector: %w", err)
	}

	baseLabels := buildBaseLabels(runID, scenario, cfg)
	lines, infoLabels := toPrometheusLines(metrics, baseLabels, tsMs)
	if runID != "" {
		lines = append(lines, buildInfoMetric(baseLabels, infoLabels, tsMs))
	}
	if len(lines) == 0 {
		return nil
	}

	return push(ctx, endpoint, c.cfg.Metrics.Token, c.cfg.Metrics.Username, c.cfg.Metrics.Password, strings.Join(lines, "\n")+"\n")
}

// RunMetadata implements engine.CollectorMetadata. It returns the configured
// endpoint so the runner can persist it in State.Metadata.
func (c *Collector) RunMetadata(_ map[string]any) map[string]string {
	if c.cfg.Metrics.Endpoint == "" {
		return nil
	}
	return map[string]string{"endpoint": c.cfg.Metrics.Endpoint}
}

// resolveTimestamp returns the Unix millisecond timestamp to use for all metric
// lines in this push, based on the optional effective_date config key:
//
//   - "auto"     → midnight UTC of today
//   - "YYYYMMDD" → midnight UTC of that date (e.g. "20260427")
//   - ""         → time.Now() (exact push time; default for local dev)
func resolveTimestamp(cfg map[string]any) (int64, error) {
	ed, _ := cfg["effective_date"].(string)
	switch {
	case ed == "" || ed == "now":
		return time.Now().UnixMilli(), nil
	case ed == "auto":
		now := time.Now().UTC()
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).UnixMilli(), nil
	default:
		t, err := time.ParseInLocation("20060102", ed, time.UTC)
		if err != nil {
			return 0, fmt.Errorf("invalid effective_date %q: expected \"auto\" or \"YYYYMMDD\"", ed)
		}
		return t.UnixMilli(), nil
	}
}

// buildBaseLabels assembles the labels that are attached to every metric line:
// run_id, scenario, and any user-defined labels from collector.config.labels.
func buildBaseLabels(runID, scenario string, cfg map[string]any) map[string]string {
	lbls := make(map[string]string)
	if runID != "" {
		lbls["run_id"] = runID
	}
	if scenario != "" {
		lbls["scenario"] = scenario
	}
	if raw, ok := cfg["labels"].(map[string]any); ok {
		for k, v := range raw {
			lbls[k] = fmt.Sprintf("%v", v)
		}
	}
	return lbls
}

// newImportRequest builds a POST request to endpoint's Prometheus import API,
// applying the auth precedence shared by push and Preflight: Bearer token,
// else Basic auth with username/password, else no auth. Used by both push
// (the real write) and Preflight (a probe with an empty body), so the probe
// can never drift from the real write.
func newImportRequest(ctx context.Context, endpoint, token, username, password, body string) (*http.Request, error) {
	u := strings.TrimRight(endpoint, "/") + "/api/v1/import/prometheus"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewBufferString(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "text/plain")
	switch {
	case token != "":
		req.Header.Set("Authorization", "Bearer "+token)
	case username != "":
		req.SetBasicAuth(username, password)
	}
	return req, nil
}

// push POSTs body to the VictoriaMetrics Prometheus import endpoint.
func push(ctx context.Context, endpoint, token, username, password, body string) error {
	req, err := newImportRequest(ctx, endpoint, token, username, password, body)
	if err != nil {
		return fmt.Errorf("victoriametrics push: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("victoriametrics push: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("victoriametrics push: status %d: %s", resp.StatusCode, msg)
	}
	return nil
}
