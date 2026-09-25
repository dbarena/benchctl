package gotpc

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dbarena/benchctl/internal/schema"
)

const sampleSummaryJSON = `{
  "transactions": [
    {"transaction": "NEW_ORDER", "status": "ok", "count": 1290, "tpm": 258.1, "takes_s": 299.9, "avg_latency_ms": 12.4, "p50_latency_ms": 8, "p90_latency_ms": 24, "p95_latency_ms": 32, "p99_latency_ms": 64, "p99_9_latency_ms": 128, "max_latency_ms": 192},
    {"transaction": "NEW_ORDER", "status": "error", "count": 1, "tpm": 0.2, "takes_s": 299.9, "avg_latency_ms": 5, "p50_latency_ms": 5, "p90_latency_ms": 5, "p95_latency_ms": 5, "p99_latency_ms": 5, "p99_9_latency_ms": 5, "max_latency_ms": 5},
    {"transaction": "DELIVERY", "status": "ok", "count": 128, "tpm": 25.6, "takes_s": 299.9, "avg_latency_ms": 35.2, "p50_latency_ms": 32, "p90_latency_ms": 64, "p95_latency_ms": 64, "p99_latency_ms": 96, "p99_9_latency_ms": 96, "max_latency_ms": 128}
  ],
  "tpm": 258.1,
  "tpm_total": 283.7,
  "efficiency_pct": 12.3
}`

func TestReadSummaryFile_TranslatesFieldByField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "summary.json")
	if err := os.WriteFile(path, []byte(sampleSummaryJSON), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	metrics, err := readSummaryFile(path)
	if err != nil {
		t.Fatalf("readSummaryFile: %v", err)
	}

	if v, ok := findPoint(metrics, "tpcc_tpm", map[string]string{"transaction": "NEW_ORDER", "status": "ok"}); !ok || v != 258.1 {
		t.Errorf("tpcc_tpm NEW_ORDER/ok = %v (ok=%v), want 258.1", v, ok)
	}
	if v, ok := findPoint(metrics, "tpcc_tpm", map[string]string{"transaction": "NEW_ORDER", "status": "error"}); !ok || v != 0.2 {
		t.Errorf("tpcc_tpm NEW_ORDER/error = %v (ok=%v), want 0.2", v, ok)
	}
	if v, ok := findPoint(metrics, "tpcc_count", map[string]string{"transaction": "DELIVERY", "status": "ok"}); !ok || v != 128 {
		t.Errorf("tpcc_count DELIVERY = %v (ok=%v), want 128", v, ok)
	}
	if v, ok := findPoint(metrics, "tpcc_duration_seconds", map[string]string{"transaction": "NEW_ORDER", "status": "ok"}); !ok || v != 299.9 {
		t.Errorf("tpcc_duration_seconds NEW_ORDER = %v (ok=%v), want 299.9", v, ok)
	}
	for quantile, want := range map[string]float64{
		"p50": 8, "p90": 24, "p95": 32, "p99": 64, "p99_9": 128, "avg": 12.4, "max": 192,
	} {
		if v, ok := findPoint(metrics, "tpcc_latency_ms", map[string]string{"transaction": "NEW_ORDER", "status": "ok", "quantile": quantile}); !ok || v != want {
			t.Errorf("tpcc_latency_ms NEW_ORDER quantile=%s = %v (ok=%v), want %v", quantile, v, ok, want)
		}
	}

	// tpm/tpm_total/efficiency_pct must not leak through as flat keys or
	// points; summaryDoc intentionally doesn't model them.
	for k := range metrics {
		if k != "_structured" {
			t.Errorf("unexpected flat key %q in metrics", k)
		}
	}
}

func TestReadSummaryFile_MissingFileErrors(t *testing.T) {
	_, err := readSummaryFile(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err == nil {
		t.Fatal("expected error for missing summary file")
	}
}

func TestReadSummaryFile_InvalidJSONErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "summary.json")
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	_, err := readSummaryFile(path)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

// ---- Run() wiring: summary/raw-samples files are adapter-owned, never scenario-facing ----

// mockRunnerWithScratchFiles simulates a real go-tpc binary: Run() always
// appends --summary-file=/--raw-samples-file= pointing at its own generated
// scratch paths, and a real go-tpc would write to them. This mock inspects
// the args it's given and writes the supplied fixture content to whichever
// scratch paths it finds, so tests can control what "go-tpc" produced
// without needing to know the adapter's internal path scheme. Pass "" for
// either fixture to simulate go-tpc not producing that file at all (e.g. an
// incompatible binary), for tests that expect Run() to surface that as an
// error.
func mockRunnerWithScratchFiles(summaryJSON, rawCSV string) cmdRunner {
	return func(_ context.Context, _ io.Writer, _ string, args ...string) ([]byte, error) {
		for _, a := range args {
			if path, ok := strings.CutPrefix(a, "--summary-file="); ok && summaryJSON != "" {
				if err := os.WriteFile(path, []byte(summaryJSON), 0o644); err != nil {
					return nil, err
				}
			}
			if path, ok := strings.CutPrefix(a, "--raw-samples-file="); ok && rawCSV != "" {
				if err := os.WriteFile(path, []byte(rawCSV), 0o644); err != nil {
					return nil, err
				}
			}
		}
		return []byte(sampleOutput), nil
	}
}

