package supabase

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbarena/benchctl/internal/config"
	"github.com/dbarena/benchctl/internal/engine"
)

// --- parseProjectRef and projectStatusAndHost ---

func TestParseProjectRef(t *testing.T) {
	input := []byte(`{"id":"qwppazzskyahpwdbxlax","ref":"qwppazzskyahpwdbxlax","organization_id":"xjynhifqpbqndckwpead","name":"bench-run-123","region":"us-east-1","status":"UNKNOWN","message":"Created project"}`)
	if ref, err := parseProjectRef(input); err != nil || ref != "qwppazzskyahpwdbxlax" {
		t.Errorf("parseProjectRef() = %q, %v; want qwppazzskyahpwdbxlax", ref, err)
	}
	if _, err := parseProjectRef([]byte(`{"status":"UNKNOWN"}`)); err == nil {
		t.Error("expected an error for a missing ref")
	}
}

func TestProjectStatusAndHost(t *testing.T) {
	input := []byte(`{"projects":[{"id":"abc","ref":"abc123","status":"ACTIVE_HEALTHY","database":{"host":"db.abc123.supabase.red","version":"17.0"}}],"message":""}`)
	status, host, err := projectStatusAndHost(input, "abc123")
	if err != nil || status != "ACTIVE_HEALTHY" || host != "db.abc123.supabase.red" {
		t.Errorf("projectStatusAndHost() = %q, %q, %v", status, host, err)
	}
	if _, _, err := projectStatusAndHost(input, "missing"); !errors.Is(err, errProjectNotListed) {
		t.Errorf("got %v, want errProjectNotListed", err)
	}
}

// --- connectionDetails ---

func TestConnectionDetails(t *testing.T) {
	for _, tc := range []struct {
		name, dbHost, mode, region, wantHost, wantPort, wantUser string
	}{
		{"direct production", "db.abc123.supabase.co", connModeDirect, "us-east-1", "db.abc123.supabase.co", "5432", "postgres"},
		{"direct staging", "db.abc123.supabase.red", connModeDirect, "us-east-1", "db.abc123.supabase.red", "5432", "postgres"},
		{"session production", "db.abc123.supabase.co", connModeSupavisorSession, "us-east-1", "aws-0-us-east-1.pooler.supabase.com", "5432", "postgres.abc123"},
		{"session staging", "db.abc123.supabase.red", connModeSupavisorSession, "us-east-1", "aws-0-us-east-1.pooler.supabase.green", "5432", "postgres.abc123"},
		{"transaction", "db.abc123.supabase.co", connModeSupavisorTransaction, "eu-central-1", "aws-0-eu-central-1.pooler.supabase.com", "6543", "postgres.abc123"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, port, user := connectionDetails(tc.dbHost, "abc123", tc.mode, tc.region)
			if host != tc.wantHost || port != tc.wantPort || user != tc.wantUser {
				t.Errorf("connectionDetails() = %q, %q, %q; want %q, %q, %q", host, port, user, tc.wantHost, tc.wantPort, tc.wantUser)
			}
		})
	}
}

// --- globalArgs and redactArgs ---

func TestGlobalArgs(t *testing.T) {
	for _, tc := range []struct {
		profile string
		workDir []string
		want    []string
	}{
		{"", []string{"/tmp/work"}, []string{"--workdir", "/tmp/work"}},
		{"supabase-staging", []string{"/tmp/work"}, []string{"--profile", "supabase-staging", "--workdir", "/tmp/work"}},
		{"supabase-staging", nil, []string{"--profile", "supabase-staging"}},
	} {
		if got := globalArgs(tc.profile, tc.workDir...); !slices.Equal(got, tc.want) {
			t.Errorf("globalArgs(%q, %v) = %v, want %v", tc.profile, tc.workDir, got, tc.want)
		}
	}
}

// --- redactArgs ---

func TestPasswordNotInArgs(t *testing.T) {
	args := []string{"projects", "create", "--db-password", "supersecret123", "--region", "us-east-1"}
	redacted := redactArgs(args)
	if strings.Contains(redacted, "supersecret123") {
		t.Errorf("password leaked in args output: %s", redacted)
	}
	if !strings.Contains(redacted, "[redacted]") {
		t.Errorf("expected [redacted] in output: %s", redacted)
	}
}

// --- parseConfig ---

func TestParseConfig(t *testing.T) {
	// with is a minimal valid config; a nil value removes the key.
	with := func(kv ...any) map[string]any {
		cfg := map[string]any{"region": "us-east-1", "org_id": "x", "project_size": "pico", "connection_mode": "direct"}
		for i := 0; i < len(kv); i += 2 {
			if kv[i+1] == nil {
				delete(cfg, kv[i].(string))
			} else {
				cfg[kv[i].(string)] = kv[i+1]
			}
		}
		return cfg
	}

	t.Run("valid", func(t *testing.T) {
		c, err := parseConfig(with(
			"org_id", "myorg", "connection_mode", "supavisor-session", "high_availability", true, "experimental", true,
			"release_channel", "internal", "postgres_engine", "17-acme", "profile", "supabase-staging",
		))
		if err != nil {
			t.Fatalf("parseConfig: %v", err)
		}
		if c.Region != "us-east-1" || c.OrgID != "myorg" || !c.HighAvailability || !c.Experimental ||
			c.ReleaseChannel != "internal" || c.PostgresEngine != "17-acme" || c.Profile != "supabase-staging" {
			t.Errorf("unexpected config: %+v", c)
		}
	})

	t.Run("disk keys default to unset and gp3, and accept strings", func(t *testing.T) {
		c, err := parseConfig(with())
		if err != nil || c.DiskSizeGB != 0 || c.DiskIOPS != 0 || c.DiskThroughputMibps != 0 || c.DiskType != diskTypeGP3 {
			t.Errorf("defaults: %+v, %v", c, err)
		}
		c, err = parseConfig(with("disk_size_gb", "16", "disk_iops", "6000", "disk_throughput_mibps", "125", "disk_type", "io2"))
		if err != nil || c.DiskSizeGB != 16 || c.DiskIOPS != 6000 || c.DiskThroughputMibps != 125 || c.DiskType != diskTypeIO2 {
			t.Errorf("values: %+v, %v", c, err)
		}
	})

	for _, tc := range []struct {
		name string
		cfg  map[string]any
		want string
	}{
		{"missing region", with("region", nil), "region"},
		{"invalid connection mode", with("connection_mode", "invalid"), "invalid"},
		{"invalid disk_size_gb", with("disk_size_gb", "not-a-number"), "disk_size_gb"},
		{"invalid disk_iops", with("disk_iops", "not-a-number"), "disk_iops"},
		{"invalid disk_throughput_mibps", with("disk_throughput_mibps", "not-a-number"), "disk_throughput_mibps"},
		{"invalid disk_type", with("disk_type", "io1"), "disk_type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseConfig(tc.cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want an error naming %q", err, tc.want)
			}
		})
	}
}

