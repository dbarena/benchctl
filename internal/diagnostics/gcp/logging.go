package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dbarena/benchctl/internal/diagnostics"
)

const (
	// logPageSize is entries per request. Large on purpose: a benchmark
	// window with log_min_duration_statement at 5s emits a few hundred lines,
	// so one page covers a normal run and the 60 reads/min limit on
	// entries.list never comes into play.
	logPageSize = 1000

	// maxLogPages bounds pagination for a pathological run.
	maxLogPages = 30

	// maxLogMessageBytes truncates one entry, for the same reason as the AWS
	// collector: bulk-load statements logged by log_min_duration_statement
	// dwarf everything else and don't provide additional value on top of
	// metrics.
	maxLogMessageBytes = 4096

	// maxLogBytes bounds the rendered log.
	maxLogBytes = 8 << 20
)

// logEntry is the part of a Cloud Logging entry this renders.
type logEntry struct {
	Timestamp   time.Time `json:"timestamp"`
	Severity    string    `json:"severity"`
	TextPayload string    `json:"textPayload"`
	InsertID    string    `json:"insertId"`
}

type entriesListResponse struct {
	Entries       []logEntry `json:"entries"`
	NextPageToken string     `json:"nextPageToken"`
}

// collectLog captures the instance's Postgres log over the whole run as plain
// text.
func (c *Collector) collectLog(ctx context.Context, req diagnostics.Request, start, end time.Time, res *diagnostics.Result) {
	endpoint := c.loggingHost + "/v2/entries:list"
	filter := logFilter(c.databaseID, start, end)

	var entries []logEntry
	var token string
	var pagesRead int
	for ; pagesRead < maxLogPages; pagesRead++ {
		payload := map[string]any{
			"resourceNames": []string{"projects/" + c.projectID},
			"filter":        filter,
			"orderBy":       "timestamp asc",
			"pageSize":      logPageSize,
		}
		if token != "" {
			payload["pageToken"] = token
		}
		body, requestURL, err := c.post(ctx, endpoint, payload)
		if err != nil {
			req.Save(res, req.Dest, "postgres.log", "POST "+requestURL, nil, err)
			return
		}
		var parsed entriesListResponse
		if err := json.Unmarshal(body, &parsed); err != nil {
			req.Save(res, req.Dest, "postgres.log", "POST "+requestURL, nil, fmt.Errorf("parse entries.list response: %w", err))
			return
		}
		entries = append(entries, parsed.Entries...)
		// A page token with no entries does not mean the end: the API returns
		// one when it ran out of time before searching every bucket. Only an
		// absent token means done.
		if parsed.NextPageToken == "" {
			break
		}
		token = parsed.NextPageToken
	}

	text, notes := renderEntries(entries)
	if pagesRead == maxLogPages {
		notes = append(notes, fmt.Sprintf("stopped at the %d page cap; the window may be incomplete", maxLogPages))
	}
	for _, n := range notes {
		res.Warnings = append(res.Warnings, "postgres.log: "+n)
	}
	req.Save(res, req.Dest, "postgres.log", "POST "+endpoint+" filter="+filter, []byte(text), nil)
}

// logFilter selects this instance's Postgres log over the window.
//
// Timestamp bounds go in the filter rather than a parameter: entries.list has
// no interval argument, and without them it defaults to the last day.
func logFilter(databaseID string, start, end time.Time) string {
	return fmt.Sprintf(
		`resource.type="cloudsql_database" AND resource.labels.database_id=%q AND timestamp>=%q AND timestamp<=%q`,
		databaseID, diagnostics.RFC3339(start), diagnostics.RFC3339(end))
}

// renderEntries turns log entries into text sorted by timestamp, truncating
// oversized messages and an oversized log, each with a marker in the text and
// a note for the caller.
func renderEntries(entries []logEntry) (text string, notes []string) {
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Timestamp.Before(entries[j].Timestamp) })

	var b strings.Builder
	clipped, dropped := 0, 0
	for i, e := range entries {
		msg := strings.TrimRight(e.TextPayload, "\n")
		if len(msg) > maxLogMessageBytes {
			msg = msg[:maxLogMessageBytes] + fmt.Sprintf(" ... [benchctl truncated %d bytes]", len(msg)-maxLogMessageBytes)
			clipped++
		}
		if b.Len()+len(msg) > maxLogBytes {
			dropped = len(entries) - i
			break
		}
		b.WriteString(diagnostics.RFC3339(e.Timestamp))
		if e.Severity != "" {
			b.WriteString(" " + e.Severity)
		}
		b.WriteByte(' ')
		b.WriteString(msg)
		b.WriteByte('\n')
	}

	if clipped > 0 {
		notes = append(notes, fmt.Sprintf("%d of %d messages exceeded %d bytes and were truncated", clipped, len(entries), maxLogMessageBytes))
	}
	if dropped > 0 {
		notes = append(notes, fmt.Sprintf("stopped at %d bytes with %d entries unread", maxLogBytes, dropped))
	}
	return b.String(), notes
}
