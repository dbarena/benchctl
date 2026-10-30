package supabase

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbarena/benchctl/internal/config"
	"github.com/dbarena/benchctl/internal/engine"
)

// --- Management API fake ---

// fakeProject is a project the fake Management API knows.
type fakeProject struct {
	name, org, status, host string
}

// fakeAPI is an httptest fake of the Management API's project endpoints. A
// created project's ref is "ref-" plus its name, and it is ACTIVE_HEALTHY on
// supabase.red straight away. override, when set, sees every request first
// and handles it by returning true.
type fakeAPI struct {
	srv      *httptest.Server
	override func(w http.ResponseWriter, r *http.Request, body []byte) bool

	mu       sync.Mutex
	projects map[string]*fakeProject
	calls    []string
	creates  []string
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	// Provision writes its manifest under the home directory.
	t.Setenv("HOME", t.TempDir())
	f := &fakeAPI{projects: map[string]*fakeProject{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	if f.override != nil && f.override(w, r, body) {
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	ref := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/projects/"), "/api-keys")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/projects":
		f.creates = append(f.creates, string(body))
		var b createProjectBody
		_ = json.Unmarshal(body, &b)
		ref := "ref-" + b.Name
		f.projects[ref] = &fakeProject{name: b.Name, org: b.OrganizationSlug, status: "ACTIVE_HEALTHY", host: "db." + ref + ".supabase.red"}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"ref":%q,"status":"UNKNOWN"}`, ref)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/projects":
		writeJSON(w, f.listLocked())
	case f.projects[ref] == nil:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Resource has been removed"}`)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api-keys"):
		fmt.Fprintf(w, `[{"name":"anon","type":"legacy","api_key":"anon"},{"name":"default","type":"publishable","api_key":"pub-%s"}]`, ref)
	case r.Method == http.MethodGet:
		writeJSON(w, f.apiProjectLocked(ref))
	case r.Method == http.MethodDelete:
		delete(f.projects, ref)
		fmt.Fprintf(w, `{"ref":%q}`, ref)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	b, _ := json.Marshal(v)
	_, _ = w.Write(b)
}

func (f *fakeAPI) apiProjectLocked(ref string) map[string]any {
	p := f.projects[ref]
	return map[string]any{
		"ref": ref, "name": p.name, "status": p.status, "organization_slug": p.org,
		"database": map[string]string{"host": p.host},
	}
}

func (f *fakeAPI) listLocked() []map[string]any {
	refs := slices.Sorted(maps.Keys(f.projects))
	out := make([]map[string]any, len(refs))
	for i, ref := range refs {
		out[i] = f.apiProjectLocked(ref)
	}
	return out
}

// add registers a project that exists before the test starts.
func (f *fakeAPI) add(ref, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.projects[ref] = &fakeProject{name: name, org: "org-123", status: "ACTIVE_HEALTHY", host: "db." + ref + ".supabase.red"}
}

// count returns how many requests matched method and path prefix.
func (f *fakeAPI) count(method, pathPrefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, method+" "+pathPrefix) {
			n++
		}
	}
	return n
}

// deleted returns the refs DELETE was called for, sorted.
func (f *fakeAPI) deleted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if ref, ok := strings.CutPrefix(c, "DELETE /v1/projects/"); ok {
			out = append(out, ref)
		}
	}
	slices.Sort(out)
	return out
}

func (f *fakeAPI) provider() *Provider {
	return &Provider{
		out: io.Discard, doer: f.srv.Client(),
		cfg:          &config.Config{Supabase: config.SupabaseConfig{AccessToken: "tok"}},
		pollInterval: time.Millisecond, recoverLookupInterval: time.Nanosecond, deleteRetryInterval: time.Nanosecond,
	}
}

// config is a Provision config against the fake, with kv applied on top.
func (f *fakeAPI) config(kv ...string) map[string]any {
	cfg := map[string]any{
		"region": "us-east-1", "org_id": "org-123", "project_size": "large", "connection_mode": "direct",
		"management_api": "true", "management_api_url": f.srv.URL,
	}
	for i := 0; i < len(kv); i += 2 {
		cfg[kv[i]] = kv[i+1]
	}
	return cfg
}

