package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dbarena/benchctl/internal/diagnostics"
)

// TestBuildMetricDataQueries_NeverSetsUnit guards a silent failure mode:
// CloudWatch does not convert units, so a MetricStat.Unit that does not match
// what was stored makes the query return nulls with no error at all.
func TestBuildMetricDataQueries_NeverSetsUnit(t *testing.T) {
	queries, err := buildMetricDataQueries("bench-pg")
	if err != nil {
		t.Fatalf("buildMetricDataQueries: %v", err)
	}
	raw, err := json.Marshal(queries)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), `"Unit"`) {
		t.Errorf("payload sets Unit, which silently nulls mismatched metrics:\n%s", raw)
	}
}

// TestBuildMetricDataQueries_IdsAreValid pins CloudWatch's rule that an Id
// must start with a lowercase letter and be unique within the call. Breaking
// either rejects the whole request.
func TestBuildMetricDataQueries_IdsAreValid(t *testing.T) {
	queries, err := buildMetricDataQueries("bench-pg")
	if err != nil {
		t.Fatalf("buildMetricDataQueries: %v", err)
	}
	seen := map[string]bool{}
	for _, q := range queries {
		if q.ID == "" || q.ID[0] < 'a' || q.ID[0] > 'z' {
			t.Errorf("Id %q must start with a lowercase letter", q.ID)
		}
		if seen[q.ID] {
			t.Errorf("duplicate Id %q", q.ID)
		}
		seen[q.ID] = true
		if q.MetricStat.Period != cloudWatchPeriod {
			t.Errorf("%s: period %d, want %d", q.Label, q.MetricStat.Period, cloudWatchPeriod)
		}
		if q.MetricStat.Metric.Namespace != "AWS/RDS" {
			t.Errorf("%s: namespace %q", q.Label, q.MetricStat.Metric.Namespace)
		}
		if len(q.MetricStat.Metric.Dimensions) != 1 || q.MetricStat.Metric.Dimensions[0].Name != "DBInstanceIdentifier" {
			t.Errorf("%s: dimensions %+v, want a single DBInstanceIdentifier", q.Label, q.MetricStat.Metric.Dimensions)
		}
	}
	if len(queries) > 500 {
		t.Errorf("%d queries exceeds CloudWatch's 500 per call", len(queries))
	}
}

// TestCollectCloudWatch_ScansAscending guards a default that quietly reverses
// every series: get-metric-data sorts descending unless told otherwise.
func TestCollectCloudWatch_ScansAscending(t *testing.T) {
	f := &fakeAWS{t: t, responses: allResponses()}
	c := newTestCollector(t, f)
	req := testRequest(t)
	var res diagnostics.Result

	c.collectCloudWatch(context.Background(), req, req.Windows[0], req.Dest, &res)

	call := f.call(t, "get-metric-data")
	if !strings.Contains(call, "--scan-by TimestampAscending") {
		t.Errorf("missing ascending scan order: %s", call)
	}
}

func TestWarnOnPartialMetricData(t *testing.T) {
	tests := []struct {
		name     string
		response string
		wantWarn string
	}{
		{
			name:     "complete is quiet",
			response: `{"MetricDataResults":[{"Label":"CPUUtilization","StatusCode":"Complete"}]}`,
		},
		{
			// Invisible in the exit code: the call succeeds and the series is
			// short.
			name:     "partial data is reported",
			response: `{"MetricDataResults":[{"Label":"ReadIOPS","StatusCode":"PartialData"}]}`,
			wantWarn: "ReadIOPS=PartialData",
		},
		{
			name:     "forbidden metric is reported",
			response: `{"MetricDataResults":[{"Label":"FreeableMemory","StatusCode":"Forbidden"}]}`,
			wantWarn: "FreeableMemory=Forbidden",
		},
		{
			name:     "top level messages are reported",
			response: `{"MetricDataResults":[],"Messages":[{"Value":"Maximum number of allowed metrics exceeded"}]}`,
			wantWarn: "Maximum number of allowed metrics exceeded",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var res diagnostics.Result
			if err := warnOnPartialMetricData([]byte(tt.response), &res); err != nil {
				t.Fatalf("warnOnPartialMetricData: %v", err)
			}
			joined := strings.Join(res.Warnings, "; ")
			if tt.wantWarn == "" {
				if joined != "" {
					t.Errorf("unexpected warnings: %s", joined)
				}
				return
			}
			if !strings.Contains(joined, tt.wantWarn) {
				t.Errorf("warnings %q do not mention %q", joined, tt.wantWarn)
			}
		})
	}
}