// --- ManagementAPIBase ---

func TestManagementAPIBase(t *testing.T) {
	for _, tc := range []struct{ profile, dbHost, want string }{
		{"", "db.abc123.supabase.co", "https://api.supabase.com"},
		{"", "db.abc123.supabase.red", "https://api.supabase.green"},
		{"", "db.abc123.example.net", "https://api.supabase.com"},
		// The profile wins: different profiles can share a domain.
		{"supabase-local", "db.abc123.supabase.red", "http://localhost:8080"},
		{"supabase-staging", "db.abc123.supabase.red", "https://api.supabase.green"},
		{"/path/to/profile.yaml", "db.abc123.supabase.red", "https://api.supabase.green"},
	} {
		if got := ManagementAPIBase(tc.profile, tc.dbHost); got != tc.want {
			t.Errorf("ManagementAPIBase(%q, %q) = %q, want %q", tc.profile, tc.dbHost, got, tc.want)
		}
	}
}

func TestTargetProjects(t *testing.T) {
	t.Run("several projects", func(t *testing.T) {
		got, _ := TargetProjects(engine.Outputs{
			outputKeyProjectRefs:          "a,b",
			outputKeyAPIURL + "a":         "https://a.supabase.red",
			outputKeyAPIURL + "b":         "https://b.supabase.red",
			outputKeyPublishableKey + "a": "pub-a",
		})
		want := []TargetProject{{Ref: "a", DBHost: "db.a.supabase.red"}, {Ref: "b", DBHost: "db.b.supabase.red"}}
		if !slices.Equal(got, want) {
			t.Errorf("TargetProjects() = %v, want %v", got, want)
		}
	})

	t.Run("single project from before api_url", func(t *testing.T) {
		got, _ := TargetProjects(engine.Outputs{outputKeyProjectRef: "a", "host": "db.a.supabase.co"})
		if want := []TargetProject{{Ref: "a", DBHost: "db.a.supabase.co"}}; !slices.Equal(got, want) {
			t.Errorf("TargetProjects() = %v, want %v", got, want)
		}
	})

	t.Run("no projects", func(t *testing.T) {
		if got, _ := TargetProjects(engine.Outputs{}); len(got) != 0 {
			t.Errorf("TargetProjects() = %v, want none", got)
		}
	})
}

// --- resizeDiskIfNeeded ---

// fakeResponse is one canned reply for a single fakeDoer.Do call.
type fakeResponse struct {
	status int
	body   string
	err    error
}

// fakeDoer returns configured responses in call order and records requests
// for assertion, without any network access.
type fakeDoer struct {
	responses []fakeResponse
	calls     []*http.Request
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	idx := len(f.calls)
	f.calls = append(f.calls, req)
	if idx >= len(f.responses) {
		return nil, fmt.Errorf("fakeDoer: no response configured for call %d (%s %s)", idx, req.Method, req.URL)
	}
	r := f.responses[idx]
	if r.err != nil {
		return nil, r.err
	}
	return &http.Response{
		StatusCode: r.status,
		Body:       io.NopCloser(strings.NewReader(r.body)),
	}, nil
}

