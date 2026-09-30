package supabase

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbarena/benchctl/internal/config"
	"github.com/dbarena/benchctl/internal/diagnostics"
)

var (
	winStart = time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	winEnd   = winStart.Add(time.Hour)
)

// fakeAPI serves canned Management API responses over httptest, so request
// shapes are exercised through a real HTTP round trip.
type fakeAPI struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	requests []*recordedRequest
	handler  func(*recordedRequest) (int, string)
}

type recordedRequest struct {
	path  string
	query url.Values
	auth  string
}

func newFakeAPI(t *testing.T, handler func(*recordedRequest) (int, string)) *fakeAPI {
	f := &fakeAPI{t: t, handler: handler}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &recordedRequest{path: r.URL.Path, query: r.URL.Query(), auth: r.Header.Get("Authorization")}
		f.mu.Lock()
		f.requests = append(f.requests, rec)
		f.mu.Unlock()
		status, body := f.handler(rec)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeAPI) matching(substr string) []*recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*recordedRequest
	for _, r := range f.requests {
		if strings.Contains(r.path, substr) {
			out = append(out, r)
		}
	}
	return out
}

// fakeCLI records the argv it was handed and optionally writes the CSVs the
// real `supabase inspect report` would.
type fakeCLI struct {
	calls   [][]string
	err     error
	writeTo func(dir string)
}

func (f *fakeCLI) run(_ context.Context, _ []string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	if f.err != nil {
		return nil, f.err
	}
	if f.writeTo != nil {
		for i, a := range args {
			if a == "--output-dir" && i+1 < len(args) {
				f.writeTo(args[i+1])
			}
		}
	}
	return nil, nil
}

// writeReportCSVs mimics the CLI, which writes into a dated subdirectory of
// --output-dir rather than the directory itself.
func writeReportCSVs(t *testing.T, names ...string) func(string) {
	return func(dir string) {
		dated := filepath.Join(dir, "2026-09-29")
		if err := os.MkdirAll(dated, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		for _, n := range names {
			if err := os.WriteFile(filepath.Join(dated, n), []byte("a,b\n1,2\n"), 0o644); err != nil {
				t.Fatalf("write %s: %v", n, err)
			}
		}
	}
}

func okHandler(r *recordedRequest) (int, string) {
	if strings.Contains(r.path, "/analytics/endpoints/logs") {
		return 200, `{"result":[{"timestamp":1790671200000000,"event_message":"LOG:  checkpoint starting: time","error_severity":"LOG"}],"error":null}`
	}
	return 200, `{"ok":true}`
}

func (f *fakeAPI) collector(cli *fakeCLI) *Collector {
	return &Collector{
		doer:         f.server.Client(),
		run:          cli.run,
		cfg:          &config.Config{},
		hostOverride: f.server.URL,
		projectRef:   "abcdefghijklmnop",
		dbHost:       "db.abcdefghijklmnop.supabase.co",
		token:        "sbp_test",
	}
}

func testRequest(t *testing.T) diagnostics.Request {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "diagnostics")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return diagnostics.Request{
		RunID:   "run-1",
		Outputs: map[string]string{"project_ref": "abcdefghijklmnop", "host": "db.abcdefghijklmnop.supabase.co"},
		Windows: []diagnostics.Window{{Name: "benchmark", Iteration: 1, Start: winStart, End: winEnd}},
		Dest:    dest,
	}
}

func TestCollect_WritesEverySource(t *testing.T) {
	f := newFakeAPI(t, okHandler)
	cli := &fakeCLI{writeTo: writeReportCSVs(t, "calls.csv", "outliers.csv", "table_stats.csv")}
	req := testRequest(t)

	res, err := f.collector(cli).Collect(context.Background(), req)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	want := []string{
		"project.json", "disk.json", "disk_usage.json",
		"postgres_config.json", "addons.json",
		"postgres.log",
		filepath.Join("inspect", "2026-09-29", "calls.csv"),
		filepath.Join("inspect", "2026-09-29", "outliers.csv"),
	}
	got := map[string]bool{}
	for _, a := range res.Artifacts {
		got[a.File] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing artifact %s; got %v", w, res.Artifacts)
			continue
		}
		if _, err := os.Stat(filepath.Join(req.Dest, w)); err != nil {
			t.Errorf("%s recorded but not written: %v", w, err)
		}
	}
	for _, r := range f.matching("") {
		if r.auth != "Bearer sbp_test" {
			t.Errorf("%s sent Authorization %q", r.path, r.auth)
		}
	}
}

