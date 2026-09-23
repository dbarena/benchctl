package stdout

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dbarena/benchctl/internal/engine"
)

func collect(t *testing.T, metrics engine.Metrics) string {
	t.Helper()
	var buf bytes.Buffer
	c := newWithWriter(&buf)
	if err := c.Collect(context.Background(), nil, metrics); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return buf.String()
}

// collectWithCfg runs Collect with a custom config map and returns stdout output.
func collectWithCfg(t *testing.T, cfg map[string]any, metrics engine.Metrics) string {
	t.Helper()
	var buf bytes.Buffer
	c := newWithWriter(&buf)
	if err := c.Collect(context.Background(), cfg, metrics); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return buf.String()
}

func TestCollect_Header(t *testing.T) {
	out := collect(t, engine.Metrics{})
	if !strings.Contains(out, "=== Results ===") {
		t.Errorf("output missing header: %q", out)
	}
}

func TestCollect_PrintsMetric(t *testing.T) {
	out := collect(t, engine.Metrics{"tpm": float64(258.1)})
	if !strings.Contains(out, "tpm") {
		t.Errorf("output missing tpm: %q", out)
	}
	if !strings.Contains(out, "258.1") {
		t.Errorf("output missing value 258.1: %q", out)
	}
}

func TestCollect_ExcludesRawOutput(t *testing.T) {
	out := collect(t, engine.Metrics{
		"tpm":        float64(100),
		"raw_output": "verbose go-tpc output here",
	})
	if strings.Contains(out, "raw_output") {
		t.Errorf("output should not contain raw_output key: %q", out)
	}
	if strings.Contains(out, "verbose go-tpc output") {
		t.Errorf("output should not contain raw_output value: %q", out)
	}
}

func TestCollect_SortedKeys(t *testing.T) {
	out := collect(t, engine.Metrics{
		"tpm":           float64(258.1),
		"NEW_ORDER.TPM": float64(258.1),
		"DELIVERY.TPM":  float64(25.6),
	})
	lines := strings.Split(strings.TrimSpace(out), "\n")
	// Find lines containing metric keys (skip header)
	var keys []string
	for _, l := range lines[1:] {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		keys = append(keys, strings.Fields(l)[0])
	}
	for i := 1; i < len(keys); i++ {
		if keys[i] < keys[i-1] {
			t.Errorf("keys not sorted at position %d: %q < %q\nfull output: %s", i, keys[i], keys[i-1], out)
		}
	}
}

func TestCollect_FloatFormatting(t *testing.T) {
	cases := []struct {
		val  float64
		want string
	}{
		{258.1, "258.1"},
		{0, "0"},
		{1290, "1290"},
		{12.456, "12.456"},
	}
	for _, tc := range cases {
		out := collect(t, engine.Metrics{"x": tc.val})
		if !strings.Contains(out, tc.want) {
			t.Errorf("value %v: want %q in output, got: %q", tc.val, tc.want, out)
		}
	}
}

func TestCollect_EmptyMetrics(t *testing.T) {
	out := collect(t, engine.Metrics{})
	// Should still print the header and nothing else.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 {
		t.Errorf("expected only header line, got %d lines: %q", len(lines), out)
	}
}

func TestCollect_OnlyRawOutput(t *testing.T) {
	// raw_output alone should produce only the header.
	out := collect(t, engine.Metrics{"raw_output": "some output"})
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 {
		t.Errorf("expected only header line, got %d lines: %q", len(lines), out)
	}
}