// TestCollectPILoad_AlignsToWindowStart guards a one-minute skew: Performance
// Insights stamps each bucket with its end time unless told otherwise, which
// shifts every series later than it happened.
func TestCollectPILoad_AlignsToWindowStart(t *testing.T) {
	f := &fakeAWS{t: t, responses: allResponses()}
	c := newTestCollector(t, f)
	req := testRequest(t)
	var res diagnostics.Result

	c.collectPILoad(context.Background(), req, req.Windows[0], req.Dest, &res)

	call := f.call(t, "pi get-resource-metrics")
	if !strings.Contains(call, "--period-alignment START_TIME") {
		t.Errorf("missing START_TIME alignment: %s", call)
	}
	if !strings.Contains(call, "--period-in-seconds "+piPeriodSeconds) {
		t.Errorf("unexpected period: %s", call)
	}
	if !strings.Contains(call, "db.wait_event") {
		t.Errorf("DB load is not grouped by wait event: %s", call)
	}
}

// TestCollectPITopSQL_RanksOnLoad pins the API's restriction that only
// db.load.avg and db.sampledload.avg can be ranked; a counter cannot.
func TestCollectPITopSQL_RanksOnLoad(t *testing.T) {
	f := &fakeAWS{t: t, responses: allResponses()}
	c := newTestCollector(t, f)
	req := testRequest(t)
	var res diagnostics.Result

	c.collectPITopSQL(context.Background(), req, req.Windows[0], req.Dest, &res)

	call := f.call(t, "describe-dimension-keys")
	if !strings.Contains(call, "--metric db.load.avg") {
		t.Errorf("must rank on db.load.avg: %s", call)
	}
	if !strings.Contains(call, "db.sql_tokenized") {
		t.Errorf("not grouped by SQL digest: %s", call)
	}
}

// TestCollectPICounters_BatchesToTheApiLimit guards the hard cap of 15
// metric queries per get-resource-metrics call.
func TestCollectPICounters_BatchesToTheApiLimit(t *testing.T) {
	f := &fakeAWS{t: t, responses: allResponses()}
	c := newTestCollector(t, f)
	req := testRequest(t)
	var res diagnostics.Result

	c.collectPICounters(context.Background(), req, req.Windows[0], req.Dest, &res)

	batches := 0
	for _, call := range f.calls {
		if !strings.Contains(call, "pi get-resource-metrics") {
			continue
		}
		batches++
		payload := jsonArgAfter(t, call, "--metric-queries")
		var queries []piMetricQuery
		if err := json.Unmarshal([]byte(payload), &queries); err != nil {
			t.Fatalf("parse metric queries: %v\n%s", err, payload)
		}
		if len(queries) > piMaxMetricQueries {
			t.Errorf("batch of %d exceeds the API limit of %d", len(queries), piMaxMetricQueries)
		}
	}
	if batches < 2 {
		t.Errorf("%d batches for %d counters; expected the list to be split", batches, len(piCounters))
	}
}

