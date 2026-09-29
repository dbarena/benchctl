package aws

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dbarena/benchctl/internal/diagnostics"
)

var (
	winStart = time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	winEnd   = winStart.Add(time.Hour)
)

// fakeAWS replays canned CLI output. Responses are matched on a substring of
// the argv so a test can key off the operation ("get-metric-data") without
// spelling out every flag, and every call is recorded so tests can assert on
// what was asked for. An unmatched call fails loudly rather than returning
// empty output, which would otherwise look like a successful empty response.
//
// Matching is longest-key-first, which makes it deterministic and lets a
// specific key override a general one; iterating the maps directly would pick
// at random between two keys that both match a call.
type fakeAWS struct {
	t         *testing.T
	responses map[string]string
	errs      map[string]error
	calls     []string
}

func (f *fakeAWS) run(_ context.Context, args ...string) ([]byte, error) {
	joined := strings.Join(args, " ")
	f.calls = append(f.calls, joined)
	if key, ok := longestMatch(joined, f.errs); ok {
		return nil, f.errs[key]
	}
	if key, ok := longestMatch(joined, f.responses); ok {
		return []byte(f.responses[key]), nil
	}
	f.t.Errorf("fakeAWS: no canned response for %q", joined)
	return nil, errors.New("no canned response")
}

// longestMatch returns the longest key of m that appears in s, so the most
// specific canned entry wins regardless of map order.
func longestMatch[V any](s string, m map[string]V) (string, bool) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys {
		if strings.Contains(s, k) {
			return k, true
		}
	}
	return "", false
}

func (f *fakeAWS) called(substr string) bool {
	for _, c := range f.calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

func (f *fakeAWS) call(t *testing.T, substr string) string {
	t.Helper()
	for _, c := range f.calls {
		if strings.Contains(c, substr) {
			return c
		}
	}
	t.Fatalf("no call matching %q; calls were:\n%s", substr, strings.Join(f.calls, "\n"))
	return ""
}

// allResponses covers every call a full Collect makes, so individual tests
// only override what they care about.
func allResponses() map[string]string {
	return map[string]string{
		"describe-db-instances": `{"DBInstances":[{"DBInstanceIdentifier":"bench-pg","StorageType":"gp3","Iops":3000,` +
			`"DBParameterGroups":[{"DBParameterGroupName":"bench-pg","ParameterApplyStatus":"in-sync"}]}]}`,
		"describe-db-parameters":     `{"Parameters":[{"ParameterName":"log_checkpoints","ParameterValue":"1"}]}`,
		"describe-events":            `{"Events":[]}`,
		"pi get-resource-metadata":   `{"Identifier":"db-ABC","Features":{"SQL_DIGEST_STATISTICS":{"Status":"ENABLED"}}}`,
		"filter-log-events":          `{"events":[{"timestamp":1790671200000,"message":"LOG:  checkpoint starting: time\n"}]}`,
		"get-metric-data":            `{"MetricDataResults":[{"Id":"m0","Label":"CPUUtilization","StatusCode":"Complete","Timestamps":[],"Values":[]}]}`,
		"pi get-resource-metrics":    `{"AlignedStartTime":1.0,"AlignedEndTime":2.0,"MetricList":[]}`,
		"pi describe-dimension-keys": `{"Keys":[]}`,
	}
}

func newTestCollector(t *testing.T, f *fakeAWS) *Collector {
	t.Helper()
	return &Collector{exec: f.run, region: "eu-central-1", instanceID: "bench-pg", resourceID: "db-ABC"}
}

func testRequest(t *testing.T) diagnostics.Request {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "diagnostics")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return diagnostics.Request{
		RunID:   "run-1",
		Outputs: map[string]string{"region": "eu-central-1", "db_instance_identifier": "bench-pg", "dbi_resource_id": "db-ABC"},
		Windows: []diagnostics.Window{{Name: "benchmark", Iteration: 1, Start: winStart, End: winEnd}},
		Dest:    dest,
	}
}

func TestCollect_WritesEverySource(t *testing.T) {
	f := &fakeAWS{t: t, responses: allResponses()}
	c := newTestCollector(t, f)
	req := testRequest(t)

	res, err := c.Collect(context.Background(), req)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", res.Warnings)
	}

	want := []string{
		"instance.json", "parameters.json", "events.json",
		"pi_resource_metadata.json",
		"postgresql.log",
		"benchmark_iter1/cloudwatch.json",
		"benchmark_iter1/pi_db_load.json",
		"benchmark_iter1/pi_top_sql.json",
		"benchmark_iter1/pi_counters_1.json",
	}
	got := map[string]string{}
	for _, a := range res.Artifacts {
		got[a.File] = a.Command
	}
	for _, w := range want {
		cmd, ok := got[w]
		if !ok {
			t.Errorf("missing artifact %s; got %v", w, res.Artifacts)
			continue
		}
		if !strings.HasPrefix(cmd, "aws ") {
			t.Errorf("%s: command %q does not record the argv", w, cmd)
		}
		if _, err := os.Stat(filepath.Join(req.Dest, w)); err != nil {
			t.Errorf("%s recorded but not written: %v", w, err)
		}
	}
}