func TestCollect_StructuredMetrics(t *testing.T) {
	sm := engine.StructuredMetrics{
		Points: []engine.MetricPoint{
			{Family: "k6_http_reqs", Labels: map[string]string{"type": "counter"}, Value: 486},
			{Family: "k6_http_req_duration", Labels: map[string]string{"type": "trend", "stat": "avg"}, Value: 178.86424},
			{Family: "k6_http_req_duration", Labels: map[string]string{"type": "trend", "stat": "p95"}, Value: 201.76},
			{Family: "k6_vus", Labels: map[string]string{"type": "gauge"}, Value: 10},
		},
		InfoLabels: map[string]string{"k6_version": "2.0.0"},
	}
	out := collect(t, engine.Metrics{engine.StructuredKey: sm})

	if strings.Contains(out, "_structured") {
		t.Errorf("output should not contain raw _structured key: %q", out)
	}
	for _, want := range []string{
		"k6_http_reqs", "486",
		"k6_http_req_duration", "avg=178.86", "p95=201.76",
		"k6_vus", "10",
		"k6_version: 2.0.0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestCollect_StructuredMetrics_MultiGroup(t *testing.T) {
	// TPCC-style: multiple groups per family, quantile as stat dimension.
	sm := engine.StructuredMetrics{
		Points: []engine.MetricPoint{
			{Family: "tpcc_tpm", Labels: map[string]string{"transaction": "NEW_ORDER", "status": "ok"}, Value: 258.1},
			{Family: "tpcc_tpm", Labels: map[string]string{"transaction": "DELIVERY", "status": "ok"}, Value: 25.6},
			{Family: "tpcc_latency_ms", Labels: map[string]string{"transaction": "NEW_ORDER", "status": "ok", "quantile": "p50"}, Value: 8},
			{Family: "tpcc_latency_ms", Labels: map[string]string{"transaction": "NEW_ORDER", "status": "ok", "quantile": "p99"}, Value: 64},
		},
		InfoLabels: map[string]string{"pg_version": "PostgreSQL 17"},
	}
	out := collect(t, engine.Metrics{engine.StructuredKey: sm})

	for _, want := range []string{
		"tpcc_tpm",
		"258.1",
		"25.6",
		"tpcc_latency_ms",
		"p50=8",
		"p99=64",
		"pg_version: PostgreSQL 17",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// Family name appears as a header (no value on the same token).
	if !strings.Contains(out, "tpcc_tpm\n") {
		t.Errorf("expected tpcc_tpm as a standalone header line:\n%s", out)
	}
	// Sub-rows carry the group-key labels.
	if !strings.Contains(out, "{status=ok,transaction=NEW_ORDER}") {
		t.Errorf("missing sub-row group key:\n%s", out)
	}
}

func TestCollect_ReturnsNilError(t *testing.T) {
	var buf bytes.Buffer
	c := newWithWriter(&buf)
	err := c.Collect(context.Background(), map[string]any{"unused": "cfg"}, engine.Metrics{"tpm": float64(1)})
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
}

func TestCollect_FileDisabledByDefault(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })

	collectWithCfg(t, map[string]any{"file_enabled": false, "iteration": 1, "labels": map[string]any{"benchmark": "mybench"}}, engine.Metrics{"tpm": float64(100)})

	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("expected no files when file_enabled=false, got %d", len(entries))
	}
}

func TestCollect_FileWritesJSON(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })

	cfg := map[string]any{
		"file_enabled": true,
		"iteration":    2,
		"labels":       map[string]any{"benchmark": "mybench"},
	}
	out := collectWithCfg(t, cfg, engine.Metrics{"tpm": float64(258.1), "warehouses": float64(10)})

	expectedFile := filepath.Join(dir, "results_mybench_2.json")
	if !strings.Contains(out, expectedFile) {
		t.Errorf("stdout missing JSON path %q:\n%s", expectedFile, out)
	}

	data, err := os.ReadFile(expectedFile)
	if err != nil {
		t.Fatalf("JSON file not created: %v", err)
	}
	var entries []map[string]any
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, data)
	}

	byName := make(map[string]map[string]any)
	for _, e := range entries {
		if name, ok := e["name"].(string); ok {
			byName[name] = e
		}
	}
	if v, ok := byName["tpm"]["value"].(float64); !ok || v != 258.1 {
		t.Errorf("tpm value: got %v, want 258.1", byName["tpm"]["value"])
	}
	if v, ok := byName["warehouses"]["value"].(float64); !ok || v != 10 {
		t.Errorf("warehouses value: got %v, want 10", byName["warehouses"]["value"])
	}
	if v, _ := byName["tpm"]["benchmark"].(string); v != "mybench" {
		t.Errorf("tpm benchmark: got %q, want %q", v, "mybench")
	}
	if v, _ := byName["tpm"]["iteration"].(float64); v != 2 {
		t.Errorf("tpm iteration: got %v, want 2", v)
	}
}

func TestCollect_FileExcludesRawOutput(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })

	cfg := map[string]any{
		"file_enabled": true,
		"iteration":    1,
		"labels":       map[string]any{"benchmark": "bench"},
	}
	collectWithCfg(t, cfg, engine.Metrics{
		"tpm":        float64(100),
		"raw_output": "verbose output",
	})

	data, err := os.ReadFile(filepath.Join(dir, "results_bench_1.json"))
	if err != nil {
		t.Fatalf("JSON file not created: %v", err)
	}
	if strings.Contains(string(data), "raw_output") {
		t.Errorf("JSON should not contain raw_output: %s", data)
	}
}

func TestCollect_FileFallsBackToScenario(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })

	cfg := map[string]any{
		"file_enabled": true,
		"iteration":    3,
		"scenario":     "myscenario",
		// no "labels" key
	}
	collectWithCfg(t, cfg, engine.Metrics{"x": float64(1)})

	if _, err := os.Stat(filepath.Join(dir, "results_myscenario_3.json")); err != nil {
		t.Errorf("expected results_myscenario_3.json, stat failed: %v", err)
	}
}