func TestResizeDiskIfNeeded_SkipWhenZero(t *testing.T) {
	// No responses configured, so a call to doer.Do would fail the test via
	// fakeDoer's "no response configured" error, proving sizeGB=0 short-circuits
	// before any HTTP call (including the token check).
	fake := &fakeDoer{}
	p := &Provider{out: io.Discard, doer: fake}
	if err := p.resizeDiskIfNeeded(context.Background(), "", "db.abc.supabase.co", "abc", 0, 0, 0, ""); err != nil {
		t.Fatalf("expected no-op, got error: %v", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("expected no HTTP calls, got %d", len(fake.calls))
	}
}

func TestResizeDiskIfNeeded_SkipWhenAlreadyBigEnough(t *testing.T) {
	fake := &fakeDoer{responses: []fakeResponse{
		{status: 200, body: `{"attributes":{"size_gb":32,"iops":3000,"type":"gp3","throughput_mibps":125}}`},
	}}
	p := &Provider{out: io.Discard, doer: fake, cfg: &config.Config{Supabase: config.SupabaseConfig{AccessToken: "test-token"}}}
	if err := p.resizeDiskIfNeeded(context.Background(), "", "db.abc.supabase.co", "abc", 16, 0, 0, ""); err != nil {
		t.Fatalf("expected no-op, got error: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("expected exactly 1 call (GET only), got %d", len(fake.calls))
	}
	if fake.calls[0].Method != http.MethodGet {
		t.Errorf("expected GET, got %s", fake.calls[0].Method)
	}
}

// A POST body carries what the config asks for: the IOPS the caller chose
// reach the API unchanged, and io2 never gets a throughput.
func TestResizeDiskIfNeeded_PostBody(t *testing.T) {
	const current = `{"attributes":{"size_gb":8,"iops":3000,"type":"gp3","throughput_mibps":125}}`
	for _, tc := range []struct {
		name                     string
		sizeGB, iops, throughput int
		diskType                 string
		resized, wantBody        string
	}{
		{"defaults", 16, 0, 0, "", `{"attributes":{"size_gb":16,"iops":3000,"type":"gp3","throughput_mibps":125}}`,
			`{"attributes":{"type":"gp3","size_gb":16,"iops":3000}}`},
		{"custom iops are not overridden", 128, 6000, 0, "", `{"attributes":{"size_gb":128,"iops":6000,"type":"gp3","throughput_mibps":125}}`,
			`{"attributes":{"type":"gp3","size_gb":128,"iops":6000}}`},
		{"throughput when set", 16, 0, 125, "", `{"attributes":{"size_gb":16,"iops":3000,"type":"gp3","throughput_mibps":125}}`,
			`{"attributes":{"type":"gp3","size_gb":16,"iops":3000,"throughput_mibps":125}}`},
		{"io2 omits throughput", 512, 20000, 125, "io2", `{"attributes":{"size_gb":512,"iops":20000,"type":"io2"}}`,
			`{"attributes":{"type":"io2","size_gb":512,"iops":20000}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeDoer{responses: []fakeResponse{{status: 200, body: current}, {status: 201}, {status: 200, body: tc.resized}}}
			p := &Provider{out: io.Discard, doer: fake, cfg: &config.Config{Supabase: config.SupabaseConfig{AccessToken: "test-token"}}}
			if err := p.resizeDiskIfNeeded(context.Background(), "", "db.abc.supabase.co", "abc", tc.sizeGB, tc.iops, tc.throughput, tc.diskType); err != nil {
				t.Fatalf("resizeDiskIfNeeded: %v", err)
			}
			if len(fake.calls) != 3 {
				t.Fatalf("expected 3 calls (GET, POST, confirmation GET), got %d", len(fake.calls))
			}
			post := fake.calls[1]
			wantURL := "https://api.supabase.com/v1/projects/abc/config/disk"
			if post.URL.String() != wantURL || post.Method != http.MethodPost || post.Header.Get("Authorization") != "Bearer test-token" {
				t.Errorf("POST: got %s %s (auth %q), want POST %s with the token", post.Method, post.URL, post.Header.Get("Authorization"), wantURL)
			}
			if body, _ := io.ReadAll(post.Body); string(body) != tc.wantBody {
				t.Errorf("POST body: got %s, want %s", body, tc.wantBody)
			}
		})
	}
}

// A resize is done only once the disk's real attributes match. The incident
// behind this: the POST returned success, but the volume stayed at its old
// size and type, and benchctl ran against an undersized disk.
func TestResizeDiskIfNeeded_NotConfirmed(t *testing.T) {
	for _, tc := range []struct {
		name           string
		iops           int
		diskType       string
		afterPostAttrs string
	}{
		{"never converges", 0, "", `{"attributes":{"size_gb":8,"iops":3000,"type":"gp3","throughput_mibps":125}}`},
		// Size matches but the type is still gp3, not the requested io2.
		{"type mismatch", 20000, "io2", `{"attributes":{"size_gb":16,"iops":3000,"type":"gp3"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeDoer{responses: []fakeResponse{
				{status: 200, body: `{"attributes":{"size_gb":8,"iops":3000,"type":"gp3"}}`},
				{status: 201},
				{status: 200, body: tc.afterPostAttrs},
			}}
			p := &Provider{out: io.Discard, doer: fake, diskResizePollTimeout: time.Nanosecond, cfg: &config.Config{Supabase: config.SupabaseConfig{AccessToken: "test-token"}}}
			err := p.resizeDiskIfNeeded(context.Background(), "", "db.abc.supabase.co", "abc", 16, tc.iops, 0, tc.diskType)
			if err == nil || !strings.Contains(err.Error(), "timed out waiting for") {
				t.Errorf("expected a timeout error, got %v", err)
			}
		})
	}
}

func TestResizeDiskIfNeeded_ErrorOnNon2xxPostResponse(t *testing.T) {
	fake := &fakeDoer{responses: []fakeResponse{
		{status: 200, body: `{"attributes":{"size_gb":8,"iops":3000,"type":"gp3","throughput_mibps":125}}`},
		{status: 429, body: `{"message":"Database disk can only be modified once per four hours."}`},
	}}
	p := &Provider{out: io.Discard, doer: fake, cfg: &config.Config{Supabase: config.SupabaseConfig{AccessToken: "test-token"}}}
	err := p.resizeDiskIfNeeded(context.Background(), "", "db.abc.supabase.co", "abc", 16, 0, 0, "")
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Errorf("expected HTTP 429 error, got %v", err)
	}
}

// TestResizeDiskIfNeeded_PollsUntilResizeConfirmed guards against trusting a
// successful POST alone: the platform's modifyDisk handler can return success
// before the underlying AWS volume modification has actually applied (see
// resizeDiskIfNeeded's doc comment), so the GET immediately after the POST
// still reporting the old size must not be treated as done.
func TestResizeDiskIfNeeded_PollsUntilResizeConfirmed(t *testing.T) {
	fake := &fakeDoer{responses: []fakeResponse{
		{status: 200, body: `{"attributes":{"size_gb":8,"iops":3000,"type":"gp3","throughput_mibps":125}}`},
		{status: 201, body: ``},
		// Still unresized right after the POST; must not be accepted as success.
		{status: 200, body: `{"attributes":{"size_gb":8,"iops":3000,"type":"gp3","throughput_mibps":125}}`},
		{status: 200, body: `{"attributes":{"size_gb":16,"iops":3000,"type":"gp3","throughput_mibps":125}}`},
	}}
	p := &Provider{out: io.Discard, doer: fake, diskResizePollInterval: time.Millisecond, cfg: &config.Config{Supabase: config.SupabaseConfig{AccessToken: "test-token"}}}
	if err := p.resizeDiskIfNeeded(context.Background(), "", "db.abc.supabase.co", "abc", 16, 0, 0, ""); err != nil {
		t.Fatalf("resizeDiskIfNeeded: %v", err)
	}
	if len(fake.calls) != 4 {
		t.Fatalf("expected 4 calls (GET, POST, 2 confirmation GETs), got %d", len(fake.calls))
	}
}

// --- alreadyDeleted, CLIErrorDetail and RunMetadata ---

func TestAlreadyDeleted(t *testing.T) {
	for _, tc := range []struct {
		stderr string
		want   bool
	}{
		{`Failed to delete project qragommjtanxcypltlcr: {"message":"Resource has been removed"}` + "\n", true},
		{`Failed to delete project abc: {"message":"Unauthorized"}` + "\n", false},
	} {
		if got := alreadyDeleted([]byte(tc.stderr)); got != tc.want {
			t.Errorf("alreadyDeleted(%q) = %v, want %v", tc.stderr, got, tc.want)
		}
	}
}

func TestCLIErrorDetail(t *testing.T) {
	for _, tc := range []struct{ name, stdout, want string }{
		{"json error", `{"_tag":"Error","error":{"code":"LegacyInvalidAccessTokenError","message":"Invalid access token format. Must be like ` + "`sbp_0102...1920`" + `."}}`,
			": Invalid access token format. Must be like `sbp_0102...1920`. (LegacyInvalidAccessTokenError)"},
		{"no code", `{"_tag":"Error","error":{"message":"something went wrong"}}`, ": something went wrong"},
		{"plain text", "some human-readable CLI output\n", ""},
		{"empty", "", ""},
	} {
		if got := CLIErrorDetail([]byte(tc.stdout)); got != tc.want {
			t.Errorf("%s: CLIErrorDetail() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestRunMetadata(t *testing.T) {
	p := New(&config.Config{})
	for _, tc := range []struct {
		outputs engine.Outputs
		want    string
	}{
		{engine.Outputs{outputKeyProjectRef: "abcxyz"}, "abcxyz"},
		{engine.Outputs{outputKeyProjectRefs: "aaa,bbb"}, "aaa,bbb"},
	} {
		if got := p.RunMetadata(tc.outputs); got["project_id"] != tc.want {
			t.Errorf("RunMetadata(%v)[project_id] = %v, want %s", tc.outputs, got["project_id"], tc.want)
		}
	}
	if got := p.RunMetadata(engine.Outputs{}); got != nil {
		t.Errorf("RunMetadata() = %v, want nil without a project ref", got)
	}
}

// --- Provision: leaked-project regression tests ---
//
// These are the regression tests for the incident that motivated this fix:
// a project was created, then the ACTIVE_HEALTHY poll timed out, and
// Provision returned nil outputs, so nothing was ever torn down and the
// project leaked with no local or remote record of its ref.

// fakeCmdResponse is one canned reply for a single fakeCmdRunner call.
type fakeCmdResponse struct {
	stdout string
	stderr string
	err    error
}

// fakeCmdRunner returns configured responses in call order and records the
// args of each call for assertions, without invoking the real supabase CLI.
type fakeCmdRunner struct {
	responses []fakeCmdResponse
	calls     [][]string
}

func (f *fakeCmdRunner) run(_ context.Context, _ []string, _ io.Writer, args ...string) ([]byte, []byte, error) {
	idx := len(f.calls)
	f.calls = append(f.calls, append([]string(nil), args...))
	if idx >= len(f.responses) {
		return nil, nil, fmt.Errorf("fakeCmdRunner: no response configured for call %d (%v)", idx, args)
	}
	r := f.responses[idx]
	return []byte(r.stdout), []byte(r.stderr), r.err
}

// noopCheckCLI stubs out the real exec.LookPath("supabase") check so these
// tests stay hermetic; they exercise fakeCmdRunner, not the real binary,
// and must pass on CI runners that don't have the supabase CLI installed.
func noopCheckCLI() error { return nil }

func testProvisionConfig() map[string]any {
	return map[string]any{
		"region":          "us-east-1",
		"org_id":          "org-123",
		"project_size":    "large",
		"connection_mode": "direct",
	}
}

// waitTimeout and waitInterval drive the two waitForProject timeout tests.
//
// The cadence is inverted on purpose: one wait of waitInterval always
// overshoots a waitTimeout budget, so the loop polls exactly once and then
// gives up, whatever the scheduler does. These used to pass
// pollTimeout: time.Nanosecond, which made the deadline check before the
// first poll a coin flip. Roughly a third of runs polled anyway, consumed the
// delete response as the projects-list reply, and failed with a parse error
// instead of a timeout. A negative timeout is not an option either:
// pollTimeoutOrDefault treats anything <= 0 as unset.
const (
	waitTimeout  = 10 * time.Millisecond
	waitInterval = 25 * time.Millisecond
)

func TestProvision_WaitForProjectTimeout_SelfCleanupSucceeds(t *testing.T) {
	fake := &fakeCmdRunner{responses: []fakeCmdResponse{
		{stdout: `{"ref":"proj123","name":"run-1"}`},                                             // projects create
		{stdout: `{"projects":[{"ref":"proj123","status":"COMING_UP","database":{"host":""}}]}`}, // projects list: not healthy yet
		{stdout: ""}, // projects delete (self-cleanup)
	}}
	p := &Provider{
		out: io.Discard, run: fake.run, checkCLI: noopCheckCLI,
		pollTimeout: waitTimeout, pollInterval: waitInterval,
		cfg: &config.Config{},
	}

	outputs, err := p.Provision(context.Background(), "run-1", testProvisionConfig())
	if err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("expected a did-not-become-ready error, got %v", err)
	}
	if outputs != nil {
		t.Errorf("expected nil outputs after successful self-cleanup, got %v", outputs)
	}
	if len(fake.calls) != 3 {
		t.Fatalf("expected 3 CLI calls (create, list, delete), got %d: %v", len(fake.calls), fake.calls)
	}
	if got := fake.calls[1]; !slices.Contains(got, "list") {
		t.Errorf("second call was not a projects list: %v", got)
	}
}

func TestProvision_OmitsSizeWhenUnset(t *testing.T) {
	// The project never becomes ready, so Provision errors; only the create
	// call's args matter here.
	fake := &fakeCmdRunner{responses: []fakeCmdResponse{
		{stdout: `{"ref":"proj123","name":"run-1"}`},                                             // projects create
		{stdout: `{"projects":[{"ref":"proj123","status":"COMING_UP","database":{"host":""}}]}`}, // projects list
		{stdout: ""}, // projects delete (self-cleanup)
	}}
	p := &Provider{
		out: io.Discard, run: fake.run, checkCLI: noopCheckCLI,
		pollTimeout: waitTimeout, pollInterval: waitInterval,
		cfg: &config.Config{},
	}
	cfg := testProvisionConfig()
	delete(cfg, "project_size")

	_, _ = p.Provision(context.Background(), "run-1", cfg)
	if len(fake.calls) == 0 || !slices.Contains(fake.calls[0], "create") {
		t.Fatalf("expected a projects create call first, got %v", fake.calls)
	}
	if slices.Contains(fake.calls[0], "--size") {
		t.Errorf("expected no --size in create args, got %v", fake.calls[0])
	}
}

func TestProvision_WaitForProjectTimeout_SelfCleanupFails(t *testing.T) {
	fake := &fakeCmdRunner{responses: []fakeCmdResponse{
		{stdout: `{"ref":"proj123","name":"run-1"}`},                                             // projects create
		{stdout: `{"projects":[{"ref":"proj123","status":"COMING_UP","database":{"host":""}}]}`}, // projects list: not healthy yet
		{stdout: "", stderr: "boom", err: fmt.Errorf("exit status 1")},                           // projects delete fails
	}}
	p := &Provider{
		out: io.Discard, run: fake.run, checkCLI: noopCheckCLI,
		pollTimeout: waitTimeout, pollInterval: waitInterval,
		cfg: &config.Config{},
	}

	outputs, err := p.Provision(context.Background(), "run-1", testProvisionConfig())
	if err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("expected a did-not-become-ready error, got %v", err)
	}
	if !strings.Contains(err.Error(), "benchctl teardown run-1") {
		t.Errorf("expected error to mention manual teardown, got %v", err)
	}
	want := engine.Outputs{outputKeyProjectRefs: "proj123", outputKeyProfile: "", outputKeyRunID: "run-1", outputKeyProjectCount: "1"}
	if !maps.Equal(outputs, want) {
		t.Errorf("expected partial outputs %v, got %v", want, outputs)
	}
}

func TestProvision_ParseProjectRefFailure_FoundByName(t *testing.T) {
	fake := &fakeCmdRunner{responses: []fakeCmdResponse{
		{stdout: `{"name":"run-1","status":"UNKNOWN"}`},                                         // projects create: no ref field
		{stdout: `{"projects":[{"ref":"proj123","name":"run-1","organization_id":"org-123"}]}`}, // projects list: found by name
		{stdout: ""}, // projects delete (self-cleanup)
	}}
	p := &Provider{out: io.Discard, run: fake.run, checkCLI: noopCheckCLI, recoverLookupInterval: time.Nanosecond, cfg: &config.Config{}}
	outputs, err := p.Provision(context.Background(), "run-1", testProvisionConfig())
	if err == nil || !strings.Contains(err.Error(), "parse project ref") {
		t.Fatalf("expected a parse-project-ref error, got %v", err)
	}
	if outputs != nil {
		t.Errorf("expected nil outputs after successful self-cleanup, got %v", outputs)
	}
	if len(fake.calls) != 3 {
		t.Fatalf("expected 3 CLI calls (create, list, delete), got %d: %v", len(fake.calls), fake.calls)
	}
}

func TestProvision_ParseProjectRefFailure_NotFoundByName(t *testing.T) {
	noMatch := fakeCmdResponse{stdout: `{"projects":[{"ref":"other","name":"x"}]}`}
	fake := &fakeCmdRunner{responses: []fakeCmdResponse{
		{stdout: `{"name":"run-1","status":"UNKNOWN"}`}, // projects create: no ref field
		noMatch, noMatch, noMatch, // projects list: no matching name, every attempt
	}}
	p := &Provider{out: io.Discard, run: fake.run, checkCLI: noopCheckCLI, recoverLookupInterval: time.Nanosecond, cfg: &config.Config{}}
	outputs, err := p.Provision(context.Background(), "run-1", testProvisionConfig())
	if err == nil || !strings.Contains(err.Error(), "parse project ref") {
		t.Fatalf("expected a parse-project-ref error, got %v", err)
	}
	if outputs != nil {
		t.Errorf("expected nil outputs when no project can be found to clean up, got %v", outputs)
	}
	if len(fake.calls) != 1+recoverLookupAttempts {
		t.Fatalf("expected %d CLI calls (create, then one list per lookup attempt) with no delete attempt, got %d: %v",
			1+recoverLookupAttempts, len(fake.calls), fake.calls)
	}
}

// --- Provision: `projects create` exits non-zero but the project exists ---
//
// The CLI retries POST /v1/projects on transport errors regardless of method,
// so a connection dropped mid-create leaves a live project and the retry fails
// with "already exists". Provision must find that project and delete it rather
// than report the failure and walk away.

// createConflict is what the CLI writes to stdout (with --output-format json)
// when the retried create hits the name it already took.
const createConflict = `{"_tag":"ProjectsCreateUnexpectedStatusError","error":{"code":"ProjectsCreateUnexpectedStatusError",` +
	`"message":"Project with name \"run-1\" already exists in your organization."}}`

func TestProvision_CreateFails_ProjectFoundByName_Deleted(t *testing.T) {
	fake := &fakeCmdRunner{responses: []fakeCmdResponse{
		{stdout: createConflict, err: fmt.Errorf("exit status 1")},                              // projects create
		{stdout: `{"projects":[{"ref":"proj123","name":"run-1","organization_id":"org-123"}]}`}, // projects list
		{stdout: ""}, // projects delete (self-cleanup)
	}}
	p := &Provider{out: io.Discard, run: fake.run, checkCLI: noopCheckCLI, recoverLookupInterval: time.Nanosecond, cfg: &config.Config{}}
	outputs, err := p.Provision(context.Background(), "run-1", testProvisionConfig())
	if err == nil || !strings.Contains(err.Error(), "projects create") {
		t.Fatalf("expected a projects-create error, got %v", err)
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected the CLI error detail to survive, got %v", err)
	}
	if outputs != nil {
		t.Errorf("expected nil outputs after successful cleanup, got %v", outputs)
	}
	if len(fake.calls) != 3 {
		t.Fatalf("expected 3 CLI calls (create, list, delete), got %d: %v", len(fake.calls), fake.calls)
	}
	if got := fake.calls[2]; got[len(got)-1] != "proj123" {
		t.Errorf("expected the recovered ref to be deleted, got %v", got)
	}
}

func TestProvision_CreateFails_ProjectFoundByName_CleanupFails(t *testing.T) {
	fake := &fakeCmdRunner{responses: []fakeCmdResponse{
		{stdout: createConflict, err: fmt.Errorf("exit status 1")},                              // projects create
		{stdout: `{"projects":[{"ref":"proj123","name":"run-1","organization_id":"org-123"}]}`}, // projects list
		{stdout: "", stderr: "boom", err: fmt.Errorf("exit status 1")},                          // projects delete fails
	}}
	p := &Provider{out: io.Discard, run: fake.run, checkCLI: noopCheckCLI, recoverLookupInterval: time.Nanosecond, cfg: &config.Config{}}
	outputs, err := p.Provision(context.Background(), "run-1", testProvisionConfig())
	if err == nil || !strings.Contains(err.Error(), "benchctl teardown run-1") {
		t.Fatalf("expected the error to point at manual teardown, got %v", err)
	}
	if outputs[outputKeyProjectRefs] != "proj123" || outputs[outputKeyRunID] != "run-1" {
		t.Errorf("expected partial outputs carrying the ref so teardown can finish, got %v", outputs)
	}
}

// When no project can be found, there is nothing to clean up, and no delete
// is attempted.
func TestProvision_CreateFails_NothingToCleanUp(t *testing.T) {
	failed := fakeCmdResponse{stderr: "unauthorized", err: fmt.Errorf("exit status 1")}
	empty := fakeCmdResponse{stdout: `{"projects":[]}`}
	conflict := fakeCmdResponse{stdout: createConflict, err: fmt.Errorf("exit status 1")}
	listErr := fakeCmdResponse{stderr: "network down", err: fmt.Errorf("exit status 1")}
	for _, tc := range []struct {
		name      string
		responses []fakeCmdResponse
	}{
		{"no project exists", []fakeCmdResponse{failed, empty, empty, empty}},
		{"the lookup never answers", []fakeCmdResponse{conflict, listErr, listErr, listErr}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeCmdRunner{responses: tc.responses}
			p := &Provider{out: io.Discard, run: fake.run, checkCLI: noopCheckCLI, recoverLookupInterval: time.Nanosecond, cfg: &config.Config{}}
			outputs, err := p.Provision(context.Background(), "run-1", testProvisionConfig())
			if err == nil || !strings.Contains(err.Error(), "projects create") {
				t.Fatalf("expected the create error to survive, got %v", err)
			}
			if outputs != nil {
				t.Errorf("expected nil outputs, got %v", outputs)
			}
			if len(fake.calls) != 1+recoverLookupAttempts {
				t.Fatalf("expected %d CLI calls with no delete attempt, got %d: %v", 1+recoverLookupAttempts, len(fake.calls), fake.calls)
			}
		})
	}
}

func TestProvision_CreateFails_RecoversAfterListLag(t *testing.T) {
	fake := &fakeCmdRunner{responses: []fakeCmdResponse{
		{stdout: createConflict, err: fmt.Errorf("exit status 1")},                              // projects create
		{stdout: `{"projects":[]}`},                                                             // projects list: not listed yet
		{stdout: `{"projects":[{"ref":"proj123","name":"run-1","organization_id":"org-123"}]}`}, // projects list: now visible
		{stdout: ""}, // projects delete (self-cleanup)
	}}
	p := &Provider{out: io.Discard, run: fake.run, checkCLI: noopCheckCLI, recoverLookupInterval: time.Nanosecond, cfg: &config.Config{}}
	outputs, err := p.Provision(context.Background(), "run-1", testProvisionConfig())
	if err == nil || !strings.Contains(err.Error(), "projects create") {
		t.Fatalf("expected a projects-create error, got %v", err)
	}
	if outputs != nil {
		t.Errorf("expected nil outputs after successful cleanup, got %v", outputs)
	}
	if len(fake.calls) != 4 {
		t.Fatalf("expected 4 CLI calls (create, list, list, delete), got %d: %v", len(fake.calls), fake.calls)
	}
}

// --- projectRefByName ---

func TestProjectRefByName(t *testing.T) {
	data := []byte(`{"projects":[` +
		`{"ref":"other","name":"run-1","organization_id":"org-999","organization_slug":"org-999"},` +
		`{"ref":"proj123","name":"run-1","organization_id":"abc","organization_slug":"org-123"}]}`)
	// The org matches by slug or by id, and other orgs' projects are ignored.
	for _, org := range []string{"org-123", "abc"} {
		if got, err := projectRefByName(data, "run-1", org); err != nil || got != "proj123" {
			t.Errorf("projectRefByName(org %s) = %q, %v; want proj123, nil", org, got, err)
		}
	}
	if _, err := projectRefByName(data, "run-1", "org-absent"); err == nil {
		t.Error("expected no match for an org that owns no project with that name")
	}
}

// --- Teardown idempotency ---

// listedRefs is a `projects list` response listing the given refs.
func listedRefs(refs ...string) string {
	listed := make([]string, len(refs))
	for i, ref := range refs {
		listed[i] = fmt.Sprintf(`{"ref":%q,"status":"ACTIVE_HEALTHY"}`, ref)
	}
	return `{"projects":[` + strings.Join(listed, ",") + `]}`
}

func TestTeardown_AlreadyDeletedIsSuccess(t *testing.T) {
	t.Run("no longer listed", func(t *testing.T) {
		fake := &fakeCmdRunner{responses: []fakeCmdResponse{{stdout: listedRefs("other")}}}
		p := &Provider{out: io.Discard, run: fake.run, checkCLI: noopCheckCLI, cfg: &config.Config{}}
		if err := p.Teardown(context.Background(), engine.Outputs{outputKeyProjectRef: "proj123"}); err != nil {
			t.Fatalf("expected an unlisted project to count as torn down, got %v", err)
		}
		if len(fake.calls) != 1 {
			t.Errorf("made %d CLI calls, want only the projects list: %v", len(fake.calls), fake.calls)
		}
	})

	t.Run("removed after listing", func(t *testing.T) {
		fake := &fakeCmdRunner{responses: []fakeCmdResponse{
			{stdout: listedRefs("proj123")},
			{stderr: "Resource has been removed", err: fmt.Errorf("exit status 1")},
		}}
		p := &Provider{out: io.Discard, run: fake.run, checkCLI: noopCheckCLI, cfg: &config.Config{}}
		if err := p.Teardown(context.Background(), engine.Outputs{outputKeyProjectRef: "proj123"}); err != nil {
			t.Fatalf("expected an already-removed project to count as torn down, got %v", err)
		}
	})
}

func TestTeardown_FindsUnrecordedProjectsByName(t *testing.T) {
	// The seed recorded before provisioning holds no refs: benchctl died
	// before it learnt them.
	listed := `{"projects":[
		{"ref":"ref-1","name":"run-1-1","status":"COMING_UP"},
		{"ref":"ref-2","name":"run-1-2","status":"ACTIVE_HEALTHY"},
		{"ref":"ref-other","name":"run-1-3","status":"ACTIVE_HEALTHY"},
		{"ref":"ref-unrelated","name":"someone-else","status":"ACTIVE_HEALTHY"}
	]}`
	fake := &fakeCLI{respond: func(args []string) fakeCmdResponse {
		if slices.Contains(args, "list") {
			return fakeCmdResponse{stdout: listed}
		}
		return fakeCmdResponse{}
	}}
	seed := refOutputs(providerConfig{ProjectCount: 2}, "run-1", nil)
	if err := fake.provider().Teardown(context.Background(), seed); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if got, want := fake.lastArgs("delete"), []string{"ref-1", "ref-2"}; !slices.Equal(got, want) {
		t.Errorf("deleted %v, want only the run's projects %v", got, want)
	}
}

func TestTeardownSeed(t *testing.T) {
	p := &Provider{}
	seed := p.TeardownSeed("run-1", map[string]any{
		"region": "eu-central-1", "org_id": "org", "connection_mode": "direct", "project_count": "3", "profile": "supabase-local",
	})
	if got := runProjectNames(seed); !slices.Equal(got, []string{"run-1-1", "run-1-2", "run-1-3"}) {
		t.Errorf("seed names %v", got)
	}
	if seed[outputKeyProfile] != "supabase-local" {
		t.Errorf("seed profile = %q", seed[outputKeyProfile])
	}
	if p.TeardownSeed("run-1", map[string]any{"project_count": "-1"}) != nil {
		t.Error("expected no seed for an invalid config")
	}
}

func TestDeleteProject_RetriesUntilDeletable(t *testing.T) {
	notReady := fakeCmdResponse{
		stderr: `Failed to delete project ref-1: {"message":"Project not ready for deletion - please try again in 14 minutes."}`,
		err:    fmt.Errorf("exit status 1"),
	}
	fake := &fakeCmdRunner{responses: []fakeCmdResponse{notReady, notReady, {}}}
	p := &Provider{out: io.Discard, run: fake.run, deleteRetryInterval: time.Nanosecond}
	if err := p.deleteProject(context.Background(), nil, "", "ref-1"); err != nil {
		t.Fatalf("deleteProject: %v", err)
	}
	if len(fake.calls) != 3 {
		t.Errorf("made %d delete calls, want 3", len(fake.calls))
	}
}

func TestDeleteProject_StopsRetryingWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	notReady := fakeCmdResponse{stderr: "Project not ready for deletion", err: fmt.Errorf("exit status 1")}
	fake := &fakeCmdRunner{responses: []fakeCmdResponse{notReady}}
	p := &Provider{out: io.Discard, run: fake.run, deleteRetryInterval: time.Hour}
	if err := p.deleteProject(ctx, nil, "", "ref-1"); err == nil {
		t.Fatal("expected an error once the context is done")
	}
}

// --- Multiple projects ---

// fakeCLI answers supabase CLI calls by their arguments rather than call
// order, so it works when projects are provisioned concurrently.
type fakeCLI struct {
	mu      sync.Mutex
	calls   [][]string
	respond func(args []string) fakeCmdResponse
}

func (f *fakeCLI) run(_ context.Context, _ []string, _ io.Writer, args ...string) ([]byte, []byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, slices.Clone(args))
	f.mu.Unlock()
	r := f.respond(args)
	return []byte(r.stdout), []byte(r.stderr), r.err
}