// TestCollect_HealthyRunIsQuiet checks a run where everything worked produces
// no warnings at all. There used to be an unconditional note here about
// Supabase having no metric history; it fired on every fetch and told the
// operator nothing they could act on.
func TestCollect_HealthyRunIsQuiet(t *testing.T) {
	f := newFakeAPI(t, okHandler)
	req := testRequest(t)

	res, err := f.collector(&fakeCLI{writeTo: writeReportCSVs(t, "calls.csv")}).Collect(context.Background(), req)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("a healthy run produced warnings: %v", res.Warnings)
	}
}

// TestCollect_TrimsTheAddonPriceList covers the 10 KB of addon products a
// project did not buy, which the billing endpoint returns alongside the one
// field that matters.
func TestCollect_TrimsTheAddonPriceList(t *testing.T) {
	f := newFakeAPI(t, func(r *recordedRequest) (int, string) {
		if strings.Contains(r.path, "/billing/addons") {
			return 200, `{"selected_addons":[{"type":"compute_instance","variant":{"id":"ci_small"}}],` +
				`"available_addons":[{"name":"Custom Domain"},{"name":"IPv4"},{"name":"Log Drain"}]}`
		}
		return okHandler(r)
	})
	req := testRequest(t)
	var res diagnostics.Result

	f.collector(&fakeCLI{}).collectConfig(context.Background(), req, &res)

	data, err := os.ReadFile(filepath.Join(req.Dest, "addons.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(data), "available_addons") {
		t.Errorf("the price list survived:\n%s", data)
	}
	if !strings.Contains(string(data), "ci_small") {
		t.Errorf("the compute variant was lost:\n%s", data)
	}
}

// TestConfigSnapshots_NoVersionEndpoint guards a duplicate: project.json
// already carries database.version, postgres_engine and release_channel, so
// /upgrade/eligibility adds nothing.
func TestConfigSnapshots_NoVersionEndpoint(t *testing.T) {
	for _, s := range configSnapshots {
		if strings.Contains(s.path, "upgrade") || s.file == "version.json" {
			t.Errorf("%s duplicates project.json's version fields", s.file)
		}
	}
}

func TestKeepField(t *testing.T) {
	out, err := keepField([]byte(`{"a":{"x":1},"b":[1,2,3]}`), "a")
	if err != nil {
		t.Fatalf("keepField: %v", err)
	}
	if strings.Contains(string(out), `"b"`) || !strings.Contains(string(out), `"x"`) {
		t.Errorf("got %s", out)
	}
	// A response without the field is passed through rather than emptied.
	same, err := keepField([]byte(`{"other":1}`), "a")
	if err != nil || string(same) != `{"other":1}` {
		t.Errorf("got %s, %v", same, err)
	}
	if _, err := keepField([]byte(`{nope`), "a"); err == nil {
		t.Error("expected a parse error")
	}
}

// TestCollect_PartialFailureKeepsTheRest covers the inspect report failing,
// which needs a live database connection and so is the likeliest source to
// break on a torn-down project.
func TestCollect_PartialFailureKeepsTheRest(t *testing.T) {
	f := newFakeAPI(t, okHandler)
	cli := &fakeCLI{err: errors.New("failed to connect to database")}
	req := testRequest(t)

	res, err := f.collector(cli).Collect(context.Background(), req)
	if err != nil {
		t.Fatalf("Collect returned an error for a partial failure: %v", err)
	}
	if !strings.Contains(strings.Join(res.Warnings, " "), "failed to connect") {
		t.Errorf("the CLI failure was not reported: %v", res.Warnings)
	}
	for _, want := range []string{"disk.json", "postgres.log"} {
		var found bool
		for _, a := range res.Artifacts {
			if a.File == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s was lost to an unrelated failure", want)
		}
	}
}

func TestPreflight(t *testing.T) {
	tests := []struct {
		name    string
		outputs map[string]string
		cfg     *config.Config
		wantIn  string
	}{
		{
			name:    "no project ref",
			outputs: map[string]string{"host": "db.x.supabase.co"},
			cfg:     &config.Config{},
			wantIn:  "exports no project_ref",
		},
		{
			name:    "no access token",
			outputs: map[string]string{"project_ref": "abc", "host": "db.abc.supabase.co"},
			cfg:     &config.Config{},
			wantIn:  "BENCHCTL_SUPABASE_ACCESS_TOKEN",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := New(tt.cfg)
			err := c.Preflight(diagnostics.Request{Outputs: tt.outputs})
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("got %v, want it to mention %q", err, tt.wantIn)
			}
		})
	}
}

