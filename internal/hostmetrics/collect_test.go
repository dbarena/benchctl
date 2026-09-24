package hostmetrics

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dbarena/benchctl/internal/engine"
)

// jsonCounterEvent mirrors rawEvent's JSON shape but with a string
// timestamp, so tests can build fixture lines without fighting Go's
// anonymous-struct literal syntax for rawEvent.Counter.
type jsonCounterEvent struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	Tags      map[string]string `json:"tags"`
	Timestamp string            `json:"timestamp"`
	Counter   struct {
		Value float64 `json:"value"`
	} `json:"counter"`
}

func writeTempLog(t *testing.T, lines []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "metrics.log")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write temp log: %v", err)
	}
	return path
}

func marshalEventLine(t *testing.T, name, ts string, tags map[string]string, value float64) string {
	t.Helper()
	ev := jsonCounterEvent{Name: name, Namespace: "host", Tags: tags, Timestamp: ts}
	ev.Counter.Value = value
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return string(b)
}

func TestCollect_MissingFileReturnsNilNil(t *testing.T) {
	var c Cursor
	pts, err := c.Collect(filepath.Join(t.TempDir(), "does-not-exist.log"))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if pts != nil {
		t.Errorf("Collect(missing) = %v, want nil", pts)
	}
}

func TestCollect_EndToEnd(t *testing.T) {
	const t0, t1, t2 = "2026-01-01T00:00:00Z", "2026-01-01T00:00:02Z", "2026-01-01T00:00:04Z"

	var lines []string
	add := func(name, ts string, tags map[string]string, value float64) {
		lines = append(lines, marshalEventLine(t, name, ts, tags, value))
	}
	// Single core: [t0,t1] 2s elapsed, idle+1/user+1 -> 50% util.
	// [t1,t2] 2s elapsed, idle+0/user+2 -> 100% util.
	add(metricCPUSecondsTotal, t0, map[string]string{"cpu": "0", "mode": "idle"}, 0)
	add(metricCPUSecondsTotal, t0, map[string]string{"cpu": "0", "mode": "user"}, 0)
	add(metricCPUSecondsTotal, t1, map[string]string{"cpu": "0", "mode": "idle"}, 1)
	add(metricCPUSecondsTotal, t1, map[string]string{"cpu": "0", "mode": "user"}, 1)
	add(metricCPUSecondsTotal, t2, map[string]string{"cpu": "0", "mode": "idle"}, 1)
	add(metricCPUSecondsTotal, t2, map[string]string{"cpu": "0", "mode": "user"}, 3)
	// Receive-only: 100 bytes over 2s = 50 B/s. No transmit samples at all.
	add(metricNetworkReceiveBytesTotal, t0, map[string]string{"device": "eth0"}, 0)
	add(metricNetworkReceiveBytesTotal, t1, map[string]string{"device": "eth0"}, 100)

	var c Cursor
	points, err := c.Collect(writeTempLog(t, lines))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	byFamily := map[string][]engine.MetricPoint{}
	for _, p := range points {
		byFamily[p.Family] = append(byFamily[p.Family], p)
	}

	cpuPts := byFamily["driver_cpu_utilization"]
	if len(cpuPts) != 3 {
		t.Fatalf("driver_cpu_utilization points = %d, want 3", len(cpuPts))
	}
	for _, p := range cpuPts {
		if p.Labels["quantile"] == "0.99" && p.Value != 1.0 {
			t.Errorf("p99 cpu utilization = %v, want 1.0", p.Value)
		}
	}

	netPts := byFamily["driver_network_throughput_bytes_per_sec"]
	if len(netPts) != 3 {
		t.Fatalf("driver_network_throughput_bytes_per_sec points = %d, want 3 (receive only)", len(netPts))
	}
	for _, p := range netPts {
		if p.Labels["direction"] != "receive" {
			t.Errorf("unexpected direction label %q", p.Labels["direction"])
		}
		if p.Value != 50 {
			t.Errorf("network throughput = %v, want 50", p.Value)
		}
	}
}

func TestAppendTo_AppendsToExistingStructuredMetrics(t *testing.T) {
	const t0, t1 = "2026-01-01T00:00:00Z", "2026-01-01T00:00:02Z"
	lines := []string{
		marshalEventLine(t, metricCPUSecondsTotal, t0, map[string]string{"cpu": "0", "mode": "idle"}, 0),
		marshalEventLine(t, metricCPUSecondsTotal, t0, map[string]string{"cpu": "0", "mode": "user"}, 0),
		marshalEventLine(t, metricCPUSecondsTotal, t1, map[string]string{"cpu": "0", "mode": "idle"}, 1),
		marshalEventLine(t, metricCPUSecondsTotal, t1, map[string]string{"cpu": "0", "mode": "user"}, 1),
	}
	path := writeTempLog(t, lines)

	existing := engine.MetricPoint{Family: "tpcc_latency_ms", Labels: map[string]string{"quantile": "0.99"}, Value: 42}
	metrics := engine.Metrics{
		engine.StructuredKey: engine.StructuredMetrics{Points: []engine.MetricPoint{existing}},
	}

	var c Cursor
	var warnBuf bytes.Buffer
	got := c.AppendTo(metrics, path, &warnBuf)

	sm, ok := got[engine.StructuredKey].(engine.StructuredMetrics)
	if !ok {
		t.Fatalf("StructuredKey is not StructuredMetrics: %T", got[engine.StructuredKey])
	}
	if len(sm.Points) <= 1 {
		t.Fatalf("expected new points appended on top of the existing one, got %d points", len(sm.Points))
	}
	if !reflect.DeepEqual(sm.Points[0], existing) {
		t.Errorf("existing point was not preserved: %+v", sm.Points[0])
	}
	if warnBuf.Len() != 0 {
		t.Errorf("unexpected warning: %s", warnBuf.String())
	}
}