// lastArgs returns the last argument of every call to subcommand, e.g. the
// project name for "create" or the ref for "delete".
func (f *fakeCLI) lastArgs(subcommand string) []string {
	var out []string
	for _, c := range f.calls {
		if slices.Contains(c, subcommand) {
			out = append(out, c[len(c)-1])
		}
	}
	slices.Sort(out)
	return out
}

func (f *fakeCLI) provider() *Provider {
	return &Provider{out: io.Discard, run: f.run, checkCLI: noopCheckCLI, recoverLookupInterval: time.Nanosecond, cfg: &config.Config{}}
}

// healthyProjects answers as if creating each named project succeeds and it
// is ACTIVE_HEALTHY straight away. A project's ref is "ref-" plus its name.
func healthyProjects(names ...string) func(args []string) fakeCmdResponse {
	var listed []string
	for _, n := range names {
		listed = append(listed, fmt.Sprintf(`{"ref":"ref-%[1]s","name":"%[1]s","organization_id":"org-123",`+
			`"status":"ACTIVE_HEALTHY","database":{"host":"db.ref-%[1]s.supabase.red"}}`, n))
	}
	list := `{"projects":[` + strings.Join(listed, ",") + `]}`
	return func(args []string) fakeCmdResponse {
		last := args[len(args)-1]
		switch {
		case slices.Contains(args, "create"):
			return fakeCmdResponse{stdout: fmt.Sprintf(`{"ref":"ref-%s"}`, last)}
		case slices.Contains(args, "list"):
			return fakeCmdResponse{stdout: list}
		case slices.Contains(args, "api-keys"):
			ref := args[slices.Index(args, "--project-ref")+1]
			return fakeCmdResponse{stdout: fmt.Sprintf(`{"keys":[{"name":"anon","type":"legacy","api_key":"anon"},{"name":"default","type":"publishable","api_key":"pub-%s"}]}`, ref)}
		default: // delete
			return fakeCmdResponse{}
		}
	}
}

