package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// TestRequestSpan covers the interval whole-run sources use: the earliest
// start and latest end across every window, not just the first.
func TestRequestSpan(t *testing.T) {
	req := Request{Windows: []Window{
		{Start: t0.Add(time.Hour), End: t0.Add(2 * time.Hour)},
		{Start: t0, End: t0.Add(30 * time.Minute)},
		{Start: t0.Add(3 * time.Hour), End: t0.Add(4 * time.Hour)},
	}}

	start, end := req.Span()
	if !start.Equal(t0) {
		t.Errorf("start = %s, want the earliest window start %s", start, t0)
	}
	if want := t0.Add(4 * time.Hour); !end.Equal(want) {
		t.Errorf("end = %s, want the latest window end %s", end, want)
	}
}

// TestRequestSave_RecordsRelativePaths keeps index.json readable and portable:
// an absolute path from the collecting machine is meaningless to a reader.
func TestRequestSave_RecordsRelativePaths(t *testing.T) {
	dest := t.TempDir()
	req := Request{Dest: dest}
	var res Result

	sub := filepath.Join(dest, "benchmark_iter1")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if !req.Save(&res, sub, "cloudwatch.json", "aws cloudwatch get-metric-data", []byte("{}"), nil) {
		t.Fatalf("Save reported failure: %v", res.Warnings)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].File != filepath.Join("benchmark_iter1", "cloudwatch.json") {
		t.Errorf("artifacts = %+v, want a path relative to Dest", res.Artifacts)
	}
}

// TestRequestSave_UpstreamErrorBecomesAWarning verifies a failed source is
// recorded rather than dropped, and writes nothing.
func TestRequestSave_UpstreamErrorBecomesAWarning(t *testing.T) {
	dest := t.TempDir()
	req := Request{Dest: dest}
	var res Result

	if req.Save(&res, dest, "pi_db_load.json", "aws pi ...", nil, errors.New("AccessDenied")) {
		t.Error("Save reported success for a failed source")
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "AccessDenied") {
		t.Errorf("warnings = %v, want the upstream error", res.Warnings)
	}
	if _, err := os.Stat(filepath.Join(dest, "pi_db_load.json")); !os.IsNotExist(err) {
		t.Error("a file was written for a failed source")
	}
}

// TestRequireCLI covers the check collectors reach through an injectable
// field. It is the only place in the diagnostics packages that touches PATH,
// and keeping it here is what lets every collector's Preflight test stay
// hermetic.
func TestRequireCLI(t *testing.T) {
	if err := RequireCLI("no-such-binary-abc123"); err == nil {
		t.Error("expected an error for a binary that is not on PATH")
	} else if !strings.Contains(err.Error(), "no-such-binary-abc123") {
		t.Errorf("error does not name the binary: %v", err)
	}
	// go is on PATH wherever these tests can run at all.
	if err := RequireCLI("go"); err != nil {
		t.Errorf("RequireCLI(go): %v", err)
	}
}
