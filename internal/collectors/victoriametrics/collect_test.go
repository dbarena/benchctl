package victoriametrics_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/dbarena/benchctl/internal/collectors/victoriametrics"
	"github.com/dbarena/benchctl/internal/config"
	"github.com/dbarena/benchctl/internal/engine"
)

// capturePush stands in for VictoriaMetrics, recording the import request so
// tests can assert on the Prometheus text actually sent.
//
// This replaces a test that pushed to a real VictoriaMetrics and asserted
// only that the call returned no error. That proved little: the import
// endpoint answers 204 on accept and drops malformed lines silently, so the
// body was never checked. The e2e workflow already pushes real go-tpc results
// to a real instance and reads them back with `benchctl results`, which is
// the stronger end of that test; what was missing is an assertion on the
// serialised lines, which needs no Docker.
type capturePush struct {
	server      *httptest.Server
	body        string
	contentType string
	path        string
	status      int
}

func newCapturePush(t *testing.T) *capturePush {
	t.Helper()
	c := &capturePush{status: http.StatusNoContent}
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		c.body = string(raw)
		c.contentType = r.Header.Get("Content-Type")
		c.path = r.URL.Path
		w.WriteHeader(c.status)
	}))
	t.Cleanup(c.server.Close)
	return c
}

func (c *capturePush) collector() *victoriametrics.Collector {
	return victoriametrics.New(&config.Config{
		Metrics: config.MetricsConfig{Endpoint: c.server.URL},
	})
}

// valueAndTimestamp reads a Prometheus line's trailing `value timestamp`.
// Parsed from the right on purpose: a label value can contain spaces, as
// pg_version does, so splitting the whole line does not work.
func valueAndTimestamp(t *testing.T, line string) (value, timestamp string) {
	t.Helper()
	fields := strings.Fields(line)
	if len(fields) < 3 {
		t.Fatalf("line has no value and timestamp: %s", line)
	}
	return fields[len(fields)-2], fields[len(fields)-1]
}

// lineFor returns the pushed line whose label set contains every fragment.
func (c *capturePush) lineFor(t *testing.T, fragments ...string) string {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(c.body), "\n") {
		matched := true
		for _, f := range fragments {
			if !strings.Contains(line, f) {
				matched = false
				break
			}
		}
		if matched {
			return line
		}
	}
	t.Fatalf("no pushed line matching %v\nbody:\n%s", fragments, c.body)
	return ""
}

func testMetrics() engine.Metrics {
	return engine.Metrics{engine.StructuredKey: engine.StructuredMetrics{
		InfoLabels: map[string]string{"pg_version": "PostgreSQL 17.9 (Debian 17.9-1.pgdg13+1)"},
		Points: []engine.MetricPoint{
			{Family: "tpcc_tpm", Labels: map[string]string{"transaction": "NEW_ORDER", "status": "ok"}, Value: 27065.8},
			// A p99 and a max quantile: the max label only appears in a run
			// that recorded one, so it is easy to leave uncovered.
			{Family: "tpcc_latency_ms", Labels: map[string]string{"transaction": "NEW_ORDER", "status": "ok", "quantile": "p99"}, Value: 5.8},
			{Family: "tpcc_latency_ms", Labels: map[string]string{"transaction": "NEW_ORDER", "status": "ok", "quantile": "max"}, Value: 104.9},
			// An error-status point. A clean short benchmark produces none, so
			// the e2e run does not exercise this path at all.
			{Family: "tpcc_tpm", Labels: map[string]string{"transaction": "DELIVERY", "status": "error"}, Value: 1},
			{Family: "tpcc_count", Labels: map[string]string{"transaction": "NEW_ORDER", "status": "ok"}, Value: 27053},
			{Family: "tpcc_duration_seconds", Labels: map[string]string{"transaction": "NEW_ORDER", "status": "ok"}, Value: 60},
		},
	}}
}

func testCfg() map[string]any {
	return map[string]any{
		"run_id":   "smoke-test-20260427-aabbcc",
		"scenario": "acme-tpcc-local",
		"labels":   map[string]any{"engine": "acme"},
	}
}

