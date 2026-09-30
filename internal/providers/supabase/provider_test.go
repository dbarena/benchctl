package supabase

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dbarena/benchctl/internal/config"
	"github.com/dbarena/benchctl/internal/engine"
)

// --- parseProjectRef ---

func TestParseProjectRef(t *testing.T) {
	input := []byte(`{"id":"qwppazzskyahpwdbxlax","ref":"qwppazzskyahpwdbxlax","organization_id":"xjynhifqpbqndckwpead","name":"bench-run-123","region":"us-east-1","status":"UNKNOWN","message":"Created project"}`)
	ref, err := parseProjectRef(input)
	if err != nil {
		t.Fatalf("parseProjectRef: %v", err)
	}
	if ref != "qwppazzskyahpwdbxlax" {
		t.Errorf("got %q, want qwppazzskyahpwdbxlax", ref)
	}
}

func TestParseProjectRef_MissingRef(t *testing.T) {
	_, err := parseProjectRef([]byte(`{"status":"UNKNOWN"}`))
	if err == nil {
		t.Fatal("expected error for missing ref")
	}
}

// --- projectStatusAndHost ---

func TestProjectStatusAndHost(t *testing.T) {
	input := []byte(`{"projects":[{"id":"abc","ref":"abc123","status":"ACTIVE_HEALTHY","database":{"host":"db.abc123.supabase.red","version":"17.0"}}],"message":""}`)
	status, host, err := projectStatusAndHost(input, "abc123")
	if err != nil {
		t.Fatalf("projectStatusAndHost: %v", err)
	}
	if status != "ACTIVE_HEALTHY" {
		t.Errorf("status: got %q, want ACTIVE_HEALTHY", status)
	}
	if host != "db.abc123.supabase.red" {
		t.Errorf("host: got %q, want db.abc123.supabase.red", host)
	}
}

func TestProjectStatusAndHost_NotFound(t *testing.T) {
	input := []byte(`{"projects":[{"ref":"other","status":"ACTIVE_HEALTHY","database":{"host":"db.other.supabase.co"}}],"message":""}`)
	_, _, err := projectStatusAndHost(input, "missing")
	if !errors.Is(err, errProjectNotListed) {
		t.Fatalf("got %v, want errProjectNotListed", err)
	}
}

// --- connectionDetails ---

func TestConnectionDetails_Direct_Production(t *testing.T) {
	host, port, user := connectionDetails("db.abc123.supabase.co", "abc123", connModeDirect, "us-east-1")
	if host != "db.abc123.supabase.co" {
		t.Errorf("host: got %q", host)
	}
	if port != "5432" {
		t.Errorf("port: got %q", port)
	}
	if user != "postgres" {
		t.Errorf("user: got %q", user)
	}
}

func TestConnectionDetails_Direct_Staging(t *testing.T) {
	host, port, user := connectionDetails("db.abc123.supabase.red", "abc123", connModeDirect, "us-east-1")
	if host != "db.abc123.supabase.red" {
		t.Errorf("host: got %q", host)
	}
	if port != "5432" {
		t.Errorf("port: got %q", port)
	}
	if user != "postgres" {
		t.Errorf("user: got %q", user)
	}
}

func TestConnectionDetails_SupavisorSession(t *testing.T) {
	host, port, user := connectionDetails("db.abc123.supabase.co", "abc123", connModeSupavisorSession, "us-east-1")
	if host != "aws-0-us-east-1.pooler.supabase.com" {
		t.Errorf("host: got %q, want aws-0-us-east-1.pooler.supabase.com", host)
	}
	if port != "5432" {
		t.Errorf("port: got %q", port)
	}
	if user != "postgres.abc123" {
		t.Errorf("user: got %q, want postgres.abc123", user)
	}
}

func TestConnectionDetails_SupavisorSession_Staging(t *testing.T) {
	host, port, user := connectionDetails("db.abc123.supabase.red", "abc123", connModeSupavisorSession, "us-east-1")
	if host != "aws-0-us-east-1.pooler.supabase.green" {
		t.Errorf("host: got %q, want aws-0-us-east-1.pooler.supabase.green", host)
	}
	if port != "5432" {
		t.Errorf("port: got %q", port)
	}
	if user != "postgres.abc123" {
		t.Errorf("user: got %q, want postgres.abc123", user)
	}
}