func TestProvision_Outputs(t *testing.T) {
	t.Run("one project keeps the connection keys", func(t *testing.T) {
		fake := &fakeCLI{respond: healthyProjects("run-1")}
		outputs, err := fake.provider().Provision(context.Background(), "run-1", testProvisionConfig())
		if err != nil {
			t.Fatalf("Provision() error = %v", err)
		}
		for key, want := range map[string]string{
			outputKeyProjectRefs:                  "ref-run-1",
			outputKeyProjectRef:                   "ref-run-1",
			"host":                                "db.ref-run-1.supabase.red",
			outputKeyAPIURL + "ref-run-1":         "https://ref-run-1.supabase.red",
			outputKeyPublishableKey + "ref-run-1": "pub-ref-run-1",
		} {
			if outputs[key] != want {
				t.Errorf("outputs[%s] = %q, want %q", key, outputs[key], want)
			}
		}
	})

	t.Run("several projects are listed by ref", func(t *testing.T) {
		fake := &fakeCLI{respond: healthyProjects("run-1-1", "run-1-2", "run-1-3")}
		cfg := testProvisionConfig()
		cfg["project_count"] = "3"
		outputs, err := fake.provider().Provision(context.Background(), "run-1", cfg)
		if err != nil {
			t.Fatalf("Provision() error = %v", err)
		}
		if got, want := fake.lastArgs("create"), []string{"run-1-1", "run-1-2", "run-1-3"}; !slices.Equal(got, want) {
			t.Errorf("created %v, want %v", got, want)
		}
		if got, want := outputs[outputKeyProjectRefs], "ref-run-1-1,ref-run-1-2,ref-run-1-3"; got != want {
			t.Errorf("project_refs = %q, want %q", got, want)
		}
		if got, want := outputs[outputKeyPublishableKey+"ref-run-1-2"], "pub-ref-run-1-2"; got != want {
			t.Errorf("publishable key = %q, want %q", got, want)
		}
		if got := len(fake.lastArgs("list")); got != 1 {
			t.Errorf("polled projects list %d times, want once for all three projects", got)
		}
		for _, key := range []string{"host", "port", "user", outputKeyProjectRef} {
			if _, ok := outputs[key]; ok {
				t.Errorf("outputs[%s] is set, want it unset for several projects", key)
			}
		}
	})
}

