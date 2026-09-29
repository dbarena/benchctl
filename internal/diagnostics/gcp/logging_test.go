package gcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbarena/benchctl/internal/diagnostics"
)

// TestLogFilter pins the filter, which is the only thing scoping the query.
// entries.list has no interval argument: without timestamp bounds in the
// filter it silently defaults to the last day.
func TestLogFilter(t *testing.T) {
	got := logFilter("proj:bench-pg", winStart, winEnd)
	for _, want := range []string{
		`resource.type="cloudsql_database"`,
		`resource.labels.database_id="proj:bench-pg"`,
		`timestamp>="2026-09-29T10:00:00Z"`,
		`timestamp<="2026-09-29T11:00:00Z"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("filter is missing %s\ngot: %s", want, got)
		}
	}
}

// TestCollectLog_PaginatesPastEmptyPages covers a documented trap: entries.list
// returns a page token with no entries when it ran out of time before
// searching every bucket. Stopping there loses the rest of the log.
func TestCollectLog_PaginatesPastEmptyPages(t *testing.T) {
	pages := []string{
		`{"entries":[{"timestamp":"2026-09-29T10:01:00Z","textPayload":"first"}],"nextPageToken":"p2"}`,
		`{"nextPageToken":"p3"}`, // no entries, but not the end
		`{"entries":[{"timestamp":"2026-09-29T10:03:00Z","textPayload":"last"}]}`,
	}
	var n int
	f := newFakeAPI(t, func(r *recordedRequest) (int, string) {
		if !strings.Contains(r.path, "entries:list") {
			return okHandler(r)
		}
		body := pages[min(n, len(pages)-1)]
		n++
		return 200, body
	})
	req := testRequest(t)
	var res diagnostics.Result

	f.collector().collectLog(context.Background(), req, winStart, winEnd, &res)

	if n != 3 {
		t.Errorf("%d requests, want 3 (the empty middle page must not stop pagination)", n)
	}
	data, err := os.ReadFile(filepath.Join(req.Dest, "postgres.log"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, want := range []string{"first", "last"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("log is missing %q:\n%s", want, data)
		}
	}
}

func TestCollectLog_RequestShape(t *testing.T) {
	f := newFakeAPI(t, okHandler)
	req := testRequest(t)
	var res diagnostics.Result

	f.collector().collectLog(context.Background(), req, winStart, winEnd, &res)

	reqs := f.matching("entries:list")
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(reqs))
	}
	body := reqs[0].body
	if got := body["orderBy"]; got != "timestamp asc" {
		t.Errorf("orderBy = %v, want ascending so the log reads forwards", got)
	}
	names, _ := body["resourceNames"].([]any)
	if len(names) != 1 || names[0] != "projects/proj" {
		t.Errorf("resourceNames = %v, want the project", body["resourceNames"])
	}
}

func TestCollectLog_ErrorIsReported(t *testing.T) {
	f := newFakeAPI(t, func(r *recordedRequest) (int, string) {
		if strings.Contains(r.path, "entries:list") {
			return 403, `{"error":{"message":"Permission logging.logEntries.list denied"}}`
		}
		return okHandler(r)
	})
	req := testRequest(t)
	var res diagnostics.Result

	f.collector().collectLog(context.Background(), req, winStart, winEnd, &res)

	if len(res.Artifacts) != 0 {
		t.Errorf("recorded an artifact for a failed fetch: %v", res.Artifacts)
	}
	joined := strings.Join(res.Warnings, " ")
	if !strings.Contains(joined, "logging.logEntries.list") {
		t.Errorf("warning does not carry the API message: %v", res.Warnings)
	}
}

func TestRenderEntries(t *testing.T) {
	entries := []logEntry{
		{Timestamp: winStart.Add(2 * time.Minute), Severity: "INFO", TextPayload: "second"},
		{Timestamp: winStart, Severity: "INFO", TextPayload: "first\n"},
	}
	text, notes := renderEntries(entries)
	if len(notes) != 0 {
		t.Errorf("unexpected notes: %v", notes)
	}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "first") {
		t.Fatalf("entries not sorted by timestamp:\n%s", text)
	}
	if !strings.HasPrefix(lines[0], "2026-09-29T10:00:00Z INFO ") {
		t.Errorf("line is not timestamped with severity: %q", lines[0])
	}
}

// TestRenderEntries_TruncatesGiantMessages mirrors the AWS fix: bulk-load
// statements logged by log_min_duration_statement dwarf everything else.
func TestRenderEntries_TruncatesGiantMessages(t *testing.T) {
	entries := []logEntry{{Timestamp: winStart, TextPayload: strings.Repeat("x", maxLogMessageBytes*3)}}

	text, notes := renderEntries(entries)
	if len(text) > maxLogMessageBytes+200 {
		t.Errorf("rendered %d bytes, want about %d", len(text), maxLogMessageBytes)
	}
	if !strings.Contains(text, "benchctl truncated") {
		t.Error("truncation is not marked in the text")
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "were truncated") {
		t.Errorf("notes = %v, want one about truncation", notes)
	}
}

func TestRenderEntries_StopsAtTheByteBudget(t *testing.T) {
	var entries []logEntry
	for range (maxLogBytes / maxLogMessageBytes) + 10 {
		entries = append(entries, logEntry{Timestamp: winStart, TextPayload: strings.Repeat("y", maxLogMessageBytes)})
	}

	text, notes := renderEntries(entries)
	if len(text) > maxLogBytes {
		t.Errorf("rendered %d bytes, want at most %d", len(text), maxLogBytes)
	}
	if !strings.Contains(strings.Join(notes, " "), "entries unread") {
		t.Errorf("notes = %v, want one about the byte budget", notes)
	}
}

// TestCollectLog_ReportsThePageCap guards the quota bound: entries.list is
// capped at 60 requests per minute per project and cannot be raised.
func TestCollectLog_ReportsThePageCap(t *testing.T) {
	f := newFakeAPI(t, func(r *recordedRequest) (int, string) {
		if strings.Contains(r.path, "entries:list") {
			return 200, fmt.Sprintf(`{"entries":[{"timestamp":%q,"textPayload":"x"}],"nextPageToken":"more"}`,
				diagnostics.RFC3339(winStart))
		}
		return okHandler(r)
	})
	req := testRequest(t)
	var res diagnostics.Result

	f.collector().collectLog(context.Background(), req, winStart, winEnd, &res)

	if got := len(f.matching("entries:list")); got != maxLogPages {
		t.Errorf("%d requests, want the cap of %d", got, maxLogPages)
	}
	if !strings.Contains(strings.Join(res.Warnings, " "), "page cap") {
		t.Errorf("warnings = %v, want one about the page cap", res.Warnings)
	}
}