func TestCollect_FileStructuredMetrics(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })

	sm := engine.StructuredMetrics{
		Points: []engine.MetricPoint{
			{Family: "k6_http_reqs", Labels: map[string]string{"type": "counter"}, Value: 486},
			{Family: "k6_http_req_duration", Labels: map[string]string{"type": "trend", "stat": "p95"}, Value: 201.76},
		},
	}
	cfg := map[string]any{
		"file_enabled": true,
		"iteration":    1,
		"labels":       map[string]any{"benchmark": "k6bench"},
	}
	collectWithCfg(t, cfg, engine.Metrics{engine.StructuredKey: sm})

	data, err := os.ReadFile(filepath.Join(dir, "results_k6bench_1.json"))
	if err != nil {
		t.Fatalf("JSON file not created: %v", err)
	}
	var entries []map[string]any
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, data)
	}
	var foundReqs, foundDuration bool
	for _, e := range entries {
		switch e["name"] {
		case "k6_http_reqs":
			foundReqs = true
			if e["type"] != "counter" {
				t.Errorf("k6_http_reqs: type=%v, want counter", e["type"])
			}
		case "k6_http_req_duration":
			if e["stat"] == "p95" {
				foundDuration = true
				if v, _ := e["value"].(float64); v != 201.76 {
					t.Errorf("k6_http_req_duration p95 value=%v, want 201.76", e["value"])
				}
			}
		}
	}
	if !foundReqs {
		t.Errorf("JSON missing k6_http_reqs entry: %s", data)
	}
	if !foundDuration {
		t.Errorf("JSON missing k6_http_req_duration p95 entry: %s", data)
	}
}

func TestCollect_FileFixtureLabelsInEntries(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })

	cfg := map[string]any{
		"file_enabled": true,
		"iteration":    1,
		"labels": map[string]any{
			"benchmark":     "tpcc",
			"scenario":      "mysc",
			"fixture_users": "4",
		},
	}
	sm := engine.StructuredMetrics{
		Points: []engine.MetricPoint{
			{Family: "tpcc_latency_ms", Labels: map[string]string{"quantile": "p50"}, Value: 100},
		},
	}
	collectWithCfg(t, cfg, engine.Metrics{"tpm": float64(200), engine.StructuredKey: sm})

	data, err := os.ReadFile(filepath.Join(dir, "results_tpcc_1_4.json"))
	if err != nil {
		t.Fatalf("JSON file not created: %v", err)
	}
	var entries []map[string]any
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, data)
	}
	for _, e := range entries {
		if v, _ := e["fixture_users"].(string); v != "4" {
			t.Errorf("entry %q: fixture_users=%q, want \"4\"", e["name"], v)
		}
		if v, _ := e["scenario"].(string); v != "mysc" {
			t.Errorf("entry %q: scenario=%q, want \"mysc\"", e["name"], v)
		}
		// benchmark must not be overwritten by cfg label (explicit key wins)
		if v, _ := e["benchmark"].(string); v != "tpcc" {
			t.Errorf("entry %q: benchmark=%q, want \"tpcc\"", e["name"], v)
		}
	}
	// Structured metric point labels still win over cfg labels.
	for _, e := range entries {
		if e["name"] == "tpcc_latency_ms" {
			if v, _ := e["quantile"].(string); v != "p50" {
				t.Errorf("structured entry: quantile=%q, want p50", v)
			}
		}
	}
}

func TestCollect_FileNameSanitization(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })

	cfg := map[string]any{
		"file_enabled": true,
		"iteration":    1,
		"labels":       map[string]any{"benchmark": "my bench/test"},
	}
	collectWithCfg(t, cfg, engine.Metrics{"x": float64(1)})

	if _, err := os.Stat(filepath.Join(dir, "results_my_bench_test_1.json")); err != nil {
		t.Errorf("expected sanitized filename results_my_bench_test_1.json: %v", err)
	}
}

func TestCollect_RawSamplesCSVExcludedFromFlatDisplay(t *testing.T) {
	const csvContent = "t_seconds,transaction,status,tpm\n1.0,NEW_ORDER,ok,600\n"
	out := collect(t, engine.Metrics{engine.RawSamplesCSVKey("run"): csvContent, "tpm": float64(100)})
	if strings.Contains(out, csvContent) {
		t.Errorf("raw_samples_csv leaked into the flat key-value display:\n%s", out)
	}
}

