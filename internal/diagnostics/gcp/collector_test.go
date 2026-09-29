package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbarena/benchctl/internal/diagnostics"
)

var (
	winStart = time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	winEnd   = winStart.Add(time.Hour)
)

// fakeAPI serves canned Google API responses over httptest, so the request
// shapes are exercised through a real HTTP round trip. Handlers are keyed by
// a substring of the path; the request that reached each one is recorded.
type fakeAPI struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	requests []*recordedRequest
	// handler returns (status, body) for a request.
	handler func(r *recordedRequest) (int, string)
}

type recordedRequest struct {
	path         string
	query        url.Values
	body         map[string]any
	auth         string
	quotaProject string
}

func newFakeAPI(t *testing.T, handler func(*recordedRequest) (int, string)) *fakeAPI {
	f := &fakeAPI{t: t, handler: handler}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &recordedRequest{
			path:         r.URL.Path,
			query:        r.URL.Query(),
			auth:         r.Header.Get("Authorization"),
			quotaProject: r.Header.Get("x-goog-user-project"),
		}
		if r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&rec.body)
		}
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

func (f *fakeAPI) collector() *Collector {
	return &Collector{
		doer:           f.server.Client(),
		token:          func(context.Context) (string, error) { return "test-token", nil },
		quotaProject:   func() string { return "quota-proj" },
		monitoringHost: f.server.URL,
		loggingHost:    f.server.URL,
		sqlAdminHost:   f.server.URL,
		projectID:      "proj",
		instance:       "bench-pg",
		databaseID:     "proj:bench-pg",
		database:       "tpcc",
	}
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

func testRequest(t *testing.T) diagnostics.Request {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "diagnostics")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return diagnostics.Request{
		RunID:   "run-1",
		Outputs: map[string]string{"project_id": "proj", "instance_name": "bench-pg", "db": "tpcc"},
		Windows: []diagnostics.Window{{Name: "benchmark", Iteration: 1, Start: winStart, End: winEnd}},
		Dest:    dest,
	}
}

// oneSeries is a minimal timeSeries.list reply.
const oneSeries = `{"timeSeries":[{"metric":{"type":"x"},"points":[{"value":{"doubleValue":0.5}}]}]}`

func okHandler(r *recordedRequest) (int, string) {
	switch {
	case strings.Contains(r.path, "/timeSeries"):
		return 200, oneSeries
	case strings.Contains(r.path, "entries:list"):
		return 200, `{"entries":[{"timestamp":"2026-09-29T10:05:00Z","severity":"INFO","textPayload":"LOG:  checkpoint starting: time"}]}`
	case strings.Contains(r.path, "/operations"):
		return 200, `{"items":[]}`
	default: // instances.get
		return 200, `{"name":"bench-pg","databaseInstalledVersion":"POSTGRES_17_4"}`
	}
}

func TestCollect_WritesEverySource(t *testing.T) {
	f := newFakeAPI(t, okHandler)
	req := testRequest(t)

	res, err := f.collector().Collect(context.Background(), req)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", res.Warnings)
	}

	want := []string{"instance.json", "operations.json", "postgres.log", "benchmark_iter1/monitoring.json"}
	got := map[string]string{}
	for _, a := range res.Artifacts {
		got[a.File] = a.Command
	}
	for _, w := range want {
		if _, ok := got[w]; !ok {
			t.Errorf("missing artifact %s; got %v", w, res.Artifacts)
			continue
		}
		if _, err := os.Stat(filepath.Join(req.Dest, w)); err != nil {
			t.Errorf("%s recorded but not written: %v", w, err)
		}
	}
	// Every call has to be authenticated; an unauthenticated one would 401
	// against the real API.
	for _, r := range f.matching("") {
		if r.auth != "Bearer test-token" {
			t.Errorf("%s sent Authorization %q", r.path, r.auth)
		}
	}
}

