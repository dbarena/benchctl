package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// stubCollector stands in for a per-platform collector. It records the
// request it was handed and returns canned results, so the driver in Run can
// be tested without any cloud access.
type stubCollector struct {
	name         string
	result       Result
	err          error
	preflightErr error
	gotRequest   *Request
	collected    bool
}

func (s *stubCollector) Name() string { return s.name }

func (s *stubCollector) Collect(_ context.Context, req Request) (Result, error) {
	s.collected = true
	s.gotRequest = &req
	return s.result, s.err
}

// preflightingCollector is a separate type so tests can choose whether the
// optional Preflight interface is satisfied at all.
type preflightingCollector struct {
	stubCollector
}

func (p *preflightingCollector) Preflight(Request) error { return p.preflightErr }

func testRequest(dest string) Request {
	return Request{
		RunID:   "run-1",
		Outputs: map[string]string{"dbi_resource_id": "db-ABC"},
		Windows: []Window{{Name: "benchmark", Iteration: 1, Start: t0, End: t1}},
		Dest:    dest,
	}
}

func readIndex(t *testing.T, dest string) Index {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dest, IndexFilename))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatalf("parse index: %v\n%s", err, data)
	}
	return idx
}

func TestRun_WritesIndexOnSuccess(t *testing.T) {
	dest := filepath.Join(t.TempDir(), DirName)
	c := &stubCollector{
		name: VendorAWS,
		result: Result{Artifacts: []Artifact{
			{File: "benchmark_iter1/pi_db_load.json", Command: "aws pi get-resource-metrics ..."},
		}},
	}

	idx, err := Run(context.Background(), nil, c, testRequest(dest))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !c.collected {
		t.Error("collector was never called")
	}

	onDisk := readIndex(t, dest)
	if onDisk.Collector != VendorAWS || onDisk.RunID != "run-1" {
		t.Errorf("index = %+v", onDisk)
	}
	if len(onDisk.Artifacts) != 1 || onDisk.Artifacts[0].Command == "" {
		t.Errorf("artifacts not recorded with their command: %+v", onDisk.Artifacts)
	}
	if len(onDisk.Windows) != 1 || onDisk.Windows[0].Name != "benchmark" {
		t.Errorf("windows not recorded: %+v", onDisk.Windows)
	}
	if len(onDisk.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", onDisk.Warnings)
	}
	if idx.CollectedAt.IsZero() {
		t.Error("CollectedAt not set")
	}
}

// TestRun_PartialFailureKeepsWhatItGot is the shape every collector is
// expected to use: some sources reachable, others not. Both halves have to
// survive, or a single disabled feature would hide everything else.
func TestRun_PartialFailureKeepsWhatItGot(t *testing.T) {
	dest := filepath.Join(t.TempDir(), DirName)
	c := &stubCollector{
		name: VendorGCP,
		result: Result{
			Artifacts: []Artifact{{File: "instance.json", Command: "gcloud sql instances describe"}},
			Warnings:  []string{"query insights is not enabled on this instance"},
		},
	}

	var out bytes.Buffer
	if _, err := Run(context.Background(), &out, c, testRequest(dest)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	idx := readIndex(t, dest)
	if len(idx.Artifacts) != 1 {
		t.Errorf("artifacts dropped: %+v", idx.Artifacts)
	}
	if len(idx.Warnings) != 1 {
		t.Errorf("warnings = %v, want one", idx.Warnings)
	}
	if !bytes.Contains(out.Bytes(), []byte("query insights")) {
		t.Errorf("warning not surfaced to the operator: %q", out.String())
	}
}

// TestRun_CollectorErrorBecomesAWarning is the invariant that keeps
// dbarenactl sweeps alive: a collector that fails outright must still produce
// an index and must not make Run return an error.
func TestRun_CollectorErrorBecomesAWarning(t *testing.T) {
	dest := filepath.Join(t.TempDir(), DirName)
	c := &stubCollector{name: VendorAWS, err: errors.New("AccessDenied: pi:GetResourceMetrics")}

	if _, err := Run(context.Background(), nil, c, testRequest(dest)); err != nil {
		t.Fatalf("Run returned an error for a collector failure: %v", err)
	}
	idx := readIndex(t, dest)
	if len(idx.Warnings) != 1 || idx.Warnings[0] != "AccessDenied: pi:GetResourceMetrics" {
		t.Errorf("warnings = %v, want the collector error", idx.Warnings)
	}
}

// TestRun_FailedPreflightSkipsCollection verifies the optional interface is
// honoured and short-circuits, so a collector with no credentials does not
// get to make a dozen doomed API calls.
func TestRun_FailedPreflightSkipsCollection(t *testing.T) {
	dest := filepath.Join(t.TempDir(), DirName)
	c := &preflightingCollector{stubCollector: stubCollector{name: VendorAWS}}
	c.preflightErr = errors.New("aws CLI not found on PATH")

	if _, err := Run(context.Background(), nil, c, testRequest(dest)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if c.collected {
		t.Error("Collect ran despite a failed preflight")
	}
	idx := readIndex(t, dest)
	if len(idx.Warnings) != 1 {
		t.Fatalf("warnings = %v, want the preflight failure", idx.Warnings)
	}
}

func TestRun_PassingPreflightProceeds(t *testing.T) {
	dest := filepath.Join(t.TempDir(), DirName)
	c := &preflightingCollector{stubCollector: stubCollector{name: VendorAWS}}

	if _, err := Run(context.Background(), nil, c, testRequest(dest)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !c.collected {
		t.Error("Collect was skipped despite a passing preflight")
	}
}

// TestRun_LocalFilesystemFailureIsReported is the one case Run does surface:
// if the destination cannot be written there is nothing to collect into, and
// silently succeeding would be a lie.
func TestRun_LocalFilesystemFailureIsReported(t *testing.T) {
	base := t.TempDir()
	// A regular file where the directory should go.
	blocked := filepath.Join(base, DirName)
	if err := os.WriteFile(blocked, []byte("in the way"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := Run(context.Background(), nil, &stubCollector{name: VendorAWS}, testRequest(blocked)); err == nil {
		t.Fatal("expected an error when the destination cannot be created")
	}
}

// TestRequestWindowDir checks the helper collectors use to place per-window
// files, including that it creates the directory.
func TestRequestWindowDir(t *testing.T) {
	dest := t.TempDir()
	req := Request{Dest: dest}
	w := Window{Name: "benchmark", Iteration: 2, Fixture: map[string]string{"client_threads": "16"}, Start: t0, End: t1}

	dir, err := req.WindowDir(w)
	if err != nil {
		t.Fatalf("WindowDir: %v", err)
	}
	want := filepath.Join(dest, "benchmark_iter2_client_threads-16")
	if dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Errorf("directory not created: %v", err)
	}
}

func TestIndexRoundTripsTimes(t *testing.T) {
	dest := filepath.Join(t.TempDir(), DirName)
	req := testRequest(dest)
	if _, err := Run(context.Background(), nil, &stubCollector{name: VendorAWS}, req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	idx := readIndex(t, dest)
	if !idx.Windows[0].Start.Equal(t0) || !idx.Windows[0].End.Equal(t1) {
		t.Errorf("window = [%s, %s], want [%s, %s]", idx.Windows[0].Start, idx.Windows[0].End, t0, t1)
	}
	if time.Since(idx.CollectedAt) > time.Minute {
		t.Errorf("CollectedAt = %s, want roughly now", idx.CollectedAt)
	}
}