func TestProvision_OneProjectFails_EveryOtherProjectIsCleanedUpOrReturned(t *testing.T) {
	healthy := healthyProjects("run-1-1", "run-1-3")
	fake := &fakeCLI{respond: func(args []string) fakeCmdResponse {
		if slices.Contains(args, "create") && args[len(args)-1] == "run-1-2" {
			return fakeCmdResponse{stderr: "boom", err: fmt.Errorf("exit status 1")}
		}
		return healthy(args)
	}}
	cfg := testProvisionConfig()
	cfg["project_count"] = "3"

	outputs, err := fake.provider().Provision(context.Background(), "run-1", cfg)
	if err == nil || !strings.Contains(err.Error(), "projects create") {
		t.Fatalf("expected the failed create to be reported, got %v", err)
	}
	// However the race between the failure and the other projects plays out,
	// each project that was created is either deleted or left for teardown.
	left := projectRefs(outputs)
	deleted := fake.lastArgs("delete")
	for _, ref := range []string{"ref-run-1-1", "ref-run-1-3"} {
		if slices.Contains(left, ref) == slices.Contains(deleted, ref) {
			t.Errorf("%s: left for teardown = %v, deleted = %v; want exactly one", ref, slices.Contains(left, ref), slices.Contains(deleted, ref))
		}
	}
}