// TestCollect_MonitoringRequestShape pins the query Cloud Monitoring is
// sensitive about: the aligner has to match the metric's kind, and the
// alignment period and resource filter decide whether anything comes back.
func TestCollect_MonitoringRequestShape(t *testing.T) {
	f := newFakeAPI(t, okHandler)
	req := testRequest(t)

	if _, err := f.collector().Collect(context.Background(), req); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	reqs := f.matching("/timeSeries")
	if len(reqs) != len(cloudSQLMetrics) {
		t.Fatalf("%d timeSeries requests, want one per metric (%d)", len(reqs), len(cloudSQLMetrics))
	}
	for _, r := range reqs {
		if got := r.query.Get("aggregation.alignmentPeriod"); got != alignmentPeriod {
			t.Errorf("alignmentPeriod = %q, want %q", got, alignmentPeriod)
		}
		if got := r.query.Get("interval.startTime"); got != "2026-09-29T10:00:00Z" {
			t.Errorf("interval.startTime = %q", got)
		}
		if got := r.query.Get("interval.endTime"); got != "2026-09-29T11:00:00Z" {
			t.Errorf("interval.endTime = %q", got)
		}
		filter := r.query.Get("filter")
		if !strings.Contains(filter, `resource.labels.database_id = "proj:bench-pg"`) {
			t.Errorf("filter does not scope to the instance: %s", filter)
		}
		if !strings.Contains(filter, `metric.type = "cloudsql.googleapis.com/database/`) {
			t.Errorf("filter has no Cloud SQL metric type: %s", filter)
		}
	}
}

// TestCloudSQLMetrics_AlignersAreValid guards the pairing Cloud Monitoring
// rejects outright: ALIGN_MEAN on a DELTA metric, or on a BOOL gauge
// ("the aligner cannot be applied to metrics with kind GAUGE and value type
// BOOL", which is how instance_state failed on a real run). The kinds and
// value types behind this table were read from the live metricDescriptors
// API.
func TestCloudSQLMetrics_AlignersAreValid(t *testing.T) {
	valid := map[string]bool{"ALIGN_MEAN": true, "ALIGN_RATE": true, "ALIGN_FRACTION_TRUE": true}
	seen := map[string]bool{}
	for _, m := range cloudSQLMetrics {
		if !valid[m.aligner] {
			t.Errorf("%s: aligner %q is not one of ALIGN_MEAN (numeric gauge), ALIGN_RATE (delta counter), ALIGN_FRACTION_TRUE (bool gauge)", m.suffix, m.aligner)
		}
		if seen[m.suffix] {
			t.Errorf("%s listed twice", m.suffix)
		}
		seen[m.suffix] = true
		// insights/* are CUMULATIVE DISTRIBUTION and need a percentile
		// reducer, not an aligner, and require Query Insights to be enabled.
		if strings.Contains(m.suffix, "insights/") {
			t.Errorf("%s needs a distribution reducer, not an aligner", m.suffix)
		}
	}
	// Rates are for counters, means for gauges: a "_count" metric read as a
	// mean would report an accumulating total rather than a rate.
	for _, m := range cloudSQLMetrics {
		if strings.HasSuffix(m.suffix, "_count") && m.aligner != "ALIGN_RATE" {
			t.Errorf("%s is a counter but is aligned %s", m.suffix, m.aligner)
		}
	}
}