// TestCollectLog_UsesMilliseconds guards a 1000x error that returns zero rows
// silently: filter-log-events takes epoch milliseconds, while the sibling
// logs start-query takes seconds.
func TestCollectLog_UsesMilliseconds(t *testing.T) {
	f := &fakeAWS{t: t, responses: allResponses()}
	c := newTestCollector(t, f)
	req := testRequest(t)
	var res diagnostics.Result

	c.collectLog(context.Background(), req, winStart, winEnd, &res)

	call := f.call(t, "filter-log-events")
	wantStart := strconv.FormatInt(winStart.UnixMilli(), 10)
	if !strings.Contains(call, "--start-time "+wantStart) {
		t.Errorf("start time is not epoch milliseconds: %s", call)
	}
	if strings.Contains(call, strconv.FormatInt(winStart.Unix(), 10)+" ") {
		t.Errorf("start time looks like epoch seconds: %s", call)
	}
}

// TestCollectLog_MissingGroupExplainsItself covers an instance provisioned
// without the CloudWatch export, where the raw API error names an exception
// rather than the cause.
func TestCollectLog_MissingGroupExplainsItself(t *testing.T) {
	f := &fakeAWS{t: t, responses: allResponses()}
	f.errs = map[string]error{"filter-log-events": errorString("ResourceNotFoundException: The specified log group does not exist")}
	c := newTestCollector(t, f)
	req := testRequest(t)
	var res diagnostics.Result

	c.collectLog(context.Background(), req, winStart, winEnd, &res)

	joined := strings.Join(res.Warnings, "; ")
	if !strings.Contains(joined, "enabled_cloudwatch_logs_exports") {
		t.Errorf("warning does not explain the cause: %s", joined)
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }

func TestRenderLogEvents(t *testing.T) {
	out := `{"events":[
		{"timestamp":1790671260000,"message":"LOG:  checkpoint complete\n"},
		{"timestamp":1790671200000,"message":"LOG:  checkpoint starting: time\n"}
	]}`
	text, notes, err := renderLogEvents([]byte(out))
	if err != nil {
		t.Fatalf("renderLogEvents: %v", err)
	}
	if len(notes) != 0 {
		t.Errorf("unexpected notes: %v", notes)
	}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines, want 2:\n%s", len(lines), text)
	}
	// Sorted by timestamp: the API makes no ordering guarantee across
	// streams, and a log that jumps backwards is unreadable.
	if !strings.Contains(lines[0], "checkpoint starting") {
		t.Errorf("events not sorted by time:\n%s", text)
	}
	if !strings.HasPrefix(lines[0], time.UnixMilli(1790671200000).UTC().Format(time.RFC3339)) {
		t.Errorf("line is not timestamped: %q", lines[0])
	}
}

// TestRenderLogEvents_TruncatesGiantMessages is the fix for a real fetch where
// 21 bulk-load INSERTs at CloudWatch's 256 KB per-event cap made up 4.5 MB of
// a 4.9 MB log. The event survives, its VALUES tuples do not.
func TestRenderLogEvents_TruncatesGiantMessages(t *testing.T) {
	giant := strings.Repeat("x", maxLogMessageBytes*3)
	out := fmt.Sprintf(`{"events":[{"timestamp":1790671200000,"message":%q}]}`, giant)

	text, notes, err := renderLogEvents([]byte(out))
	if err != nil {
		t.Fatalf("renderLogEvents: %v", err)
	}
	if len(text) > maxLogMessageBytes+200 {
		t.Errorf("rendered %d bytes, want the message clipped to about %d", len(text), maxLogMessageBytes)
	}
	if !strings.Contains(text, "benchctl truncated") {
		t.Error("truncation is not marked in the text")
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "were truncated") {
		t.Errorf("notes = %v, want one about truncation", notes)
	}
}

// TestRenderLogEvents_StopsAtTheByteBudget covers the case the event cap
// misses entirely: few events, enormous total.
func TestRenderLogEvents_StopsAtTheByteBudget(t *testing.T) {
	var events []string
	msg := strings.Repeat("y", maxLogMessageBytes)
	for range (maxLogBytes / maxLogMessageBytes) + 10 {
		events = append(events, fmt.Sprintf(`{"timestamp":1790671200000,"message":%q}`, msg))
	}
	out := `{"events":[` + strings.Join(events, ",") + `]}`

	text, notes, err := renderLogEvents([]byte(out))
	if err != nil {
		t.Fatalf("renderLogEvents: %v", err)
	}
	if len(text) > maxLogBytes {
		t.Errorf("rendered %d bytes, want at most %d", len(text), maxLogBytes)
	}
	joined := strings.Join(notes, "; ")
	if !strings.Contains(joined, "events unread") {
		t.Errorf("notes = %v, want one about the byte budget", notes)
	}
}