func TestCollect_RawSamplesCSVExcludedFromJSONEntries(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })

	cfg := map[string]any{
		"file_enabled": true,
		"iteration":    1,
		"labels":       map[string]any{"benchmark": "mybench"},
	}
	collectWithCfg(t, cfg, engine.Metrics{engine.RawSamplesCSVKey("run"): "t_seconds,transaction,status,tpm\n1.0,NEW_ORDER,ok,600\n", "tpm": float64(100)})

	data, err := os.ReadFile(filepath.Join(dir, "results_mybench_1.json"))
	if err != nil {
		t.Fatalf("read results json: %v", err)
	}
	var entries []map[string]any
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("unmarshal results json: %v", err)
	}
	for _, e := range entries {
		if name, _ := e["name"].(string); strings.HasPrefix(name, "raw_samples_csv") {
			t.Errorf("raw_samples_csv should not appear as a results.json entry: %+v", e)
		}
	}
}

func TestCollect_WritesRawSamplesCSVWhenPresent(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })

	const csvContent = "t_seconds,transaction,status,tpm\n1.0,NEW_ORDER,ok,600\n"
	cfg := map[string]any{
		"file_enabled": true,
		"iteration":    3,
		"labels":       map[string]any{"benchmark": "mybench", "fixture_threads": "12"},
	}
	out := collectWithCfg(t, cfg, engine.Metrics{engine.RawSamplesCSVKey("run"): csvContent})

	expectedFile := filepath.Join(dir, "raw_samples_mybench_3_12_run.csv")
	if !strings.Contains(out, expectedFile) {
		t.Errorf("stdout missing raw samples path %q:\n%s", expectedFile, out)
	}

	data, err := os.ReadFile(expectedFile)
	if err != nil {
		t.Fatalf("raw samples file not created: %v", err)
	}
	if string(data) != csvContent {
		t.Errorf("raw samples file content = %q, want %q", data, csvContent)
	}

	// Written verbatim, not run through the JSON entries path.
	if _, err := os.Stat(filepath.Join(dir, "results_mybench_3_12.json")); err != nil {
		t.Errorf("results_*.json should still be written alongside it: %v", err)
	}
}

// TestCollect_WritesSeparateRawSamplesCSVPerStep is the multi-step case this
// naming exists for: a warm-up step and a benchmark step, each tagged with
// its own engine.RawSamplesCSVKey, land in two distinct files rather than
// one overwriting or being combined with the other.
func TestCollect_WritesSeparateRawSamplesCSVPerStep(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })

	const warmupCSV = "t_seconds,transaction,status,tpm\n1.0,NEW_ORDER,ok,500\n"
	const benchmarkCSV = "t_seconds,transaction,status,tpm\n1.0,NEW_ORDER,ok,600\n"
	cfg := map[string]any{
		"file_enabled": true,
		"iteration":    1,
		"labels":       map[string]any{"benchmark": "mybench"},
	}
	collectWithCfg(t, cfg, engine.Metrics{
		engine.RawSamplesCSVKey("warm-up"):   warmupCSV,
		engine.RawSamplesCSVKey("benchmark"): benchmarkCSV,
	})

	warmupData, err := os.ReadFile(filepath.Join(dir, "raw_samples_mybench_1_warm-up.csv"))
	if err != nil {
		t.Fatalf("warm-up raw samples file not created: %v", err)
	}
	if string(warmupData) != warmupCSV {
		t.Errorf("warm-up raw samples file content = %q, want %q", warmupData, warmupCSV)
	}

	benchmarkData, err := os.ReadFile(filepath.Join(dir, "raw_samples_mybench_1_benchmark.csv"))
	if err != nil {
		t.Fatalf("benchmark raw samples file not created: %v", err)
	}
	if string(benchmarkData) != benchmarkCSV {
		t.Errorf("benchmark raw samples file content = %q, want %q", benchmarkData, benchmarkCSV)
	}
}

func TestCollect_NoRawSamplesFileWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })

	cfg := map[string]any{
		"file_enabled": true,
		"iteration":    1,
		"labels":       map[string]any{"benchmark": "mybench"},
	}
	collectWithCfg(t, cfg, engine.Metrics{"tpm": float64(100)})

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "raw_samples_") {
			t.Errorf("unexpected raw samples file %q when metrics had none", e.Name())
		}
	}
}

func TestCollect_NoRawSamplesFileWhenFileDisabled(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })

	cfg := map[string]any{
		"file_enabled": false,
		"iteration":    1,
		"labels":       map[string]any{"benchmark": "mybench"},
	}
	collectWithCfg(t, cfg, engine.Metrics{engine.RawSamplesCSVKey("run"): "t_seconds,transaction,status,tpm\n"})

	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("expected no files when file_enabled=false, got %d", len(entries))
	}
}