func TestTeardown_DeletesEveryProject(t *testing.T) {
	fake := &fakeCLI{respond: func(args []string) fakeCmdResponse {
		switch {
		case slices.Contains(args, "list"):
			return fakeCmdResponse{stdout: listedRefs("a", "b", "c")}
		case args[len(args)-1] == "b":
			return fakeCmdResponse{stderr: "boom", err: fmt.Errorf("exit status 1")}
		}
		return fakeCmdResponse{}
	}}
	err := fake.provider().Teardown(context.Background(), engine.Outputs{outputKeyProjectRefs: "a,b,c"})
	if err == nil || !strings.Contains(err.Error(), "projects delete b") {
		t.Fatalf("expected the failed delete to be reported, got %v", err)
	}
	if got, want := fake.lastArgs("delete"), []string{"a", "b", "c"}; !slices.Equal(got, want) {
		t.Errorf("deleted %v, want %v (one failure must not stop the rest)", got, want)
	}
}

func TestParsePositiveIntConfig(t *testing.T) {
	for _, tc := range []struct {
		value   any
		want    int
		wantErr bool
	}{
		{value: nil, want: 7},
		{value: "", want: 7},
		{value: "5", want: 5},
		{value: "-1", wantErr: true},
	} {
		got, err := parsePositiveIntConfig(map[string]any{"k": tc.value}, "k", 7)
		if (err != nil) != tc.wantErr || (!tc.wantErr && got != tc.want) {
			t.Errorf("value %v: got %d, %v; want %d, error %v", tc.value, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestParseConfig_ParallelismDefaults(t *testing.T) {
	c, err := parseConfig(testProvisionConfig())
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if c.ProjectCount != 1 || c.MaxParallelProjects != defaultMaxParallelProjects {
		t.Errorf("project_count = %d, max_parallel_projects = %d; want 1, %d", c.ProjectCount, c.MaxParallelProjects, defaultMaxParallelProjects)
	}
}

func TestRunCaptureErr_RetriesRateLimitedCalls(t *testing.T) {
	throttled := fakeCmdResponse{
		stderr: `Failed to delete project a: {"message":"ThrottlerException: Too Many Requests"}`,
		err:    fmt.Errorf("exit status 1"),
	}

	t.Run("succeeds once the limit lifts", func(t *testing.T) {
		fake := &fakeCmdRunner{responses: []fakeCmdResponse{throttled, throttled, {}}}
		p := &Provider{out: io.Discard, run: fake.run, rateLimitBackoff: time.Nanosecond}
		if _, _, err := p.runCaptureErr(context.Background(), nil, "projects", "delete", "a"); err != nil {
			t.Fatalf("runCaptureErr() error = %v", err)
		}
		if len(fake.calls) != 3 {
			t.Errorf("made %d calls, want 3", len(fake.calls))
		}
	})

	t.Run("gives up after rateLimitRetries", func(t *testing.T) {
		fake := &fakeCmdRunner{responses: slices.Repeat([]fakeCmdResponse{throttled}, rateLimitRetries+2)}
		p := &Provider{out: io.Discard, run: fake.run, rateLimitBackoff: time.Nanosecond}
		if _, _, err := p.runCaptureErr(context.Background(), nil, "projects", "delete", "a"); err == nil {
			t.Fatal("expected the last rate-limited error")
		}
		if len(fake.calls) != rateLimitRetries+1 {
			t.Errorf("made %d calls, want %d", len(fake.calls), rateLimitRetries+1)
		}
	})

	t.Run("other failures are not retried", func(t *testing.T) {
		fake := &fakeCmdRunner{responses: []fakeCmdResponse{{stderr: "boom", err: fmt.Errorf("exit status 1")}}}
		p := &Provider{out: io.Discard, run: fake.run, rateLimitBackoff: time.Nanosecond}
		if _, _, err := p.runCaptureErr(context.Background(), nil, "projects", "delete", "a"); err == nil {
			t.Fatal("expected the failure to be returned")
		}
		if len(fake.calls) != 1 {
			t.Errorf("made %d calls, want 1", len(fake.calls))
		}
	})
}