const sampleRawSamplesCSV = "t_seconds,transaction,status,count,tpm,avg_latency_ms,p50_latency_ms,p90_latency_ms,p95_latency_ms,p99_latency_ms,p99_9_latency_ms,max_latency_ms\n1.0,NEW_ORDER,ok,10,600,5,4,6,7,8,9,10\n"

func TestRun_ReadsBackItsOwnGeneratedSummaryFile(t *testing.T) {
	a := newWithRunner(mockRunnerWithScratchFiles(sampleSummaryJSON, sampleRawSamplesCSV))
	step := schema.SuiteStep{Command: "run"}
	metrics, err := a.Run(context.Background(), fullOutputs(), step)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if v, ok := findPoint(metrics, "tpcc_tpm", map[string]string{"transaction": "NEW_ORDER", "status": "ok"}); !ok || v != 258.1 {
		t.Errorf("tpcc_tpm NEW_ORDER/ok = %v (ok=%v), want 258.1", v, ok)
	}
	if _, ok := metrics["raw_output"]; ok {
		t.Error("raw_output should not be populated: no collector consumes it")
	}
}

func TestRun_ReadsBackItsOwnGeneratedRawSamplesFile(t *testing.T) {
	a := newWithRunner(mockRunnerWithScratchFiles(sampleSummaryJSON, sampleRawSamplesCSV))
	step := schema.SuiteStep{Command: "run"}
	metrics, err := a.Run(context.Background(), fullOutputs(), step)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, ok := metrics["raw_samples_csv"].(string)
	if !ok {
		t.Fatal("raw_samples_csv not present in metrics")
	}
	if got != sampleRawSamplesCSV {
		t.Errorf("raw_samples_csv = %q, want %q", got, sampleRawSamplesCSV)
	}
}

// TestRun_RejectsScenarioSuppliedReservedArgs locks in the guard: a scenario
// setting raw-samples-file/summary-file itself must fail loudly rather than
// silently doing something surprising.
func TestRun_RejectsScenarioSuppliedReservedArgs(t *testing.T) {
	for _, key := range []string{"raw-samples-file", "summary-file"} {
		t.Run(key, func(t *testing.T) {
			a := newWithRunner(mockRunnerWithScratchFiles(sampleSummaryJSON, sampleRawSamplesCSV))
			step := schema.SuiteStep{
				Command: "run",
				Args:    map[string]string{key: "/tmp/whatever"},
			}
			_, err := a.Run(context.Background(), fullOutputs(), step)
			if err == nil {
				t.Fatalf("expected an error when a scenario sets %q", key)
			}
			if !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "managed internally") {
				t.Errorf("error %q should name %q and explain it's managed internally", err, key)
			}
		})
	}
}

// TestRun_RemovesScratchFilesAfterReading guards against a scratch path
// lingering on disk after it's been read; a leftover file previously got
// mistaken for a real fetched artifact when these paths were still
// scenario-configured and happened to collide with the stdout collector's
// own raw_samples_*.csv naming.
func TestRun_RemovesScratchFilesAfterReading(t *testing.T) {
	var capturedArgs []string
	mock := func(_ context.Context, _ io.Writer, _ string, args ...string) ([]byte, error) {
		capturedArgs = args
		return mockRunnerWithScratchFiles(sampleSummaryJSON, sampleRawSamplesCSV)(context.Background(), nil, "", args...)
	}
	a := newWithRunner(mock)
	step := schema.SuiteStep{Command: "run"}
	if _, err := a.Run(context.Background(), fullOutputs(), step); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, a := range capturedArgs {
		for _, prefix := range []string{"--summary-file=", "--raw-samples-file="} {
			if path, ok := strings.CutPrefix(a, prefix); ok {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("scratch path %s should be removed after Run, stat err = %v", path, err)
				}
			}
		}
	}
}

func TestRun_MissingRawSamplesFileErrors(t *testing.T) {
	// go-tpc reports success but doesn't write the raw-samples file:
	// simulates an incompatible binary. Must surface as a clear error, not
	// silently degrade.
	a := newWithRunner(mockRunnerWithScratchFiles(sampleSummaryJSON, ""))
	step := schema.SuiteStep{Command: "run"}
	_, err := a.Run(context.Background(), fullOutputs(), step)
	if err == nil {
		t.Fatal("expected error when go-tpc didn't produce the raw-samples file")
	}
}

func TestRun_MissingSummaryFileErrors(t *testing.T) {
	// Same, for the summary file.
	a := newWithRunner(mockRunnerWithScratchFiles("", sampleRawSamplesCSV))
	step := schema.SuiteStep{Command: "run"}
	_, err := a.Run(context.Background(), fullOutputs(), step)
	if err == nil {
		t.Fatal("expected error when go-tpc didn't produce the summary file")
	}
}

// sanity check that our JSON fixture round-trips the way go-tpc's own
// encoder would (guards against a typo in sampleSummaryJSON above).
func TestSampleSummaryJSON_IsValid(t *testing.T) {
	var doc summaryDoc
	if err := json.Unmarshal([]byte(sampleSummaryJSON), &doc); err != nil {
		t.Fatalf("sampleSummaryJSON is not valid: %v", err)
	}
	if len(doc.Transactions) != 3 {
		t.Fatalf("want 3 transactions, got %d", len(doc.Transactions))
	}
}