func TestAppendTo_MissingFileLeavesMetricsUnchanged(t *testing.T) {
	metrics := engine.Metrics{"foo": "bar"}
	var c Cursor
	var warnBuf bytes.Buffer
	got := c.AppendTo(metrics, filepath.Join(t.TempDir(), "missing.log"), &warnBuf)
	if len(got) != 1 || got["foo"] != "bar" {
		t.Errorf("metrics changed unexpectedly: %+v", got)
	}
	if warnBuf.Len() != 0 {
		t.Errorf("unexpected warning: %s", warnBuf.String())
	}
}

func TestAppendTo_ErrorLogsWarningAndLeavesMetricsUnchanged(t *testing.T) {
	dirPath := t.TempDir() // a directory: os.Open succeeds, Read fails, surfacing a parse error.
	metrics := engine.Metrics{"foo": "bar"}
	var c Cursor
	var warnBuf bytes.Buffer
	got := c.AppendTo(metrics, dirPath, &warnBuf)
	if len(got) != 1 || got["foo"] != "bar" {
		t.Errorf("metrics changed unexpectedly: %+v", got)
	}
	if warnBuf.Len() == 0 {
		t.Error("expected a warning to be logged")
	}
}

// TestCursor_SecondCallOnlySeesSamplesSinceThePrevious is the crux of why
// Cursor exists: Driver.Collect fires once per benchmark entry per fixture
// per iteration, all against the same ever-growing Vector log. Without a
// watermark, a second fixture's numbers would be diluted by the first
// fixture's activity too, instead of reflecting only its own window.
func TestCursor_SecondCallOnlySeesSamplesSinceThePrevious(t *testing.T) {
	const t0, t1, t2, t3 = "2026-01-01T00:00:00Z", "2026-01-01T00:00:02Z", "2026-01-01T00:00:04Z", "2026-01-01T00:00:06Z"

	// Fixture 1's window: [t0,t1], idle+1/user+1 over 2s -> 50% util.
	fixture1 := []string{
		marshalEventLine(t, metricCPUSecondsTotal, t0, map[string]string{"cpu": "0", "mode": "idle"}, 0),
		marshalEventLine(t, metricCPUSecondsTotal, t0, map[string]string{"cpu": "0", "mode": "user"}, 0),
		marshalEventLine(t, metricCPUSecondsTotal, t1, map[string]string{"cpu": "0", "mode": "idle"}, 1),
		marshalEventLine(t, metricCPUSecondsTotal, t1, map[string]string{"cpu": "0", "mode": "user"}, 1),
	}
	// Fixture 2's window: [t2,t3], idle+0/user+2 over 2s -> 100% util. Appended
	// to the same file, as Vector would do across fixtures within one run.
	fixture2 := []string{
		marshalEventLine(t, metricCPUSecondsTotal, t2, map[string]string{"cpu": "0", "mode": "idle"}, 1),
		marshalEventLine(t, metricCPUSecondsTotal, t2, map[string]string{"cpu": "0", "mode": "user"}, 1),
		marshalEventLine(t, metricCPUSecondsTotal, t3, map[string]string{"cpu": "0", "mode": "idle"}, 1),
		marshalEventLine(t, metricCPUSecondsTotal, t3, map[string]string{"cpu": "0", "mode": "user"}, 3),
	}

	path := writeTempLog(t, fixture1)
	var c Cursor

	pts1, err := c.Collect(path)
	if err != nil {
		t.Fatalf("Collect (fixture 1): %v", err)
	}
	if got := utilizationP99(pts1); got != 0.5 {
		t.Fatalf("fixture 1 p99 utilization = %v, want 0.5", got)
	}

	appendToTempLog(t, path, fixture2)
	pts2, err := c.Collect(path)
	if err != nil {
		t.Fatalf("Collect (fixture 2): %v", err)
	}
	// Without the watermark, this would see all of fixture1+fixture2's
	// samples and report a p99 blended across both windows instead of 1.0.
	if got := utilizationP99(pts2); got != 1.0 {
		t.Fatalf("fixture 2 p99 utilization = %v, want 1.0 (isolated from fixture 1)", got)
	}
}

func utilizationP99(points []engine.MetricPoint) float64 {
	for _, p := range points {
		if p.Family == "driver_cpu_utilization" && p.Labels["quantile"] == "0.99" {
			return p.Value
		}
	}
	return math.NaN()
}

func appendToTempLog(t *testing.T, path string, lines []string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open temp log for append: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		t.Fatalf("append temp log: %v", err)
	}
}