// outputs is a teardown's outputs for the fake.
func (f *fakeAPI) outputs(kv ...string) engine.Outputs {
	out := engine.Outputs{outputKeyManagementAPI: "true", outputKeyManagementAPIURL: f.srv.URL}
	for i := 0; i < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

// testClient is a client that tests point at their own server.
func testClient(doer httpDoer) *mgmtClient {
	return &mgmtClient{out: io.Discard, doer: doer, base: "https://api.supabase.com", token: "test-token", block: &block{}}
}

func runManifest(t *testing.T, runID string) []manifestEvent {
	t.Helper()
	events, err := readManifest(filepath.Join(workDirForRun(runID), manifestFile))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	return events
}

// writeRunManifest writes a run's manifest as Provision would have left it.
func writeRunManifest(t *testing.T, runID string, events ...manifestEvent) {
	t.Helper()
	if err := os.MkdirAll(workDirForRun(runID), 0o755); err != nil {
		t.Fatal(err)
	}
	m, err := openManifest(workDirForRun(runID))
	if err != nil {
		t.Fatal(err)
	}
	defer m.close()
	for _, ev := range events {
		if err := m.write(ev); err != nil {
			t.Fatal(err)
		}
	}
}

// --- mgmtClient ---

func TestDoThrottled_RetriesAfterRateLimitReset(t *testing.T) {
	var calls atomic.Int32
	throttleFirst := func(n int32) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) <= n {
				w.Header().Set("X-RateLimit-Reset", "0.05")
				w.WriteHeader(http.StatusTooManyRequests)
				fmt.Fprint(w, `{"message":"ThrottlerException: Too Many Requests"}`)
				return
			}
			fmt.Fprint(w, `{}`)
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	t.Run("succeeds once the block lifts", func(t *testing.T) {
		calls.Store(0)
		srv := throttleFirst(2)
		c := testClient(srv.Client())
		c.base = srv.URL
		start := time.Now()
		if _, err := c.doThrottled(context.Background(), http.MethodDelete, "/v1/projects/a", nil); err != nil {
			t.Fatalf("doThrottled: %v", err)
		}
		if calls.Load() != 3 {
			t.Errorf("made %d calls, want 3", calls.Load())
		}
		if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
			t.Errorf("retried after %s, want at least two 50ms pauses", elapsed)
		}
	})

	t.Run("keeps retrying until ctx ends", func(t *testing.T) {
		calls.Store(0)
		srv := throttleFirst(1 << 30)
		c := testClient(srv.Client())
		c.base = srv.URL
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		defer cancel()
		_, err := c.doThrottled(ctx, http.MethodDelete, "/v1/projects/a", nil)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v, want the context deadline", err)
		}
		if calls.Load() < 5 {
			t.Errorf("made %d calls, want more than the old cap of 5", calls.Load())
		}
	})
}

func TestRateLimitReset(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   time.Duration
		ok     bool
	}{
		{"", 0, false}, // not the throttler: the endpoint refused the request
		{"2", 2 * time.Second, true},
		{"0.05", 50 * time.Millisecond, true},
		{"0", minRateLimitWait, true}, // never retry in a tight loop
		{"soon", defaultRateLimitWait, true},
	} {
		h := http.Header{}
		if tc.header != "" {
			h.Set("X-RateLimit-Reset", tc.header)
		}
		if got, ok := rateLimitReset(h); got != tc.want || ok != tc.ok {
			t.Errorf("rateLimitReset(%q) = %s, %v; want %s, %v", tc.header, got, ok, tc.want, tc.ok)
		}
	}
}