// TestAPIBase_DerivesTheEnvironment guards against a staging project being
// queried against production, which would return 404s that look like a
// missing project rather than a wrong host.
func TestAPIBase_DerivesTheEnvironment(t *testing.T) {
	tests := []struct{ dbHost, want string }{
		{"db.abc.supabase.co", "https://api.supabase.com/v1/projects/abc"},
		{"db.abc.supabase.red", "https://api.supabase.green/v1/projects/abc"},
	}
	for _, tt := range tests {
		c := &Collector{projectRef: "abc", dbHost: tt.dbHost}
		if got := c.apiBase(); got != tt.want {
			t.Errorf("apiBase for %s = %q, want %q", tt.dbHost, got, tt.want)
		}
	}
}

func TestInspectReport_UsesTheProjectRefNotThePassword(t *testing.T) {
	f := newFakeAPI(t, okHandler)
	cli := &fakeCLI{writeTo: writeReportCSVs(t, "calls.csv")}
	req := testRequest(t)

	var res diagnostics.Result
	f.collector(cli).collectInspectReport(context.Background(), req, &res)

	if len(cli.calls) != 1 {
		t.Fatalf("%d CLI calls, want 1", len(cli.calls))
	}
	argv := strings.Join(cli.calls[0], " ")
	if !strings.Contains(argv, "--project-ref abcdefghijklmnop") {
		t.Errorf("argv = %q, want --project-ref", argv)
	}
	// --db-url would put the database password in the argv, and from there
	// into index.json's Command field.
	if strings.Contains(argv, "--db-url") {
		t.Errorf("argv passes a connection string: %q", argv)
	}
	for _, a := range res.Artifacts {
		if strings.Contains(a.Command, "--db-url") || strings.Contains(a.Command, "postgres://") {
			t.Errorf("recorded command leaks a connection string: %s", a.Command)
		}
	}
}

func TestAPIErrorMessage(t *testing.T) {
	if got := apiErrorMessage([]byte(`{"message":"Project not found"}`), "404 Not Found"); got != "Project not found" {
		t.Errorf("got %q", got)
	}
	if got := apiErrorMessage([]byte(`nope`), "500 Internal Server Error"); got != "500 Internal Server Error" {
		t.Errorf("got %q, want the status fallback", got)
	}
}

func TestConfigSnapshots_EachHasAReason(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range configSnapshots {
		if s.why == "" {
			t.Errorf("%s has no recorded reason for being collected", s.file)
		}
		if seen[s.file] {
			t.Errorf("%s collected twice", s.file)
		}
		seen[s.file] = true
		if !strings.HasSuffix(s.file, ".json") {
			t.Errorf("%s is not a .json artifact", s.file)
		}
	}
	// The IO ceiling is the one snapshot a benchmark cannot do without: it is
	// the only place provisioned IOPS and throughput are reported.
	if !seen["disk.json"] {
		t.Error("disk.json is missing; it is the only source of provisioned IOPS")
	}
}