func TestRenderLogEvents_ReportsPaginationCap(t *testing.T) {
	out := `{"events":[{"timestamp":1790671200000,"message":"LOG:  x"}],"nextToken":"more"}`
	_, notes, err := renderLogEvents([]byte(out))
	if err != nil {
		t.Fatalf("renderLogEvents: %v", err)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "event cap") {
		t.Errorf("notes = %v, want one about the event cap", notes)
	}
}

func TestShellArgs_QuotesJSONPayloads(t *testing.T) {
	got := shellArgs([]string{"aws", "pi", "get-resource-metrics", "--metric-queries", `[{"Metric":"db.load.avg"}]`})
	if !strings.Contains(got, `'[{"Metric":"db.load.avg"}]'`) {
		t.Errorf("JSON payload is not quoted, so the recorded command is not reproducible: %s", got)
	}
	if strings.Contains(shellArgs([]string{"aws", "rds", "describe-events"}), "'") {
		t.Error("plain arguments should not be quoted")
	}
}

func TestLogGroupName(t *testing.T) {
	if got, want := logGroupName("bench-pg"), "/aws/rds/instance/bench-pg/postgresql"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// jsonArgAfter pulls the value following flag out of a recorded argv.
func jsonArgAfter(t *testing.T, call, flag string) string {
	t.Helper()
	idx := strings.Index(call, flag+" ")
	if idx < 0 {
		t.Fatalf("no %s in %q", flag, call)
	}
	rest := call[idx+len(flag)+1:]
	if end := strings.Index(rest, " --"); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

// TestParameterGroupName_ReadsTheInstanceResponse covers reusing the
// describe-db-instances response instead of asking RDS for the same record a
// second time.
func TestParameterGroupName_ReadsTheInstanceResponse(t *testing.T) {
	tests := []struct {
		name     string
		instance string
		want     string
		wantErr  string
	}{
		{
			name:     "in-sync group",
			instance: `{"DBInstances":[{"DBParameterGroups":[{"DBParameterGroupName":"bench-pg","ParameterApplyStatus":"in-sync"}]}]}`,
			want:     "bench-pg",
		},
		{
			// The name is still returned: the parameters are worth capturing,
			// but the caller has to know they are not what is running.
			name:     "pending reboot is reported",
			instance: `{"DBInstances":[{"DBParameterGroups":[{"DBParameterGroupName":"bench-pg","ParameterApplyStatus":"pending-reboot"}]}]}`,
			want:     "bench-pg",
			wantErr:  "not running the parameters",
		},
		{
			name:     "no group",
			instance: `{"DBInstances":[{}]}`,
			wantErr:  "no parameter group",
		},
		{
			name:     "instance description unavailable",
			instance: ``,
			wantErr:  "could not be read",
		},
		{
			name:     "malformed",
			instance: `{nope`,
			wantErr:  "parse instance description",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parameterGroupName([]byte(tt.instance))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("parameterGroupName: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("group = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestCollect_ReadsTheInstanceOnce guards against the redundant second
// describe-db-instances call that used to fetch the parameter group.
func TestCollect_ReadsTheInstanceOnce(t *testing.T) {
	f := &fakeAWS{t: t, responses: allResponses()}
	c := newTestCollector(t, f)

	if _, err := c.Collect(context.Background(), testRequest(t)); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	n := 0
	for _, call := range f.calls {
		if strings.Contains(call, "describe-db-instances") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d describe-db-instances calls, want 1", n)
	}
}