func TestDoThrottled_ReturnsAnEndpoints429(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// The disk endpoint's own cooldown: a 429 with no X-RateLimit headers.
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"message":"Database disk can only be modified once per four hours."}`)
	}))
	t.Cleanup(srv.Close)
	c := testClient(srv.Client())
	c.base = srv.URL

	_, err := c.doThrottled(context.Background(), http.MethodPost, "/v1/projects/a/config/disk", nil)
	if !hasStatus(err, http.StatusTooManyRequests) || !strings.Contains(err.Error(), "once per four hours") {
		t.Fatalf("got %v, want the endpoint's 429 returned", err)
	}
	if calls.Load() != 1 {
		t.Errorf("made %d calls, want 1: the request must not be retried", calls.Load())
	}
	if c.block.hold(0) == false {
		// hold(0) only extends a block that has already lifted, so true here
		// means the 429 did not block the other calls.
		t.Error("the endpoint's 429 blocked every throttled call")
	}
}

// alwaysThrottled is a server whose throttler never lifts its block.
func alwaysThrottled(t *testing.T) *mgmtClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Reset", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	c := testClient(srv.Client())
	c.base = srv.URL
	return c
}

func TestPolling_DeadlinesCoverThrottling(t *testing.T) {
	p := mgmt{&Provider{out: io.Discard, pollInterval: time.Millisecond, pollTimeout: 200 * time.Millisecond,
		diskResizePollInterval: time.Millisecond, diskResizePollTimeout: 200 * time.Millisecond}}
	within := func(t *testing.T, f func() error) error {
		t.Helper()
		start := time.Now()
		err := f()
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("returned after %s, want soon after the 200ms deadline", elapsed)
		}
		return err
	}

	t.Run("waitForProject", func(t *testing.T) {
		err := within(t, func() error {
			_, err := p.waitForProject(context.Background(), alwaysThrottled(t), "abc", 200*time.Millisecond)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "project abc was still not listed yet after 200ms") {
			t.Errorf("got %v, want the ready timeout", err)
		}
	})

	t.Run("publishableKey", func(t *testing.T) {
		err := within(t, func() error {
			_, err := p.publishableKey(context.Background(), alwaysThrottled(t), "abc")
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "after 200ms") {
			t.Errorf("got %v, want the key timeout", err)
		}
	})

	t.Run("waitForDiskResize", func(t *testing.T) {
		disk := throttledDisk{client: alwaysThrottled(t), path: "/v1/projects/abc/config/disk"}
		err := within(t, func() error { return p.waitForDiskResize(context.Background(), disk, 16, 3000, "gp3") })
		if err == nil || !strings.Contains(err.Error(), "timed out waiting") {
			t.Errorf("got %v, want the resize timeout", err)
		}
	})

	t.Run("the run ending is not reported as a timeout", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err := p.waitForProject(ctx, alwaysThrottled(t), "abc", time.Hour)
		if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "was still") {
			t.Errorf("got %v, want the run's own cancellation", err)
		}
	})
}

func TestThrottledDisk_ResizesThroughTheClient(t *testing.T) {
	var mu sync.Mutex
	var noSkip []string
	size, gets := 8, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get(skipThrottlingHeader) != "skip-me" {
			noSkip = append(noSkip, r.Method+" "+r.URL.Path)
		}
		switch r.Method {
		case http.MethodGet:
			if gets++; gets == 1 {
				// The first read is throttled; the resize has to wait it out.
				w.Header().Set("X-RateLimit-Reset", "0.02")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			fmt.Fprintf(w, `{"attributes":{"type":"gp3","size_gb":%d,"iops":3000}}`, size)
		case http.MethodPost:
			size = 16
			w.WriteHeader(http.StatusCreated)
		}
	}))
	t.Cleanup(srv.Close)
	c := testClient(srv.Client())
	c.base, c.skipToken = srv.URL, "skip-me"

	p := &Provider{out: io.Discard, diskResizePollInterval: time.Millisecond}
	disk := throttledDisk{client: c, path: "/v1/projects/abc/config/disk"}
	if err := p.resizeDiskVia(context.Background(), disk, 16, 0, 0, ""); err != nil {
		t.Fatalf("resizeDiskVia: %v", err)
	}
	if gets < 3 {
		t.Errorf("read the disk %d times, want the throttled read retried, then one read to check and one to verify", gets)
	}
	if len(noSkip) > 0 {
		t.Errorf("requests sent without the skip-throttling token: %v", noSkip)
	}
}

func TestAPIErrorMessage(t *testing.T) {
	if got := APIErrorMessage([]byte(`{"message":"Project not found"}`), "404 Not Found"); got != "Project not found" {
		t.Errorf("got %q", got)
	}
	if got := APIErrorMessage([]byte(`nope`), "500 Internal Server Error"); got != "500 Internal Server Error" {
		t.Errorf("got %q, want the status fallback", got)
	}
}

func TestParseConfig_ManagementAPIURL(t *testing.T) {
	base := map[string]any{"region": "r", "org_id": "o", "connection_mode": "direct", "management_api": "true"}
	with := func(kv ...string) map[string]any {
		cfg := maps.Clone(base)
		for i := 0; i < len(kv); i += 2 {
			cfg[kv[i]] = kv[i+1]
		}
		return cfg
	}
	for _, tc := range []struct {
		cfg     map[string]any
		want    string
		wantErr bool
	}{
		{cfg: with(), want: "https://api.supabase.com"},
		{cfg: with("profile", "supabase-local"), want: "http://localhost:8080"},
		{cfg: with("profile", "supabase-local", "management_api_url", "https://x.ngrok.app/"), want: "https://x.ngrok.app"},
		// An unknown profile must never fall through to production.
		{cfg: with("profile", "/path/to/profile.yaml"), wantErr: true},
	} {
		c, err := parseConfig(tc.cfg)
		if (err != nil) != tc.wantErr || c.ManagementAPIURL != tc.want {
			t.Errorf("parseConfig(%v) = %q, %v; want %q, error %v", tc.cfg, c.ManagementAPIURL, err, tc.want, tc.wantErr)
		}
	}
	if _, err := parseConfig(with("max_failed_projects", "-1")); err == nil {
		t.Error("expected an error for a negative max_failed_projects")
	}
}

// --- Provision ---

func TestMgmtProvision_Outputs(t *testing.T) {
	t.Run("one project keeps today's outputs", func(t *testing.T) {
		f := newFakeAPI(t)
		outputs, err := f.provider().Provision(context.Background(), "run-1", f.config())
		if err != nil {
			t.Fatalf("Provision() error = %v", err)
		}
		want := engine.Outputs{
			outputKeyProjectRefs:                  "ref-run-1",
			outputKeyProjectRef:                   "ref-run-1",
			"host":                                "db.ref-run-1.supabase.red",
			"port":                                "5432",
			"user":                                "postgres",
			"password":                            outputs["password"],
			"db":                                  "postgres",
			"sslmode":                             "require",
			"sslnegotiation":                      "direct",
			outputKeyVendor:                       "supabase",
			outputKeyAPIURL + "ref-run-1":         "https://ref-run-1.supabase.red",
			outputKeyPublishableKey + "ref-run-1": "pub-ref-run-1",
			outputKeyProfile:                      "",
			outputKeyManagementAPI:                "true",
			outputKeyManagementAPIURL:             f.srv.URL,
			outputKeyOrgID:                        "org-123",
			outputKeyRunID:                        "run-1",
			outputKeyProjectCount:                 "1",
		}
		if len(outputs["password"]) != 48 || !maps.Equal(outputs, want) {
			t.Errorf("outputs = %v, want %v", outputs, want)
		}

		// organization_id would land the create in the delete bucket.
		f.mu.Lock()
		create := f.creates[0]
		f.mu.Unlock()
		var body map[string]any
		_ = json.Unmarshal([]byte(create), &body)
		if _, ok := body["organization_id"]; ok || body["organization_slug"] != "org-123" {
			t.Errorf("create body = %s, want organization_slug only", create)
		}

		// The creating line comes first, so a crash can't leak a project.
		want2 := []manifestEvent{
			{E: "creating", Name: "run-1"},
			{E: "created", Name: "run-1", Ref: "ref-run-1"},
			{E: "ready", Ref: "ref-run-1", APIURL: "https://ref-run-1.supabase.red", Key: "pub-ref-run-1"},
		}
		if got := runManifest(t, "run-1"); !slices.Equal(got, want2) {
			t.Errorf("manifest = %v, want %v", got, want2)
		}
	})

	t.Run("several projects are listed in the manifest", func(t *testing.T) {
		f := newFakeAPI(t)
		outputs, err := f.provider().Provision(context.Background(), "run-1", f.config("project_count", "3"))
		if err != nil {
			t.Fatalf("Provision() error = %v", err)
		}
		if outputs[outputKeyProjectsManifest] != filepath.Join(workDirForRun("run-1"), manifestFile) || outputs[outputKeyProjectsReady] != "3" {
			t.Errorf("outputs = %v, want the manifest and the ready count", outputs)
		}
		if outputs[engine.OutputKeyRunFilesDir] != workDirForRun("run-1") {
			t.Errorf("run files dir = %q, want the directory that holds the manifest, so it travels with the run", outputs[engine.OutputKeyRunFilesDir])
		}
		// Per-project keys would outgrow the workload's argv at scale.
		for key := range outputs {
			if key == outputKeyProjectRefs || key == "host" || key == outputKeyProjectRef ||
				strings.HasPrefix(key, outputKeyAPIURL) || strings.HasPrefix(key, outputKeyPublishableKey) {
				t.Errorf("outputs[%s] is set, want it unset for several projects", key)
			}
		}
		if got := len(runManifest(t, "run-1")); got != 9 {
			t.Errorf("manifest has %d lines, want creating, created and ready per project", got)
		}
		if info, err := os.Stat(outputs[outputKeyProjectsManifest]); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("manifest stat = %v, %v; want mode 0600", info, err)
		}
	})
}

func TestMgmtProvision_PublishableKeyIsPolledFor(t *testing.T) {
	f := newFakeAPI(t)
	var keyCalls atomic.Int32
	f.override = func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if !strings.HasSuffix(r.URL.Path, "/api-keys") {
			return false
		}
		if keyCalls.Add(1) == 1 {
			fmt.Fprint(w, `[{"name":"anon","type":"legacy","api_key":"anon"}]`)
			return true
		}
		return false
	}
	outputs, err := f.provider().Provision(context.Background(), "run-1", f.config())
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if outputs[outputKeyPublishableKey+"ref-run-1"] != "pub-ref-run-1" || keyCalls.Load() != 2 {
		t.Errorf("key = %q after %d calls, want pub-ref-run-1 after 2", outputs[outputKeyPublishableKey+"ref-run-1"], keyCalls.Load())
	}
}

// A failed project is left for teardown: deleting it inline would block on
// the API's refusal to delete a project that is still coming up.
func TestMgmtProvision_Failures(t *testing.T) {

	t.Run("a project that never gets healthy fails", func(t *testing.T) {
		f := newFakeAPI(t)
		f.override = func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
			if r.Method == http.MethodGet && r.URL.Path == "/v1/projects/ref-run-1" {
				fmt.Fprint(w, `{"ref":"ref-run-1","status":"COMING_UP","database":{"host":""}}`)
				return true
			}
			return false
		}
		outputs, err := f.provider().Provision(context.Background(), "run-1", f.config("ready_timeout", "20ms"))
		if err == nil || !strings.Contains(err.Error(), "still COMING_UP after 20ms") {
			t.Fatalf("got %v, want a still-COMING_UP error naming the timeout", err)
		}
		want := engine.Outputs{
			outputKeyProjectRefs: "ref-run-1", outputKeyProfile: "", outputKeyManagementAPI: "true", outputKeyManagementAPIURL: f.srv.URL,
			outputKeyOrgID: "org-123", outputKeyRunID: "run-1", outputKeyProjectCount: "1",
		}
		if !maps.Equal(outputs, want) || len(f.deleted()) != 0 {
			t.Errorf("outputs = %v, deleted %v; want %v and no delete", outputs, f.deleted(), want)
		}
	})

	// failSecond rejects run-1-2's create with a 402, but only once another
	// project exists. A failure that won the race outright would cancel the
	// other creates before they made anything, which is correct but leaves
	// these tests nothing to check.
	failSecond := func(f *fakeAPI) {
		f.override = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
			if r.Method == http.MethodPost && strings.Contains(string(body), `"run-1-2"`) {
				for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
					f.mu.Lock()
					n := len(f.projects)
					f.mu.Unlock()
					if n > 0 {
						break
					}
				}
				w.WriteHeader(http.StatusPaymentRequired)
				fmt.Fprint(w, `{"message":"payment required"}`)
				return true
			}
			return false
		}
	}

	t.Run("a project that reports INIT_FAILED fails at once", func(t *testing.T) {
		f := newFakeAPI(t)
		f.override = func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
			if r.Method == http.MethodGet && r.URL.Path == "/v1/projects/ref-run-1" {
				fmt.Fprint(w, `{"ref":"ref-run-1","status":"INIT_FAILED","database":{"host":""}}`)
				return true
			}
			return false
		}
		_, err := f.provider().Provision(context.Background(), "run-1", f.config())
		if err == nil || !strings.Contains(err.Error(), "reached status INIT_FAILED") {
			t.Fatalf("got %v, want a failure naming INIT_FAILED", err)
		}
		if n := f.count(http.MethodGet, "/v1/projects/ref-run-1"); n != 1 {
			t.Errorf("polled %d times, want 1: a failed status is not waited out", n)
		}
	})

	t.Run("the first failure cancels the run by default", func(t *testing.T) {
		f := newFakeAPI(t)
		failSecond(f)
		p := f.provider()
		// A create cancelled in flight can still reach the fake; give the
		// lookup that recovers it time to see it.
		p.recoverLookupInterval = 100 * time.Millisecond
		outputs, err := p.Provision(context.Background(), "run-1", f.config("project_count", "3"))
		if err == nil || !strings.Contains(err.Error(), "create project run-1-2: HTTP 402: payment required") {
			t.Fatalf("got %v, want the failed create", err)
		}
		// However the race plays out, every project created is left for
		// teardown.
		f.mu.Lock()
		created := slices.Sorted(maps.Keys(f.projects))
		f.mu.Unlock()
		events, err := readManifest(outputs[outputKeyProjectsManifest])
		if err != nil {
			t.Fatalf("read manifest: %v", err)
		}
		if left := undeletedRefs(events); !slices.Equal(slices.Sorted(slices.Values(left)), created) {
			t.Errorf("left for teardown %v, want every created project %v", left, created)
		}
		if orphans := orphanNames(events); slices.Contains(orphans, "run-1-2") {
			t.Errorf("orphan names %v include run-1-2, whose create the API rejected", orphans)
		}
		if !slices.Contains(events, manifestEvent{E: eventRejected, Name: "run-1-2"}) {
			t.Errorf("manifest %v has no rejected event for run-1-2", events)
		}
		if len(f.deleted()) != 0 {
			t.Errorf("deleted %v, want none", f.deleted())
		}
	})

	t.Run("max_failed_projects tolerates failures", func(t *testing.T) {
		f := newFakeAPI(t)
		failSecond(f)
		outputs, err := f.provider().Provision(context.Background(), "run-1", f.config("project_count", "3", "max_failed_projects", "1"))
		if err != nil {
			t.Fatalf("Provision() error = %v", err)
		}
		if got := outputs[outputKeyProjectsReady]; got != "2" {
			t.Errorf("projects_ready = %q, want 2", got)
		}
		got, err := TargetProjects(outputs)
		slices.SortFunc(got, func(a, b TargetProject) int { return strings.Compare(a.Ref, b.Ref) })
		if want := []TargetProject{{Ref: "ref-run-1-1", DBHost: "db.ref-run-1-1.supabase.red"}, {Ref: "ref-run-1-3", DBHost: "db.ref-run-1-3.supabase.red"}}; err != nil || !slices.Equal(got, want) {
			t.Errorf("TargetProjects() = %v, %v; want %v", got, err, want)
		}
	})
}

// A create can fail after the project exists. Provision must find that
// project so teardown deletes it, rather than report the failure and walk
// away.
func TestMgmtProvision_CreateFailsButTheProjectExists(t *testing.T) {
	t.Run("name already taken", func(t *testing.T) {
		f := newFakeAPI(t)
		f.add("proj123", "run-1")
		f.override = func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"message":"Project with name \"run-1\" already exists in your organization."}`)
				return true
			}
			return false
		}
		outputs, err := f.provider().Provision(context.Background(), "run-1", f.config())
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("got %v, want the create error", err)
		}
		if outputs[outputKeyProjectRefs] != "proj123" {
			t.Errorf("outputs = %v, want proj123 left for teardown", outputs)
		}
	})

	t.Run("response lost, listed only on a retry", func(t *testing.T) {
		f := newFakeAPI(t)
		var lists atomic.Int32
		f.override = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
			switch {
			case r.Method == http.MethodPost:
				f.add("ref-run-1", "run-1")
				fmt.Fprint(w, `{"status":"UNKNOWN"}`)
				return true
			case r.Method == http.MethodGet && r.URL.Path == "/v1/projects":
				if lists.Add(1) == 1 {
					fmt.Fprint(w, `[]`)
					return true
				}
			}
			return false
		}
		outputs, err := f.provider().Provision(context.Background(), "run-1", f.config())
		if err == nil || !strings.Contains(err.Error(), "parse project ref") {
			t.Fatalf("got %v, want a parse error", err)
		}
		if outputs[outputKeyProjectRefs] != "ref-run-1" || lists.Load() != 2 {
			t.Errorf("outputs = %v after %d lists, want ref-run-1 after 2", outputs, lists.Load())
		}
	})
}