func TestConnectionDetails_SupavisorTransaction(t *testing.T) {
	host, port, user := connectionDetails("db.abc123.supabase.co", "abc123", connModeSupavisorTransaction, "eu-central-1")
	if host != "aws-0-eu-central-1.pooler.supabase.com" {
		t.Errorf("host: got %q", host)
	}
	if port != "6543" {
		t.Errorf("port: got %q, want 6543", port)
	}
	if user != "postgres.abc123" {
		t.Errorf("user: got %q", user)
	}
}

// --- usesPooler ---

func TestUsesPooler(t *testing.T) {
	cases := []struct {
		mode string
		want bool
	}{
		{connModeDirect, false},
		{connModeSupavisorSession, true},
		{connModeSupavisorTransaction, true},
	}
	for _, c := range cases {
		if got := usesPooler(c.mode); got != c.want {
			t.Errorf("usesPooler(%q) = %v, want %v", c.mode, got, c.want)
		}
	}
}

// --- globalArgs ---

func TestBuildCreateArgs_Plain(t *testing.T) {
	args := globalArgs("", "/tmp/work")
	if len(args) != 2 {
		t.Errorf("expected 2 args (--workdir only), got %v", args)
	}
	if args[0] != "--workdir" {
		t.Errorf("expected --workdir, got %q", args[0])
	}
}

func TestBuildCreateArgs_WithProfile(t *testing.T) {
	args := globalArgs("supabase-staging", "/tmp/work")
	if args[0] != "--profile" || args[1] != "supabase-staging" {
		t.Errorf("expected --profile supabase-staging, got %v", args)
	}
	if args[2] != "--workdir" {
		t.Errorf("expected --workdir, got %v", args)
	}
}