// TestCollectMonitoring_PartialFailureKeepsTheRest covers one metric being
// unavailable: the other 30 still have to land.
func TestCollectMonitoring_PartialFailureKeepsTheRest(t *testing.T) {
	f := newFakeAPI(t, func(r *recordedRequest) (int, string) {
		if strings.Contains(r.query.Get("filter"), "deadlock_count") {
			return 403, `{"error":{"message":"Permission monitoring.timeSeries.list denied"}}`
		}
		return okHandler(r)
	})
	req := testRequest(t)
	var res diagnostics.Result

	f.collector().collectMonitoring(context.Background(), req, req.Windows[0], req.Dest, &res)

	if len(res.Artifacts) != 1 {
		t.Fatalf("artifacts = %+v, want monitoring.json despite one failed metric", res.Artifacts)
	}
	joined := strings.Join(res.Warnings, " ")
	if !strings.Contains(joined, "deadlock_count") {
		t.Errorf("the failed metric is not named: %v", res.Warnings)
	}

	data, err := os.ReadFile(filepath.Join(req.Dest, "monitoring.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(data, &merged); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(merged) != len(cloudSQLMetrics)-1 {
		t.Errorf("%d metrics in the artifact, want %d", len(merged), len(cloudSQLMetrics)-1)
	}
	if _, ok := merged["postgresql/deadlock_count"]; ok {
		t.Error("the failed metric was recorded anyway")
	}
}

// TestCollectMonitoring_EmptyExplainsTheDelay covers the tail gap: fetching
// immediately after a run can legitimately return nothing, and a bare empty
// file would look like a bug.
func TestCollectMonitoring_EmptyExplainsTheDelay(t *testing.T) {
	f := newFakeAPI(t, func(r *recordedRequest) (int, string) {
		if strings.Contains(r.path, "/timeSeries") {
			return 200, `{}`
		}
		return okHandler(r)
	})
	req := testRequest(t)
	var res diagnostics.Result

	f.collector().collectMonitoring(context.Background(), req, req.Windows[0], req.Dest, &res)

	if len(res.Artifacts) != 0 {
		t.Errorf("wrote an artifact with no data: %v", res.Artifacts)
	}
	joined := strings.Join(res.Warnings, " ")
	if !strings.Contains(joined, "165s") {
		t.Errorf("warning does not mention the publishing delay: %v", res.Warnings)
	}
}

// TestTimeSeries_FollowsPagination checks a metric split across pages is
// reassembled.
func TestTimeSeries_FollowsPagination(t *testing.T) {
	var calls int
	f := newFakeAPI(t, func(r *recordedRequest) (int, string) {
		if !strings.Contains(r.path, "/timeSeries") {
			return okHandler(r)
		}
		calls++
		if r.query.Get("pageToken") == "" {
			return 200, `{"timeSeries":[{"metric":{"type":"a"}}],"nextPageToken":"p2"}`
		}
		return 200, `{"timeSeries":[{"metric":{"type":"b"}}]}`
	})

	series, err := f.collector().timeSeries(context.Background(),
		"cloudsql.googleapis.com/database/cpu/utilization", "ALIGN_MEAN", false, winStart, winEnd)
	if err != nil {
		t.Fatalf("timeSeries: %v", err)
	}
	if len(series) != 2 {
		t.Errorf("%d series, want both pages", len(series))
	}
	if calls != 2 {
		t.Errorf("%d requests, want 2", calls)
	}
}

func TestPreflight(t *testing.T) {
	tests := []struct {
		name    string
		outputs map[string]string
		token   tokenSource
		wantIn  string
	}{
		{
			name:    "missing identifiers",
			outputs: map[string]string{"host": "10.1.2.3"},
			token:   func(context.Context) (string, error) { return "t", nil },
			wantIn:  "exports no Cloud SQL identifiers",
		},
		{
			name:    "no credentials",
			outputs: map[string]string{"project_id": "proj", "instance_name": "bench-pg"},
			token:   func(context.Context) (string, error) { return "", fmt.Errorf("reauthentication required") },
			wantIn:  "gcloud auth application-default login",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := New(nil)
			c.token = tt.token
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

func TestPreflight_ResolvesDatabaseID(t *testing.T) {
	c := New(nil)
	c.token = func(context.Context) (string, error) { return "t", nil }

	if err := c.Preflight(diagnostics.Request{Outputs: map[string]string{
		"project_id": "proj", "instance_name": "bench-pg",
	}}); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	// Monitoring and Logging both name the instance this way; getting it
	// wrong returns zero series with no error.
	if c.databaseID != "proj:bench-pg" {
		t.Errorf("databaseID = %q, want proj:bench-pg", c.databaseID)
	}
}

func TestAPIErrorMessage(t *testing.T) {
	got := apiErrorMessage([]byte(`{"error":{"code":403,"message":"Permission denied on resource"}}`), "403 Forbidden")
	if got != "Permission denied on resource" {
		t.Errorf("got %q, want the API message", got)
	}
	if got := apiErrorMessage([]byte(`not json`), "500 Internal Server Error"); got != "500 Internal Server Error" {
		t.Errorf("got %q, want the status fallback", got)
	}
}

// TestAPIErrorMessage_QuotaHint covers a rejection whose message names a
// Google-owned project number and is otherwise unactionable.
func TestAPIErrorMessage_QuotaHint(t *testing.T) {
	body := []byte(`{"error":{"message":"Quota exceeded for quota metric 'Read requests' of service 'logging.googleapis.com' for consumer 'project_number:764086051850'."}}`)
	got := apiErrorMessage(body, "429 Too Many Requests")
	if !strings.Contains(got, "set-quota-project") {
		t.Errorf("quota error carries no hint: %s", got)
	}
	// An unrelated error must not collect the hint.
	plain := apiErrorMessage([]byte(`{"error":{"message":"Permission denied"}}`), "403")
	if strings.Contains(plain, "set-quota-project") {
		t.Errorf("hint attached to an unrelated error: %s", plain)
	}
}

// TestRequests_CarryTheQuotaProject guards a header that is invisible when
// missing: without x-goog-user-project, a bearer token on a plain HTTP
// request has API quota billed to the OAuth client's own project, which for
// gcloud credentials is a Google-owned project shared by every gcloud user.
// The quota_project_id in the ADC file only reaches the wire if we put it
// there.
func TestRequests_CarryTheQuotaProject(t *testing.T) {
	f := newFakeAPI(t, okHandler)
	req := testRequest(t)

	if _, err := f.collector().Collect(context.Background(), req); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	reqs := f.matching("")
	if len(reqs) == 0 {
		t.Fatal("no requests recorded")
	}
	for _, r := range reqs {
		if got := r.quotaProject; got != "quota-proj" {
			t.Errorf("%s sent x-goog-user-project %q, want quota-proj", r.path, got)
		}
	}
}

// TestRequests_OmitTheQuotaProjectWhenUnset keeps the header optional.
// Sending a project the caller lacks serviceusage.services.use on turns a
// working request into a 403, so no quota project means no header.
func TestRequests_OmitTheQuotaProjectWhenUnset(t *testing.T) {
	f := newFakeAPI(t, okHandler)
	c := f.collector()
	c.quotaProject = func() string { return "" }
	req := testRequest(t)

	if _, err := c.Collect(context.Background(), req); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, r := range f.matching("") {
		if r.quotaProject != "" {
			t.Errorf("%s sent x-goog-user-project %q with none configured", r.path, r.quotaProject)
		}
	}
}

// TestCollect_PerDatabaseMetricsAreScopedToTheBenchmark covers the noise Cloud
// SQL adds by default: a metric with a database label is reported for
// cloudsqladmin, postgres and template1 as well, so an unfiltered query
// returns four series where one is wanted, and naive aggregation across the
// label mixes in Google's housekeeping.
func TestCollect_PerDatabaseMetricsAreScopedToTheBenchmark(t *testing.T) {
	f := newFakeAPI(t, okHandler)
	req := testRequest(t)

	if _, err := f.collector().Collect(context.Background(), req); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	var scoped, unscoped int
	for _, r := range f.matching("/timeSeries") {
		filter := r.query.Get("filter")
		if strings.Contains(filter, `metric.labels.database = "tpcc"`) {
			scoped++
			continue
		}
		unscoped++
		// A metric with no database label must not be filtered on one: the
		// filter would match nothing and the series would vanish.
		for _, m := range cloudSQLMetrics {
			if m.perDatabase && strings.Contains(filter, m.suffix) {
				t.Errorf("%s is per-database but was queried unscoped", m.suffix)
			}
		}
	}

	var want int
	for _, m := range cloudSQLMetrics {
		if m.perDatabase {
			want++
		}
	}
	if scoped != want {
		t.Errorf("%d scoped queries, want %d (one per per-database metric)", scoped, want)
	}
	if unscoped != len(cloudSQLMetrics)-want {
		t.Errorf("%d unscoped queries, want %d", unscoped, len(cloudSQLMetrics)-want)
	}
}

// TestTimeSeries_UnknownDatabaseCollectsEverything keeps the filter optional:
// a run whose outputs carry no db name should still get all four databases
// rather than nothing.
func TestTimeSeries_UnknownDatabaseCollectsEverything(t *testing.T) {
	f := newFakeAPI(t, okHandler)
	c := f.collector()
	c.database = ""

	if _, err := c.timeSeries(context.Background(),
		"cloudsql.googleapis.com/database/postgresql/transaction_count", "ALIGN_RATE", true, winStart, winEnd); err != nil {
		t.Fatalf("timeSeries: %v", err)
	}
	if filter := f.matching("/timeSeries")[0].query.Get("filter"); strings.Contains(filter, "metric.labels.database") {
		t.Errorf("filtered on an unknown database: %s", filter)
	}
}

// TestCloudSQLMetrics_ExcludesFixedParameters guards against a constant
// creeping back into the table. A benchmark pins the tier, disk size and
// provisioned IO, so these never vary within a run and belong in
// instance.json, not in a per-window time series.
func TestCloudSQLMetrics_ExcludesFixedParameters(t *testing.T) {
	fixed := []string{
		"cpu/reserved_cores", "memory/quota", "disk/quota",
		"disk/provisioning/iops", "disk/provisioning/throughput",
		"instance_state", "postgresql/num_backends",
		"postgresql/transaction_id_utilization",
	}
	for _, m := range cloudSQLMetrics {
		for _, f := range fixed {
			if m.suffix == f {
				t.Errorf("%s is constant for a run; it belongs in instance.json", f)
			}
		}
	}
}