// --- Teardown ---

func TestMgmtTeardown_SendsTheRunsSkipToken(t *testing.T) {
	f := newFakeAPI(t)
	var mu sync.Mutex
	var noSkip []string
	f.override = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.Header.Get(skipThrottlingHeader) != "skip-me" {
			mu.Lock()
			noSkip = append(noSkip, r.Method+" "+r.URL.Path)
			mu.Unlock()
		}
		return false
	}
	p := f.provider()
	outputs, err := p.Provision(context.Background(), "run-1", f.config("throttle_skip_token", "skip-me"))
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if err := p.Teardown(context.Background(), outputs); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(f.deleted()) != 1 {
		t.Fatalf("deleted %v, want the one project", f.deleted())
	}
	if len(noSkip) > 0 {
		t.Errorf("requests sent without the skip-throttling token: %v", noSkip)
	}
}

func TestMgmtTeardown(t *testing.T) {
	t.Run("a project no longer listed is not deleted", func(t *testing.T) {
		f := newFakeAPI(t)
		f.add("other", "x")
		if err := f.provider().Teardown(context.Background(), f.outputs(outputKeyProjectRef, "proj123")); err != nil {
			t.Fatalf("Teardown: %v", err)
		}
		if len(f.deleted()) != 0 {
			t.Errorf("deleted %v, want none", f.deleted())
		}
	})

	t.Run("a failed delete is reported and doesn't stop the rest", func(t *testing.T) {
		f := newFakeAPI(t)
		for _, ref := range []string{"a", "b", "c"} {
			f.add(ref, ref)
		}
		f.override = func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
			if r.Method == http.MethodDelete && r.URL.Path == "/v1/projects/b" {
				w.WriteHeader(http.StatusInternalServerError)
				return true
			}
			return false
		}
		err := f.provider().Teardown(context.Background(), f.outputs(outputKeyProjectRefs, "a,b,c"))
		if err == nil || !strings.Contains(err.Error(), "delete project b") {
			t.Fatalf("got %v, want the failed delete", err)
		}
		if got := f.deleted(); !slices.Equal(got, []string{"a", "b", "c"}) {
			t.Errorf("deleted %v, want a, b and c", got)
		}
	})

	t.Run("finds unrecorded projects by name", func(t *testing.T) {
		f := newFakeAPI(t)
		f.add("ref-1", "run-1-1")
		f.add("ref-2", "run-1-2")
		f.add("ref-other", "run-1-3")
		f.add("ref-unrelated", "someone-else")
		f.add("ref-other-org", "run-1-1")
		f.mu.Lock()
		f.projects["ref-other-org"].org = "org-999"
		f.mu.Unlock()
		seed := mgmtRefOutputs(providerConfig{ProjectCount: 2, ManagementAPIURL: f.srv.URL, OrgID: "org-123"}, "run-1", nil)
		if err := f.provider().Teardown(context.Background(), seed); err != nil {
			t.Fatalf("Teardown: %v", err)
		}
		if got := f.deleted(); !slices.Equal(got, []string{"ref-1", "ref-2"}) {
			t.Errorf("deleted %v, want only the run's projects in its org", got)
		}
	})
}