// TestCollect_PartialFailureKeepsTheRest is the behaviour the whole design
// hangs on: Performance Insights being unavailable must not cost us
// CloudWatch, the log, or the configuration snapshot.
func TestCollect_PartialFailureKeepsTheRest(t *testing.T) {
	f := &fakeAWS{
		t:         t,
		responses: allResponses(),
		errs: map[string]error{
			"pi ": errors.New("AccessDeniedException: pi:GetResourceMetrics"),
		},
	}
	c := newTestCollector(t, f)
	req := testRequest(t)

	res, err := c.Collect(context.Background(), req)
	if err != nil {
		t.Fatalf("Collect returned an error for a partial failure: %v", err)
	}
	if len(res.Warnings) == 0 {
		t.Error("Performance Insights failed without a warning")
	}
	for _, a := range res.Artifacts {
		if strings.Contains(a.File, "pi_") {
			t.Errorf("recorded a Performance Insights artifact that failed: %s", a.File)
		}
	}
	for _, want := range []string{"cloudwatch.json", "instance.json", "postgresql.log"} {
		found := false
		for _, a := range res.Artifacts {
			if strings.HasSuffix(a.File, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s was lost to an unrelated failure", want)
		}
	}
}

// TestCollect_TotalFailureIsAnError distinguishes "nothing worked" from
// "something worked": only the former deserves an error, per the
// diagnostics.Collector contract.
func TestCollect_TotalFailureIsAnError(t *testing.T) {
	f := &fakeAWS{t: t, responses: allResponses(), errs: map[string]error{"": errors.New("network is unreachable")}}
	c := newTestCollector(t, f)

	res, err := c.Collect(context.Background(), testRequest(t))
	if err == nil {
		t.Fatal("expected an error when every source failed")
	}
	if len(res.Artifacts) != 0 {
		t.Errorf("artifacts recorded despite total failure: %v", res.Artifacts)
	}
}

// TestCollect_PerWindowSourcesRunPerWindow guards the split between
// whole-run artifacts and per-window ones. Collecting the log or the instance
// description once per fixture would multiply calls for identical data, and
// collecting DB load once for the whole run would destroy the attribution
// that step windows exist to provide.
func TestCollect_PerWindowSourcesRunPerWindow(t *testing.T) {
	f := &fakeAWS{t: t, responses: allResponses()}
	c := newTestCollector(t, f)
	req := testRequest(t)
	req.Windows = []diagnostics.Window{
		{Name: "warmup", Iteration: 1, Start: winStart, End: winStart.Add(30 * time.Minute)},
		{Name: "benchmark", Iteration: 1, Start: winStart.Add(30 * time.Minute), End: winEnd},
	}

	res, err := c.Collect(context.Background(), req)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	count := func(substr string) int {
		n := 0
		for _, a := range res.Artifacts {
			if strings.Contains(a.File, substr) {
				n++
			}
		}
		return n
	}
	if got := count("cloudwatch.json"); got != 2 {
		t.Errorf("%d cloudwatch artifacts, want one per window (2)", got)
	}
	if got := count("pi_db_load.json"); got != 2 {
		t.Errorf("%d DB load artifacts, want one per window (2)", got)
	}
	if got := count("instance.json"); got != 1 {
		t.Errorf("%d instance snapshots, want exactly 1 for the whole run", got)
	}
	if got := count("postgresql.log"); got != 1 {
		t.Errorf("%d log fetches, want exactly 1 for the whole run", got)
	}
}

// TestCollect_LogAndEventsSpanEveryWindow checks the whole-run sources cover
// the full benchmark rather than just the first step.
func TestCollect_LogAndEventsSpanEveryWindow(t *testing.T) {
	f := &fakeAWS{t: t, responses: allResponses()}
	c := newTestCollector(t, f)
	req := testRequest(t)
	last := winEnd.Add(2 * time.Hour)
	req.Windows = []diagnostics.Window{
		{Name: "warmup", Start: winStart, End: winEnd},
		{Name: "benchmark", Start: winEnd, End: last},
	}

	if _, err := c.Collect(context.Background(), req); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	logCall := f.call(t, "filter-log-events")
	if !strings.Contains(logCall, strconvMilli(winStart)) || !strings.Contains(logCall, strconvMilli(last)) {
		t.Errorf("log fetch does not span every window: %s", logCall)
	}
	eventsCall := f.call(t, "describe-events")
	if !strings.Contains(eventsCall, winStart.Format(time.RFC3339)) || !strings.Contains(eventsCall, last.Format(time.RFC3339)) {
		t.Errorf("event fetch does not span every window: %s", eventsCall)
	}
}

func strconvMilli(t time.Time) string {
	b, _ := json.Marshal(t.UnixMilli())
	return string(b)
}

func TestPreflight_RequiresIdentifiers(t *testing.T) {
	tests := []struct {
		name    string
		outputs map[string]string
		wantIn  string
	}{
		{
			// Self-hosted Postgres on EC2: correctly declares vendor aws, but
			// there is no RDS instance behind it.
			name:    "self-hosted aws target",
			outputs: map[string]string{"region": "eu-central-1", "public_ip": "3.1.2.3"},
			wantIn:  "only RDS targets are supported",
		},
		{
			name:    "missing region",
			outputs: map[string]string{"db_instance_identifier": "bench-pg", "dbi_resource_id": "db-ABC"},
			wantIn:  "region",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Collector{exec: (&fakeAWS{t: t, responses: allResponses()}).run}
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
