package supabase

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dbarena/benchctl/internal/diagnostics"
)

// TestCollectLog_RequestShape pins the three things the analytics endpoint is
// strict about: Z-suffixed timestamps (a +00:00 offset is rejected by its
// schema), an explicit column list (select * is refused), and the source
// filter that selects Postgres out of the unified log stream.
func TestCollectLog_RequestShape(t *testing.T) {
	f := newFakeAPI(t, okHandler)
	req := testRequest(t)
	var res diagnostics.Result

	f.collector(&fakeCLI{}).collectLog(context.Background(), req, winStart, winEnd, &res)

	reqs := f.matching("/analytics/endpoints/logs")
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(reqs))
	}
	q := reqs[0].query
	if got := q.Get("iso_timestamp_start"); got != "2026-09-29T10:00:00Z" {
		t.Errorf("iso_timestamp_start = %q, want a Z-suffixed timestamp", got)
	}
	if got := q.Get("iso_timestamp_end"); got != "2026-09-29T11:00:00Z" {
		t.Errorf("iso_timestamp_end = %q", got)
	}
	sql := q.Get("sql")
	if strings.Contains(sql, "select *") || strings.Contains(sql, "count(*)") {
		t.Errorf("sql uses a form the endpoint rejects: %s", sql)
	}
	for _, want := range []string{"source = 'postgres_logs'", "log_attributes['parsed.query_id']", "order by timestamp asc"} {
		if !strings.Contains(sql, want) {
			t.Errorf("sql is missing %s\ngot: %s", want, sql)
		}
	}
}

// TestCollectLog_ErrorInsideA200 covers the endpoint's habit of reporting
// failure in the body rather than the status, which a status-only check would
// ship as an empty log.
func TestCollectLog_ErrorInsideA200(t *testing.T) {
	f := newFakeAPI(t, func(r *recordedRequest) (int, string) {
		if strings.Contains(r.path, "/analytics/endpoints/logs") {
			return 200, `{"result":null,"error":{"message":"resource exhausted"}}`
		}
		return okHandler(r)
	})
	req := testRequest(t)
	var res diagnostics.Result

	f.collector(&fakeCLI{}).collectLog(context.Background(), req, winStart, winEnd, &res)

	if len(res.Artifacts) != 0 {
		t.Errorf("wrote a log despite the error: %v", res.Artifacts)
	}
	if !strings.Contains(strings.Join(res.Warnings, " "), "resource exhausted") {
		t.Errorf("the in-body error was not reported: %v", res.Warnings)
	}
}

// TestSplitSpan_RespectsThe24HourCap covers a sweep running longer than a
// day: the endpoint rejects a wider range outright, which would lose the
// whole log rather than part of it.
func TestSplitSpan_RespectsThe24HourCap(t *testing.T) {
	start := winStart
	tests := []struct {
		name string
		end  time.Time
		want int
	}{
		{"an hour", start.Add(time.Hour), 1},
		{"exactly a day", start.Add(24 * time.Hour), 1},
		{"a day and a minute", start.Add(24*time.Hour + time.Minute), 2},
		{"three days", start.Add(72 * time.Hour), 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitSpan(start, tt.end, maxLogChunk)
			if len(got) != tt.want {
				t.Fatalf("%d slices, want %d", len(got), tt.want)
			}
			// The slices have to tile the span exactly, or a gap loses log.
			if !got[0].start.Equal(start) {
				t.Errorf("first slice starts at %s, want %s", got[0].start, start)
			}
			if !got[len(got)-1].end.Equal(tt.end) {
				t.Errorf("last slice ends at %s, want %s", got[len(got)-1].end, tt.end)
			}
			for i := 1; i < len(got); i++ {
				if !got[i].start.Equal(got[i-1].end) {
					t.Errorf("gap between slice %d and %d", i-1, i)
				}
				if got[i].end.Sub(got[i].start) > maxLogChunk {
					t.Errorf("slice %d is wider than the cap", i)
				}
			}
		})
	}
}

func TestCollectLog_LongSpanIsRequestedInSlices(t *testing.T) {
	f := newFakeAPI(t, okHandler)
	req := testRequest(t)
	var res diagnostics.Result

	f.collector(&fakeCLI{}).collectLog(context.Background(), req, winStart, winStart.Add(50*time.Hour), &res)

	if got := len(f.matching("/analytics/endpoints/logs")); got != 3 {
		t.Errorf("%d requests for a 50 hour span, want 3", got)
	}
}

func TestRenderRows(t *testing.T) {
	rows := []logRow{
		{Timestamp: float64(winStart.Add(time.Minute).UnixMicro()), EventMessage: "second", ErrorSeverity: "LOG"},
		{Timestamp: float64(winStart.UnixMicro()), EventMessage: "first\n", ErrorSeverity: "LOG", QueryID: "12345"},
	}
	text, notes := renderRows(rows)
	if len(notes) != 0 {
		t.Errorf("unexpected notes: %v", notes)
	}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "first") {
		t.Fatalf("rows not sorted by timestamp:\n%s", text)
	}
	if !strings.HasPrefix(lines[0], "2026-09-29T10:00:00Z LOG queryid=12345 ") {
		t.Errorf("line is missing its timestamp, severity or queryid: %q", lines[0])
	}
}

// TestRenderRows_HandlesBothTimestampShapes covers the endpoint having
// returned the timestamp as a microsecond number and as a string across
// versions.
func TestRenderRows_HandlesBothTimestampShapes(t *testing.T) {
	for _, ts := range []any{float64(winStart.UnixMicro()), "2026-09-29T10:00:00Z"} {
		text, _ := renderRows([]logRow{{Timestamp: ts, EventMessage: "x"}})
		if !strings.HasPrefix(text, "2026-09-29T10:00:00Z") {
			t.Errorf("timestamp %v rendered as %q", ts, text)
		}
	}
}

func TestRenderRows_TruncatesGiantMessages(t *testing.T) {
	giant := strings.Repeat("x", maxLogMessageBytes*3)
	text, notes := renderRows([]logRow{{Timestamp: float64(winStart.UnixMicro()), EventMessage: giant}})

	if len(text) > maxLogMessageBytes+200 {
		t.Errorf("rendered %d bytes, want about %d", len(text), maxLogMessageBytes)
	}
	if !strings.Contains(text, "benchctl truncated") {
		t.Error("truncation is not marked in the text")
	}
	if len(notes) != 1 {
		t.Errorf("notes = %v, want one about truncation", notes)
	}
}

func TestCollectLog_ReportsTheRowLimit(t *testing.T) {
	f := newFakeAPI(t, func(r *recordedRequest) (int, string) {
		if strings.Contains(r.path, "/analytics/endpoints/logs") {
			var b strings.Builder
			b.WriteString(`{"error":null,"result":[`)
			for i := 0; i < logRowLimit; i++ {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString(`{"timestamp":1790671200000000,"event_message":"x"}`)
			}
			b.WriteString(`]}`)
			return 200, b.String()
		}
		return okHandler(r)
	})
	req := testRequest(t)
	var res diagnostics.Result

	f.collector(&fakeCLI{}).collectLog(context.Background(), req, winStart, winEnd, &res)

	if !strings.Contains(strings.Join(res.Warnings, " "), "row limit") {
		t.Errorf("hitting the row limit was not reported: %v", res.Warnings)
	}
}