func TestCollect_PushesEveryPointAsPrometheusText(t *testing.T) {
	c := newCapturePush(t)

	if err := c.collector().Collect(context.Background(), testCfg(), testMetrics()); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if c.path != "/api/v1/import/prometheus" {
		t.Errorf("posted to %q", c.path)
	}
	if c.contentType != "text/plain" {
		t.Errorf("Content-Type = %q, want text/plain", c.contentType)
	}

	// Six points plus the info metric the run id triggers.
	lines := strings.Split(strings.TrimSpace(c.body), "\n")
	if len(lines) != 7 {
		t.Errorf("%d lines, want 7 (6 points and the info metric):\n%s", len(lines), c.body)
	}

	// Every line carries the join keys `benchctl results` queries by, and a
	// timestamp; without those the samples are unattributable.
	for _, line := range lines {
		for _, want := range []string{`run_id="smoke-test-20260427-aabbcc"`, `scenario="acme-tpcc-local"`, `engine="acme"`} {
			if !strings.Contains(line, want) {
				t.Errorf("line is missing %s: %s", want, line)
			}
		}
		value, ts := valueAndTimestamp(t, line)
		if value == "" || ts == "" {
			t.Errorf("line is not `name{labels} value timestamp`: %s", line)
		}
		if _, err := strconv.ParseInt(ts, 10, 64); err != nil {
			t.Errorf("trailing field is not a millisecond timestamp: %s", line)
		}
	}
}

// TestCollect_SerialisesTheErrorStatusPoint covers a label value a clean
// benchmark never emits, so the e2e run cannot cover it.
func TestCollect_SerialisesTheErrorStatusPoint(t *testing.T) {
	c := newCapturePush(t)
	if err := c.collector().Collect(context.Background(), testCfg(), testMetrics()); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	line := c.lineFor(t, `transaction="DELIVERY"`, `status="error"`)
	if !strings.HasPrefix(line, "tpcc_tpm{") {
		t.Errorf("wrong metric family: %s", line)
	}
	if value, _ := valueAndTimestamp(t, line); value != "1" {
		t.Errorf("value is %s, want 1: %s", value, line)
	}
}

// TestCollect_SerialisesTheMaxQuantile is the sibling gap: p99 shows up in
// every run, max only when one was recorded.
func TestCollect_SerialisesTheMaxQuantile(t *testing.T) {
	c := newCapturePush(t)
	if err := c.collector().Collect(context.Background(), testCfg(), testMetrics()); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	line := c.lineFor(t, "tpcc_latency_ms", `quantile="max"`)
	if value, _ := valueAndTimestamp(t, line); value != "104.9" {
		t.Errorf("value is %s, want 104.9: %s", value, line)
	}
	// The two quantiles have to be distinct series, or one overwrites the
	// other on ingest.
	p99 := c.lineFor(t, "tpcc_latency_ms", `quantile="p99"`)
	if p99 == line {
		t.Error("p99 and max collapsed into one line")
	}
}

// TestCollect_InfoMetricCarriesTheTextLabels checks the one line that exists
// to hold non-numeric metadata, since a Prometheus sample cannot.
func TestCollect_InfoMetricCarriesTheTextLabels(t *testing.T) {
	c := newCapturePush(t)
	if err := c.collector().Collect(context.Background(), testCfg(), testMetrics()); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	line := c.lineFor(t, "pg_version")
	if !strings.Contains(line, `pg_version="PostgreSQL 17.9 (Debian 17.9-1.pgdg13+1)"`) {
		t.Errorf("version label was mangled: %s", line)
	}
}

// TestCollect_RejectedPushIsReported guards the one thing a real
// VictoriaMetrics would have told us and a 204 never does.
func TestCollect_RejectedPushIsReported(t *testing.T) {
	c := newCapturePush(t)
	c.status = http.StatusBadRequest

	err := c.collector().Collect(context.Background(), testCfg(), testMetrics())
	if err == nil {
		t.Fatal("a rejected push reported success")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error does not carry the status: %v", err)
	}
}

// TestCollect_NoPointsStillRecordsTheRun documents what a step that measured
// nothing pushes: just the info metric, so the run is still attributable in
// VictoriaMetrics even though it produced no samples.
func TestCollect_NoPointsStillRecordsTheRun(t *testing.T) {
	c := newCapturePush(t)

	if err := c.collector().Collect(context.Background(), testCfg(), engine.Metrics{}); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(c.body), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "benchctl_run_info{") {
		t.Errorf("want only the info metric, got:\n%s", c.body)
	}
}

// TestCollect_NothingToRecordPushesNothing is the other half: with no points
// and no run id there is nothing to attribute, so no request is made at all.
func TestCollect_NothingToRecordPushesNothing(t *testing.T) {
	c := newCapturePush(t)

	if err := c.collector().Collect(context.Background(), map[string]any{}, engine.Metrics{}); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if c.body != "" {
		t.Errorf("pushed a body with nothing to record:\n%s", c.body)
	}
}