func TestBuildCreateArgs_ProfileOnly(t *testing.T) {
	args := globalArgs("supabase-staging")
	if len(args) != 2 || args[0] != "--profile" || args[1] != "supabase-staging" {
		t.Errorf("expected [--profile supabase-staging], got %v", args)
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

func TestParseConfig_MissingRegion(t *testing.T) {
	_, err := parseConfig(map[string]any{"org_id": "x", "project_size": "pico", "connection_mode": "direct"})
	if err == nil || !strings.Contains(err.Error(), "region") {
		t.Errorf("expected region error, got %v", err)
	}
}

func TestParseConfig_InvalidConnectionMode(t *testing.T) {
	_, err := parseConfig(map[string]any{
		"region":          "us-east-1",
		"org_id":          "x",
		"project_size":    "pico",
		"connection_mode": "invalid",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Errorf("expected connection_mode error, got %v", err)
	}
}

func TestParseConfig_Valid(t *testing.T) {
	c, err := parseConfig(map[string]any{
		"region":            "us-east-1",
		"org_id":            "myorg",
		"project_size":      "pico",
		"connection_mode":   "supavisor-session",
		"high_availability": true,
		"experimental":      true,
		"release_channel":   "internal",
		"postgres_engine":   "17-acme",
		"profile":           "supabase-staging",
	})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if c.Region != "us-east-1" || c.OrgID != "myorg" || !c.HighAvailability || !c.Experimental ||
		c.ReleaseChannel != "internal" || c.PostgresEngine != "17-acme" || c.Profile != "supabase-staging" {
		t.Errorf("unexpected config: %+v", c)
	}
}

// --- parseConfig: disk_size_gb ---

func TestParseConfig_DiskSizeGB_Absent(t *testing.T) {
	c, err := parseConfig(map[string]any{
		"region": "us-east-1", "org_id": "x", "project_size": "pico", "connection_mode": "direct",
	})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if c.DiskSizeGB != 0 {
		t.Errorf("DiskSizeGB: got %d, want 0", c.DiskSizeGB)
	}
}

func TestParseConfig_DiskSizeGB_StringValue(t *testing.T) {
	c, err := parseConfig(map[string]any{
		"region": "us-east-1", "org_id": "x", "project_size": "pico", "connection_mode": "direct",
		"disk_size_gb": "16",
	})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if c.DiskSizeGB != 16 {
		t.Errorf("DiskSizeGB: got %d, want 16", c.DiskSizeGB)
	}
}

func TestParseConfig_DiskSizeGB_Invalid(t *testing.T) {
	_, err := parseConfig(map[string]any{
		"region": "us-east-1", "org_id": "x", "project_size": "pico", "connection_mode": "direct",
		"disk_size_gb": "not-a-number",
	})
	if err == nil || !strings.Contains(err.Error(), "disk_size_gb") {
		t.Errorf("expected disk_size_gb error, got %v", err)
	}
}

func TestParseConfig_DiskIOPS_Absent(t *testing.T) {
	c, err := parseConfig(map[string]any{
		"region": "us-east-1", "org_id": "x", "project_size": "pico", "connection_mode": "direct",
	})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if c.DiskIOPS != 0 {
		t.Errorf("DiskIOPS: got %d, want 0", c.DiskIOPS)
	}
}

func TestParseConfig_DiskIOPS_StringValue(t *testing.T) {
	c, err := parseConfig(map[string]any{
		"region": "us-east-1", "org_id": "x", "project_size": "pico", "connection_mode": "direct",
		"disk_iops": "6000",
	})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if c.DiskIOPS != 6000 {
		t.Errorf("DiskIOPS: got %d, want 6000", c.DiskIOPS)
	}
}

func TestParseConfig_DiskIOPS_Invalid(t *testing.T) {
	_, err := parseConfig(map[string]any{
		"region": "us-east-1", "org_id": "x", "project_size": "pico", "connection_mode": "direct",
		"disk_iops": "not-a-number",
	})
	if err == nil || !strings.Contains(err.Error(), "disk_iops") {
		t.Errorf("expected disk_iops error, got %v", err)
	}
}

func TestParseConfig_DiskThroughputMibps_Absent(t *testing.T) {
	c, err := parseConfig(map[string]any{
		"region": "us-east-1", "org_id": "x", "project_size": "pico", "connection_mode": "direct",
	})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if c.DiskThroughputMibps != 0 {
		t.Errorf("DiskThroughputMibps: got %d, want 0", c.DiskThroughputMibps)
	}
}

func TestParseConfig_DiskThroughputMibps_StringValue(t *testing.T) {
	c, err := parseConfig(map[string]any{
		"region": "us-east-1", "org_id": "x", "project_size": "pico", "connection_mode": "direct",
		"disk_throughput_mibps": "125",
	})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if c.DiskThroughputMibps != 125 {
		t.Errorf("DiskThroughputMibps: got %d, want 125", c.DiskThroughputMibps)
	}
}

func TestParseConfig_DiskThroughputMibps_Invalid(t *testing.T) {
	_, err := parseConfig(map[string]any{
		"region": "us-east-1", "org_id": "x", "project_size": "pico", "connection_mode": "direct",
		"disk_throughput_mibps": "not-a-number",
	})
	if err == nil || !strings.Contains(err.Error(), "disk_throughput_mibps") {
		t.Errorf("expected disk_throughput_mibps error, got %v", err)
	}
}

// --- parseConfig: disk_type ---

func TestParseConfig_DiskType_DefaultsToGP3(t *testing.T) {
	c, err := parseConfig(map[string]any{
		"region": "us-east-1", "org_id": "x", "project_size": "pico", "connection_mode": "direct",
	})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if c.DiskType != diskTypeGP3 {
		t.Errorf("DiskType: got %q, want %q", c.DiskType, diskTypeGP3)
	}
}

func TestParseConfig_DiskType_IO2(t *testing.T) {
	c, err := parseConfig(map[string]any{
		"region": "us-east-1", "org_id": "x", "project_size": "pico", "connection_mode": "direct",
		"disk_type": "io2",
	})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if c.DiskType != diskTypeIO2 {
		t.Errorf("DiskType: got %q, want %q", c.DiskType, diskTypeIO2)
	}
}

func TestParseConfig_DiskType_Invalid(t *testing.T) {
	_, err := parseConfig(map[string]any{
		"region": "us-east-1", "org_id": "x", "project_size": "pico", "connection_mode": "direct",
		"disk_type": "io1",
	})
	if err == nil || !strings.Contains(err.Error(), "disk_type") {
		t.Errorf("expected disk_type error, got %v", err)
	}
}

// --- ManagementAPIHost ---

func TestManagementAPIHost_Production(t *testing.T) {
	if got := ManagementAPIHost("db.abc123.supabase.co"); got != "api.supabase.com" {
		t.Errorf("got %q, want api.supabase.com", got)
	}
}

func TestManagementAPIHost_Staging(t *testing.T) {
	if got := ManagementAPIHost("db.abc123.supabase.red"); got != "api.supabase.green" {
		t.Errorf("got %q, want api.supabase.green", got)
	}
}

func TestManagementAPIHost_UnknownDomainFallsBackToProduction(t *testing.T) {
	if got := ManagementAPIHost("db.abc123.example.net"); got != "api.supabase.com" {
		t.Errorf("got %q, want api.supabase.com (fallback)", got)
	}
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
	if err := p.resizeDiskIfNeeded(context.Background(), "db.abc.supabase.co", "abc", 0, 0, 0, ""); err != nil {
		t.Fatalf("expected no-op, got error: %v", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("expected no HTTP calls, got %d", len(fake.calls))
	}
}

func TestResizeDiskIfNeeded_NoToken(t *testing.T) {
	fake := &fakeDoer{}
	p := &Provider{out: io.Discard, doer: fake, cfg: &config.Config{}}
	err := p.resizeDiskIfNeeded(context.Background(), "db.abc.supabase.co", "abc", 16, 0, 0, "")
	if err == nil || !strings.Contains(err.Error(), "access token") {
		t.Errorf("expected access token error, got %v", err)
	}
}

func TestResizeDiskIfNeeded_SkipWhenAlreadyBigEnough(t *testing.T) {
	fake := &fakeDoer{responses: []fakeResponse{
		{status: 200, body: `{"attributes":{"size_gb":32,"iops":3000,"type":"gp3","throughput_mibps":125}}`},
	}}
	p := &Provider{out: io.Discard, doer: fake, cfg: &config.Config{Supabase: config.SupabaseConfig{AccessToken: "test-token"}}}
	if err := p.resizeDiskIfNeeded(context.Background(), "db.abc.supabase.co", "abc", 16, 0, 0, ""); err != nil {
		t.Fatalf("expected no-op, got error: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("expected exactly 1 call (GET only), got %d", len(fake.calls))
	}
	if fake.calls[0].Method != http.MethodGet {
		t.Errorf("expected GET, got %s", fake.calls[0].Method)
	}
}

func TestResizeDiskIfNeeded_PostsExpectedBodyAndURL(t *testing.T) {
	fake := &fakeDoer{responses: []fakeResponse{
		{status: 200, body: `{"attributes":{"size_gb":8,"iops":3000,"type":"gp3","throughput_mibps":125}}`},
		{status: 201, body: ``},
		{status: 200, body: `{"attributes":{"size_gb":16,"iops":3000,"type":"gp3","throughput_mibps":125}}`},
	}}
	p := &Provider{out: io.Discard, doer: fake, cfg: &config.Config{Supabase: config.SupabaseConfig{AccessToken: "test-token"}}}
	if err := p.resizeDiskIfNeeded(context.Background(), "db.abc.supabase.co", "abc", 16, 0, 0, ""); err != nil {
		t.Fatalf("resizeDiskIfNeeded: %v", err)
	}
	if len(fake.calls) != 3 {
		t.Fatalf("expected 3 calls (GET, POST, confirmation GET), got %d", len(fake.calls))
	}
	get, post := fake.calls[0], fake.calls[1]
	wantURL := "https://api.supabase.com/v1/projects/abc/config/disk"
	if get.URL.String() != wantURL || get.Method != http.MethodGet {
		t.Errorf("GET: got %s %s, want GET %s", get.Method, get.URL, wantURL)
	}
	if post.URL.String() != wantURL || post.Method != http.MethodPost {
		t.Errorf("POST: got %s %s, want POST %s", post.Method, post.URL, wantURL)
	}
	if got := post.Header.Get("Authorization"); got != "Bearer test-token" {
		t.Errorf("Authorization header: got %q", got)
	}
	body, _ := io.ReadAll(post.Body)
	wantBody := `{"attributes":{"type":"gp3","size_gb":16,"iops":3000}}`
	if string(body) != wantBody {
		t.Errorf("POST body: got %s, want %s", body, wantBody)
	}
}

func TestResizeDiskIfNeeded_CustomIOPSNotHardOverridden(t *testing.T) {
	// Guards against silently pinning every tier to 3000 IOPS: XL's
	// documented baseline (6000, per compute-and-disk docs) must reach the
	// POST body unchanged, not get clamped/overridden.
	fake := &fakeDoer{responses: []fakeResponse{
		{status: 200, body: `{"attributes":{"size_gb":8,"iops":3000,"type":"gp3","throughput_mibps":125}}`},
		{status: 201, body: ``},
		{status: 200, body: `{"attributes":{"size_gb":128,"iops":6000,"type":"gp3","throughput_mibps":125}}`},
	}}
	p := &Provider{out: io.Discard, doer: fake, cfg: &config.Config{Supabase: config.SupabaseConfig{AccessToken: "test-token"}}}
	if err := p.resizeDiskIfNeeded(context.Background(), "db.abc.supabase.co", "abc", 128, 6000, 0, ""); err != nil {
		t.Fatalf("resizeDiskIfNeeded: %v", err)
	}
	body, _ := io.ReadAll(fake.calls[1].Body)
	wantBody := `{"attributes":{"type":"gp3","size_gb":128,"iops":6000}}`
	if string(body) != wantBody {
		t.Errorf("POST body: got %s, want %s", body, wantBody)
	}
}

func TestResizeDiskIfNeeded_IncludesThroughputWhenSet(t *testing.T) {
	fake := &fakeDoer{responses: []fakeResponse{
		{status: 200, body: `{"attributes":{"size_gb":8,"iops":3000,"type":"gp3","throughput_mibps":125}}`},
		{status: 201, body: ``},
		{status: 200, body: `{"attributes":{"size_gb":16,"iops":3000,"type":"gp3","throughput_mibps":125}}`},
	}}
	p := &Provider{out: io.Discard, doer: fake, cfg: &config.Config{Supabase: config.SupabaseConfig{AccessToken: "test-token"}}}
	if err := p.resizeDiskIfNeeded(context.Background(), "db.abc.supabase.co", "abc", 16, 0, 125, ""); err != nil {
		t.Fatalf("resizeDiskIfNeeded: %v", err)
	}
	body, _ := io.ReadAll(fake.calls[1].Body)
	wantBody := `{"attributes":{"type":"gp3","size_gb":16,"iops":3000,"throughput_mibps":125}}`
	if string(body) != wantBody {
		t.Errorf("POST body: got %s, want %s", body, wantBody)
	}
}

func TestResizeDiskIfNeeded_IO2OmitsThroughputAndSetsType(t *testing.T) {
	// io2 doesn't accept a throughput_mibps attribute; a caller passing one
	// (e.g. a leftover gp3-era config value) must not have it forwarded.
	fake := &fakeDoer{responses: []fakeResponse{
		{status: 200, body: `{"attributes":{"size_gb":8,"iops":3000,"type":"gp3"}}`},
		{status: 201, body: ``},
		{status: 200, body: `{"attributes":{"size_gb":512,"iops":20000,"type":"io2"}}`},
	}}
	p := &Provider{out: io.Discard, doer: fake, cfg: &config.Config{Supabase: config.SupabaseConfig{AccessToken: "test-token"}}}
	if err := p.resizeDiskIfNeeded(context.Background(), "db.abc.supabase.co", "abc", 512, 20000, 125, "io2"); err != nil {
		t.Fatalf("resizeDiskIfNeeded: %v", err)
	}
	body, _ := io.ReadAll(fake.calls[1].Body)
	wantBody := `{"attributes":{"type":"io2","size_gb":512,"iops":20000}}`
	if string(body) != wantBody {
		t.Errorf("POST body: got %s, want %s", body, wantBody)
	}
}

func TestResizeDiskIfNeeded_ErrorOnNon2xxPostResponse(t *testing.T) {
	fake := &fakeDoer{responses: []fakeResponse{
		{status: 200, body: `{"attributes":{"size_gb":8,"iops":3000,"type":"gp3","throughput_mibps":125}}`},
		{status: 429, body: `{"message":"Database disk can only be modified once per four hours."}`},
	}}
	p := &Provider{out: io.Discard, doer: fake, cfg: &config.Config{Supabase: config.SupabaseConfig{AccessToken: "test-token"}}}
	err := p.resizeDiskIfNeeded(context.Background(), "db.abc.supabase.co", "abc", 16, 0, 0, "")
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Errorf("expected HTTP 429 error, got %v", err)
	}
}

func TestResizeDiskIfNeeded_ErrorOnGetFailure(t *testing.T) {
	fake := &fakeDoer{responses: []fakeResponse{
		{status: 401, body: `{"message":"Unauthorized"}`},
	}}
	p := &Provider{out: io.Discard, doer: fake, cfg: &config.Config{Supabase: config.SupabaseConfig{AccessToken: "test-token"}}}
	err := p.resizeDiskIfNeeded(context.Background(), "db.abc.supabase.co", "abc", 16, 0, 0, "")
	if err == nil || !strings.Contains(err.Error(), "get current size") {
		t.Errorf("expected get-current-size error, got %v", err)
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
	if err := p.resizeDiskIfNeeded(context.Background(), "db.abc.supabase.co", "abc", 16, 0, 0, ""); err != nil {
		t.Fatalf("resizeDiskIfNeeded: %v", err)
	}
	if len(fake.calls) != 4 {
		t.Fatalf("expected 4 calls (GET, POST, 2 confirmation GETs), got %d", len(fake.calls))
	}
}

// TestResizeDiskIfNeeded_TimesOutIfNeverConfirmed is the regression test for
// the incident that motivated this verification: the POST returned success,
// but the disk's real attributes never changed. Without polling, benchctl
// would have reported the resize as done and continued into a benchmark run
// against an unresized, undersized disk.
func TestResizeDiskIfNeeded_TimesOutIfNeverConfirmed(t *testing.T) {
	fake := &fakeDoer{responses: []fakeResponse{
		{status: 200, body: `{"attributes":{"size_gb":8,"iops":3000,"type":"gp3","throughput_mibps":125}}`},
		{status: 201, body: ``},
		// Never converges; matches the incident, where the volume stayed at
		// its pre-resize size/type/IOPS indefinitely despite the 201.
		{status: 200, body: `{"attributes":{"size_gb":8,"iops":3000,"type":"gp3","throughput_mibps":125}}`},
	}}
	p := &Provider{out: io.Discard, doer: fake, diskResizePollTimeout: time.Nanosecond, cfg: &config.Config{Supabase: config.SupabaseConfig{AccessToken: "test-token"}}}
	err := p.resizeDiskIfNeeded(context.Background(), "db.abc.supabase.co", "abc", 16, 0, 0, "")
	if err == nil || !strings.Contains(err.Error(), "timed out waiting for") {
		t.Errorf("expected a timeout error, got %v", err)
	}
}

// TestResizeDiskIfNeeded_TypeMismatchNotConfirmed guards specifically against
// the incident's gp3-instead-of-io2 symptom: size alone reaching the target
// must not be enough when type/IOPS haven't also converged.
func TestResizeDiskIfNeeded_TypeMismatchNotConfirmed(t *testing.T) {
	fake := &fakeDoer{responses: []fakeResponse{
		{status: 200, body: `{"attributes":{"size_gb":8,"iops":3000,"type":"gp3"}}`},
		{status: 201, body: ``},
		// Size matches but type is still gp3, not the requested io2.
		{status: 200, body: `{"attributes":{"size_gb":16,"iops":3000,"type":"gp3"}}`},
	}}
	p := &Provider{out: io.Discard, doer: fake, diskResizePollTimeout: time.Nanosecond, cfg: &config.Config{Supabase: config.SupabaseConfig{AccessToken: "test-token"}}}
	err := p.resizeDiskIfNeeded(context.Background(), "db.abc.supabase.co", "abc", 16, 20000, 0, "io2")
	if err == nil || !strings.Contains(err.Error(), "timed out waiting for") {
		t.Errorf("expected a timeout error, got %v", err)
	}
}

// --- TeardownMissingProjectRef ---

func TestTeardownMissingProjectRef(t *testing.T) {
	p := New(&config.Config{})
	err := p.Teardown(context.Background(), engine.Outputs{})
	if err == nil || !strings.Contains(err.Error(), outputKeyProjectRef) {
		t.Errorf("expected missing project_ref error, got %v", err)
	}
}

// --- alreadyDeleted ---

func TestAlreadyDeleted_MatchesResourceRemoved(t *testing.T) {
	stderr := []byte(`Failed to delete project qragommjtanxcypltlcr: {"message":"Resource has been removed"}` + "\n")
	if !alreadyDeleted(stderr) {
		t.Error("expected alreadyDeleted to match a 'Resource has been removed' response")
	}
}

func TestAlreadyDeleted_UnrelatedError(t *testing.T) {
	stderr := []byte(`Failed to delete project abc: {"message":"Unauthorized"}` + "\n")
	if alreadyDeleted(stderr) {
		t.Error("expected alreadyDeleted to not match an unrelated error")
	}
}

// --- cliErrorDetail ---

func TestCliErrorDetail_ParsesJSONError(t *testing.T) {
	stdout := []byte(`{"_tag":"Error","error":{"code":"LegacyInvalidAccessTokenError","message":"Invalid access token format. Must be like ` + "`sbp_0102...1920`" + `."}}`)
	got := cliErrorDetail(stdout)
	want := ": Invalid access token format. Must be like `sbp_0102...1920`. (LegacyInvalidAccessTokenError)"
	if got != want {
		t.Errorf("cliErrorDetail() = %q, want %q", got, want)
	}
}

func TestCliErrorDetail_OmitsCodeWhenAbsent(t *testing.T) {
	stdout := []byte(`{"_tag":"Error","error":{"message":"something went wrong"}}`)
	if got, want := cliErrorDetail(stdout), ": something went wrong"; got != want {
		t.Errorf("cliErrorDetail() = %q, want %q", got, want)
	}
}

func TestCliErrorDetail_EmptyOnPlainText(t *testing.T) {
	if got := cliErrorDetail([]byte("some human-readable CLI output\n")); got != "" {
		t.Errorf("cliErrorDetail() = %q, want empty string", got)
	}
}

func TestCliErrorDetail_EmptyOnEmptyStdout(t *testing.T) {
	if got := cliErrorDetail(nil); got != "" {
		t.Errorf("cliErrorDetail() = %q, want empty string", got)
	}
}

// --- RunMetadata ---

func TestRunMetadata_ReturnsProjectID(t *testing.T) {
	p := New(&config.Config{})
	got := p.RunMetadata(engine.Outputs{outputKeyProjectRef: "abcxyz"})
	if got["project_id"] != "abcxyz" {
		t.Errorf("RunMetadata()[project_id] = %v, want abcxyz", got["project_id"])
	}
}

func TestRunMetadata_NilWhenProjectRefAbsent(t *testing.T) {
	p := New(&config.Config{})
	got := p.RunMetadata(engine.Outputs{})
	if got != nil {
		t.Errorf("RunMetadata() = %v, want nil", got)
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
	want := engine.Outputs{outputKeyProjectRef: "proj123", outputKeyProfile: "", outputKeyRunID: "run-1"}
	if len(outputs) != len(want) || outputs[outputKeyProjectRef] != "proj123" || outputs[outputKeyRunID] != "run-1" {
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
	if outputs[outputKeyProjectRef] != "proj123" || outputs[outputKeyRunID] != "run-1" {
		t.Errorf("expected partial outputs carrying the ref so teardown can finish, got %v", outputs)
	}
}

func TestProvision_CreateFails_NoProjectExists(t *testing.T) {
	empty := fakeCmdResponse{stdout: `{"projects":[]}`}
	fake := &fakeCmdRunner{responses: []fakeCmdResponse{
		{stdout: "", stderr: "unauthorized", err: fmt.Errorf("exit status 1")}, // projects create
		empty, empty, empty, // projects list: nothing to recover
	}}
	p := &Provider{out: io.Discard, run: fake.run, checkCLI: noopCheckCLI, recoverLookupInterval: time.Nanosecond, cfg: &config.Config{}}
	outputs, err := p.Provision(context.Background(), "run-1", testProvisionConfig())
	if err == nil || !strings.Contains(err.Error(), "projects create") {
		t.Fatalf("expected a projects-create error, got %v", err)
	}
	if outputs != nil {
		t.Errorf("expected nil outputs when nothing was created, got %v", outputs)
	}
	if len(fake.calls) != 1+recoverLookupAttempts {
		t.Fatalf("expected %d CLI calls with no delete attempt, got %d: %v", 1+recoverLookupAttempts, len(fake.calls), fake.calls)
	}
}

func TestProvision_CreateFails_ListFails(t *testing.T) {
	listErr := fakeCmdResponse{stdout: "", stderr: "network down", err: fmt.Errorf("exit status 1")}
	fake := &fakeCmdRunner{responses: []fakeCmdResponse{
		{stdout: createConflict, err: fmt.Errorf("exit status 1")}, // projects create
		listErr, listErr, listErr, // projects list never answers
	}}
	p := &Provider{out: io.Discard, run: fake.run, checkCLI: noopCheckCLI, recoverLookupInterval: time.Nanosecond, cfg: &config.Config{}}
	outputs, err := p.Provision(context.Background(), "run-1", testProvisionConfig())
	if err == nil || !strings.Contains(err.Error(), "projects create") {
		t.Fatalf("expected the original create error to survive a failed lookup, got %v", err)
	}
	if outputs != nil {
		t.Errorf("expected nil outputs, got %v", outputs)
	}
	if len(fake.calls) != 1+recoverLookupAttempts {
		t.Fatalf("expected %d CLI calls with no delete attempt, got %d: %v", 1+recoverLookupAttempts, len(fake.calls), fake.calls)
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

func TestProjectRefByName_IgnoresOtherOrgs(t *testing.T) {
	data := []byte(`{"projects":[` +
		`{"ref":"other","name":"run-1","organization_id":"org-999","organization_slug":"org-999"},` +
		`{"ref":"proj123","name":"run-1","organization_id":"org-123","organization_slug":"org-123"}]}`)
	got, err := projectRefByName(data, "run-1", "org-123")
	if err != nil || got != "proj123" {
		t.Fatalf("projectRefByName() = %q, %v; want proj123, nil", got, err)
	}
	if _, err := projectRefByName(data, "run-1", "org-absent"); err == nil {
		t.Error("expected no match for an org that owns no project with that name")
	}
}

func TestProjectRefByName_MatchesOrgSlug(t *testing.T) {
	data := []byte(`{"projects":[{"ref":"proj123","name":"run-1","organization_id":"abc","organization_slug":"org-123"}]}`)
	got, err := projectRefByName(data, "run-1", "org-123")
	if err != nil || got != "proj123" {
		t.Fatalf("projectRefByName() = %q, %v; want proj123, nil", got, err)
	}
}

// --- Teardown idempotency ---

func TestTeardown_AlreadyDeletedIsSuccess(t *testing.T) {
	fake := &fakeCmdRunner{responses: []fakeCmdResponse{
		{stdout: "", stderr: "Resource has been removed", err: fmt.Errorf("exit status 1")},
	}}
	p := &Provider{out: io.Discard, run: fake.run, checkCLI: noopCheckCLI, cfg: &config.Config{}}
	err := p.Teardown(context.Background(), engine.Outputs{outputKeyProjectRef: "proj123"})
	if err != nil {
		t.Fatalf("expected an already-removed project to count as torn down, got %v", err)
	}
}

func TestTeardown_DeleteFailureIsReported(t *testing.T) {
	fake := &fakeCmdRunner{responses: []fakeCmdResponse{
		{stdout: "", stderr: "boom", err: fmt.Errorf("exit status 1")},
	}}
	p := &Provider{out: io.Discard, run: fake.run, checkCLI: noopCheckCLI, cfg: &config.Config{}}
	err := p.Teardown(context.Background(), engine.Outputs{outputKeyProjectRef: "proj123"})
	if err == nil || !strings.Contains(err.Error(), "projects delete proj123") {
		t.Fatalf("expected a projects-delete error, got %v", err)
	}
}