func TestMgmtTeardown_FromManifest(t *testing.T) {
	created := func(refs ...string) []manifestEvent {
		var evs []manifestEvent
		for _, ref := range refs {
			evs = append(evs,
				manifestEvent{E: eventCreating, Name: "n-" + ref},
				manifestEvent{E: eventCreated, Name: "n-" + ref, Ref: ref})
		}
		return evs
	}
	seed := func(f *fakeAPI) engine.Outputs {
		return mgmtRefOutputs(providerConfig{ProjectCount: 3, ManagementAPIURL: f.srv.URL, OrgID: "org-123"}, "run-1", nil)
	}

	t.Run("skips deleted refs and removes the workdir when done", func(t *testing.T) {
		f := newFakeAPI(t)
		for _, ref := range []string{"a", "b", "c"} {
			f.add(ref, "n-"+ref)
		}
		writeRunManifest(t, "run-1", append(created("a", "b", "c"), manifestEvent{E: eventDeleted, Ref: "b"})...)
		if err := f.provider().Teardown(context.Background(), seed(f)); err != nil {
			t.Fatalf("Teardown: %v", err)
		}
		if got := f.deleted(); !slices.Equal(got, []string{"a", "c"}) {
			t.Errorf("deleted %v, want a and c", got)
		}
		// The manifest is the record: no project list is fetched.
		if n := f.count(http.MethodGet, "/v1/projects"); n != 0 {
			t.Errorf("made %d GETs, want none", n)
		}
		if _, err := os.Stat(workDirForRun("run-1")); !os.IsNotExist(err) {
			t.Errorf("workdir still there: %v", err)
		}
	})

	t.Run("a failure keeps the manifest, with deleted lines, for a rerun", func(t *testing.T) {
		f := newFakeAPI(t)
		for _, ref := range []string{"a", "b"} {
			f.add(ref, "n-"+ref)
		}
		writeRunManifest(t, "run-1", created("a", "b")...)
		f.override = func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
			if r.Method == http.MethodDelete && r.URL.Path == "/v1/projects/b" {
				w.WriteHeader(http.StatusInternalServerError)
				return true
			}
			return false
		}
		if err := f.provider().Teardown(context.Background(), seed(f)); err == nil || !strings.Contains(err.Error(), "delete project b") {
			t.Fatalf("got %v, want the failed delete", err)
		}
		if left := undeletedRefs(runManifest(t, "run-1")); !slices.Equal(left, []string{"b"}) {
			t.Errorf("left %v in the manifest, want b", left)
		}
	})

	t.Run("not ready goes to the back of the queue", func(t *testing.T) {
		f := newFakeAPI(t)
		var refs []string
		n := 3 * defaultMaxParallelProjects
		for i := range n {
			ref := fmt.Sprintf("r%02d", i)
			refs = append(refs, ref)
			f.add(ref, "n-"+ref)
		}
		writeRunManifest(t, "run-1", created(refs...)...)
		var firstTry atomic.Bool
		firstTry.Store(true)
		f.override = func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
			if r.Method == http.MethodDelete && r.URL.Path == "/v1/projects/r00" && firstTry.Swap(false) {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"message":"Project not ready for deletion - please try again in 14 minutes."}`)
				return true
			}
			return false
		}
		if err := f.provider().Teardown(context.Background(), seed(f)); err != nil {
			t.Fatalf("Teardown: %v", err)
		}
		f.mu.Lock()
		calls := slices.Clone(f.calls)
		f.mu.Unlock()
		// By the time the retry is taken off the queue, every other ref has
		// been, and all but the other workers' have been deleted.
		var tries, before int
		for _, c := range calls {
			if c == "DELETE /v1/projects/r00" {
				if tries++; tries == 2 {
					break
				}
			} else if strings.HasPrefix(c, http.MethodDelete) {
				before++
			}
		}
		if want := n - defaultMaxParallelProjects; tries != 2 || before < want {
			t.Errorf("r00 tried %d times, retried after %d other deletes, want at least %d: %v", tries, before, want, calls)
		}
	})

	t.Run("403 then 404 counts as deleted", func(t *testing.T) {
		f := newFakeAPI(t)
		f.add("a", "n-a")
		writeRunManifest(t, "run-1", append(created("a", "gone"), manifestEvent{E: eventReady, Ref: "a"})...)
		f.override = func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
			if r.URL.Path == "/v1/projects/gone" && r.Method == http.MethodDelete {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `{"message":"You do not have permission to perform this action"}`)
				return true
			}
			return false
		}
		if err := f.provider().Teardown(context.Background(), seed(f)); err != nil {
			t.Fatalf("Teardown: %v", err)
		}
		if n := f.count(http.MethodGet, "/v1/projects/gone"); n != 1 {
			t.Errorf("made %d GETs to confirm, want 1", n)
		}
	})

	t.Run("a create never recorded as created is found by name", func(t *testing.T) {
		f := newFakeAPI(t)
		f.add("a", "n-a")
		f.add("lost", "run-1-2") // created, but benchctl died before it saw the ref
		f.add("other", "someone-else")
		writeRunManifest(t, "run-1", append(created("a"), manifestEvent{E: eventCreating, Name: "run-1-2"})...)
		if err := f.provider().Teardown(context.Background(), seed(f)); err != nil {
			t.Fatalf("Teardown: %v", err)
		}
		if got := f.deleted(); !slices.Equal(got, []string{"a", "lost"}) {
			t.Errorf("deleted %v, want a and lost", got)
		}
		if n := f.count(http.MethodGet, "/v1/projects"); n != 1 {
			t.Errorf("made %d lists, want 1 for all the unrecorded names", n)
		}
	})

	t.Run("a create the API rejected is not looked up", func(t *testing.T) {
		f := newFakeAPI(t)
		f.add("a", "n-a")
		writeRunManifest(t, "run-1", append(created("a"),
			manifestEvent{E: eventCreating, Name: "run-1-2"}, manifestEvent{E: eventRejected, Name: "run-1-2"})...)
		if err := f.provider().Teardown(context.Background(), seed(f)); err != nil {
			t.Fatalf("Teardown: %v", err)
		}
		if n := f.count(http.MethodGet, "/v1/projects"); n != 0 {
			t.Errorf("made %d lists, want none: no create has an unknown outcome", n)
		}
	})

	t.Run("a project listed late is found on a later look", func(t *testing.T) {
		f := newFakeAPI(t)
		f.add("lost", "run-1-2")
		var lists atomic.Int32
		f.override = func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
			if r.Method == http.MethodGet && r.URL.Path == "/v1/projects" && lists.Add(1) == 1 {
				fmt.Fprint(w, `[]`) // not listed yet
				return true
			}
			return false
		}
		writeRunManifest(t, "run-1", manifestEvent{E: eventCreating, Name: "run-1-2"})
		if err := f.provider().Teardown(context.Background(), seed(f)); err != nil {
			t.Fatalf("Teardown: %v", err)
		}
		if got := f.deleted(); !slices.Equal(got, []string{"lost"}) {
			t.Errorf("deleted %v, want the project listed on the second look", got)
		}
		if n := lists.Load(); n != 2 {
			t.Errorf("made %d lists, want 2", n)
		}
	})

	t.Run("a name never found is reported", func(t *testing.T) {
		f := newFakeAPI(t)
		writeRunManifest(t, "run-1", manifestEvent{E: eventCreating, Name: "run-1-2"})
		var out bytes.Buffer
		p := f.provider()
		p.out = &out
		if err := p.Teardown(context.Background(), seed(f)); err != nil {
			t.Fatalf("Teardown: %v", err)
		}
		if n := f.count(http.MethodGet, "/v1/projects"); n != recoverLookupAttempts {
			t.Errorf("made %d lists, want %d", n, recoverLookupAttempts)
		}
		if !strings.Contains(out.String(), "run-1-2") {
			t.Errorf("output %q does not name the create it could not find", out.String())
		}
	})
}

func TestMgmtTargetProjects_FromManifest(t *testing.T) {
	path := filepath.Join(t.TempDir(), manifestFile)
	m, err := openManifest(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range []manifestEvent{
		{E: eventCreated, Name: "r-1", Ref: "a"},
		{E: eventCreated, Name: "r-2", Ref: "b"},
		{E: eventReady, Ref: "b", APIURL: "https://b.supabase.red"},
	} {
		if err := m.write(ev); err != nil {
			t.Fatal(err)
		}
	}
	m.close()
	got, err := TargetProjects(engine.Outputs{outputKeyProjectsManifest: path})
	if want := []TargetProject{{Ref: "b", DBHost: "db.b.supabase.red"}}; err != nil || !slices.Equal(got, want) {
		t.Errorf("TargetProjects() = %v, %v, want %v", got, err, want)
	}
}

func TestMgmtTeardownSeed(t *testing.T) {
	p := &Provider{}
	seed := p.TeardownSeed("run-1", map[string]any{
		"region": "eu-central-1", "org_id": "org", "connection_mode": "direct", "project_count": "3",
		"profile": "supabase-local", "management_api": "true",
	})
	if got := runProjectNames(seed); !slices.Equal(got, []string{"run-1-1", "run-1-2", "run-1-3"}) {
		t.Errorf("seed names %v", got)
	}
	if seed[outputKeyManagementAPI] != "true" || seed[outputKeyManagementAPIURL] != "http://localhost:8080" || seed[outputKeyOrgID] != "org" {
		t.Errorf("seed = %v, want management_api, management_api_url and org_id", seed)
	}
}

// --- dispatch ---

// errNoCLI stands in for the CLI path: the CLI path checks for the binary
// first, so seeing this error proves it was taken, and no binary runs.
var errNoCLI = errors.New("cli path taken")

func TestProvision_ChoosesThePathByManagementAPI(t *testing.T) {
	for _, tc := range []struct {
		name   string
		flag   any
		viaAPI bool
	}{
		{"unset", nil, false},
		{"false", "false", false},
		{"bool true", true, true},
		{"string true", "true", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAPI(t)
			p := f.provider()
			p.checkCLI = func() error { return errNoCLI }
			cfg := map[string]any{"region": "r", "org_id": "org-123", "connection_mode": "direct"}
			if tc.viaAPI {
				cfg["management_api"], cfg["management_api_url"] = tc.flag, f.srv.URL
			}
			_, err := p.Provision(context.Background(), "run-1", cfg)
			if tc.viaAPI != (err == nil) || (!tc.viaAPI && !errors.Is(err, errNoCLI)) {
				t.Fatalf("Provision() error = %v", err)
			}
			if got := f.count(http.MethodPost, "/v1/projects"); (got == 1) != tc.viaAPI {
				t.Errorf("%d creates through the API, want it used: %v", got, tc.viaAPI)
			}
		})
	}
}

// A key only the Management API path reads must not be dropped quietly.
func TestParseConfig_ManagementAPIKeysNeedTheFlag(t *testing.T) {
	base := map[string]any{"region": "r", "org_id": "o", "connection_mode": "direct"}
	for _, key := range mgmtOnlyKeys {
		cfg := maps.Clone(base)
		cfg[key] = "1"
		if _, err := parseConfig(cfg); err == nil || !strings.Contains(err.Error(), "management_api: true") {
			t.Errorf("config.%s without the flag: error = %v, want one naming management_api", key, err)
		}
		cfg[key] = "" // a scenario that always passes the key, empty
		if _, err := parseConfig(cfg); err != nil {
			t.Errorf("config.%s empty: %v", key, err)
		}
	}
}

func TestTeardown_ChoosesThePathByTheSeed(t *testing.T) {
	f := newFakeAPI(t)
	f.add("proj123", "run-1")
	p := f.provider()
	p.checkCLI = func() error { return errNoCLI }

	// A run from before the key existed goes through the CLI.
	if err := p.Teardown(context.Background(), engine.Outputs{outputKeyProjectRefs: "proj123"}); !errors.Is(err, errNoCLI) {
		t.Errorf("Teardown() without the key = %v, want the CLI path", err)
	}
	if err := p.Teardown(context.Background(), f.outputs(outputKeyProjectRefs, "proj123")); err != nil {
		t.Fatalf("Teardown() with the key: %v", err)
	}
	if got := f.deleted(); !slices.Equal(got, []string{"proj123"}) {
		t.Errorf("deleted %v through the API, want proj123", got)
	}
}

func TestReadManifest_SkipsAPartialLastLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), manifestFile)
	data := `{"e":"creating","name":"r-1"}` + "\n" + `{"e":"created","name":"r-1","ref":"a"}` + "\n" + `{"e":"crea`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	events, err := readManifest(path)
	if err != nil || len(events) != 2 {
		t.Errorf("readManifest() = %v, %v; want the 2 whole lines", events, err)
	}
}
