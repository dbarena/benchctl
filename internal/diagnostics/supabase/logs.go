package supabase

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/dbarena/benchctl/internal/diagnostics"
)

const (
	// logRowLimit caps rows per request. The endpoint publishes no row limit,
	// so this is benchctl's own bound.
	logRowLimit = 10000

	// maxLogChunk is the widest range the endpoint accepts. A longer span is
	// requested in slices rather than failing validation outright, which
	// would lose the whole log for a sweep running over a day.
	maxLogChunk = 24 * time.Hour

	// maxLogMessageBytes truncates one entry, as in the AWS and GCP
	// collectors: statements logged for exceeding log_min_duration_statement
	// dwarf everything else and say nothing the catalogue snapshot does not.
	maxLogMessageBytes = 4096

	// maxLogBytes bounds the rendered log.
	maxLogBytes = 8 << 20
)

// logQuery selects the project's Postgres log.
//
// Note that `select *` and `count(*)` are rejected, so columns are listed.
//
// parsed.query_id is the pg_stat_statements queryid, which joins a log
// line to a row in the report's calls.csv and outliers.csv.
const logQuery = `select timestamp, event_message, ` +
	`log_attributes['parsed.error_severity'] as error_severity, ` +
	`log_attributes['parsed.sql_state_code'] as sql_state_code, ` +
	`log_attributes['parsed.query_id'] as query_id, ` +
	`log_attributes['parsed.backend_type'] as backend_type ` +
	`from logs where source = 'postgres_logs' order by timestamp asc limit `

// logRow is one row of the analytics response.
type logRow struct {
	Timestamp     any    `json:"timestamp"`
	EventMessage  string `json:"event_message"`
	ErrorSeverity string `json:"error_severity"`
	SQLStateCode  string `json:"sql_state_code"`
	QueryID       string `json:"query_id"`
	BackendType   string `json:"backend_type"`
}

// analyticsResponse carries its error inside a 200 body, so both have to be
// checked.
type analyticsResponse struct {
	Result []logRow `json:"result"`
	Error  any      `json:"error"`
}

// collectLog captures the Postgres log over the whole run as plain text.
func (c *Collector) collectLog(ctx context.Context, req diagnostics.Request, start, end time.Time, res *diagnostics.Result) {
	var rows []logRow
	var lastURL string
	for _, chunk := range splitSpan(start, end, maxLogChunk) {
		params := url.Values{}
		params.Set("sql", logQuery+fmt.Sprint(logRowLimit))
		// Strictly Z-suffixed: the endpoint's schema rejects a +00:00 offset.
		params.Set("iso_timestamp_start", diagnostics.RFC3339(chunk.start))
		params.Set("iso_timestamp_end", diagnostics.RFC3339(chunk.end))

		body, requestURL, err := c.get(ctx, "/analytics/endpoints/logs", params)
		lastURL = requestURL
		if err != nil {
			req.Save(res, req.Dest, "postgres.log", "GET "+requestURL, nil, err)
			return
		}
		var parsed analyticsResponse
		if err := json.Unmarshal(body, &parsed); err != nil {
			req.Save(res, req.Dest, "postgres.log", "GET "+requestURL, nil, fmt.Errorf("parse analytics response: %w", err))
			return
		}
		if msg := errorText(parsed.Error); msg != "" {
			req.Save(res, req.Dest, "postgres.log", "GET "+requestURL, nil, fmt.Errorf("analytics query: %s", msg))
			return
		}
		rows = append(rows, parsed.Result...)
		if len(parsed.Result) >= logRowLimit {
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("postgres.log: a slice hit the %d row limit; the log is incomplete", logRowLimit))
		}
	}

	text, notes := renderRows(rows)
	for _, n := range notes {
		res.Warnings = append(res.Warnings, "postgres.log: "+n)
	}
	req.Save(res, req.Dest, "postgres.log", "GET "+lastURL, []byte(text), nil)
}

// errorText renders the endpoint's error field, which is null on success and
// either a string or an object otherwise.
func errorText(v any) string {
	switch e := v.(type) {
	case nil:
		return ""
	case string:
		return e
	default:
		out, err := json.Marshal(e)
		if err != nil {
			return fmt.Sprint(e)
		}
		return string(out)
	}
}

type span struct{ start, end time.Time }

// splitSpan divides [start, end] into slices no wider than max.
func splitSpan(start, end time.Time, max time.Duration) []span {
	if !end.After(start) {
		return []span{{start, end}}
	}
	var out []span
	for s := start; s.Before(end); s = s.Add(max) {
		e := s.Add(max)
		if e.After(end) {
			e = end
		}
		out = append(out, span{s, e})
	}
	return out
}

// renderRows turns log rows into text sorted by timestamp, truncating
// oversized messages and an oversized log with a marker in each case.
func renderRows(rows []logRow) (text string, notes []string) {
	sort.SliceStable(rows, func(i, j int) bool { return rowTime(rows[i]) < rowTime(rows[j]) })

	var b strings.Builder
	clipped, dropped := 0, 0
	for i, r := range rows {
		msg := strings.TrimRight(r.EventMessage, "\n")
		if len(msg) > maxLogMessageBytes {
			msg = msg[:maxLogMessageBytes] + fmt.Sprintf(" ... [benchctl truncated %d bytes]", len(msg)-maxLogMessageBytes)
			clipped++
		}
		if b.Len()+len(msg) > maxLogBytes {
			dropped = len(rows) - i
			break
		}
		b.WriteString(rowTimestampText(r))
		if r.ErrorSeverity != "" {
			b.WriteString(" " + r.ErrorSeverity)
		}
		// Statement logs report queryid 0, which is noise rather than a join
		// key.
		if r.QueryID != "" && r.QueryID != "0" {
			b.WriteString(" queryid=" + r.QueryID)
		}
		b.WriteByte(' ')
		b.WriteString(msg)
		b.WriteByte('\n')
	}

	if clipped > 0 {
		notes = append(notes, fmt.Sprintf("%d of %d messages exceeded %d bytes and were truncated", clipped, len(rows), maxLogMessageBytes))
	}
	if dropped > 0 {
		notes = append(notes, fmt.Sprintf("stopped at %d bytes with %d rows unread", maxLogBytes, dropped))
	}
	return b.String(), notes
}

// rowTime normalises the timestamp for sorting. The endpoint has returned it
// both as a microsecond number and as a string across versions, so neither is
// assumed.
func rowTime(r logRow) float64 {
	switch t := r.Timestamp.(type) {
	case float64:
		return t
	case string:
		if parsed, err := time.Parse(time.RFC3339, t); err == nil {
			return float64(parsed.UnixMicro())
		}
	}
	return 0
}

func rowTimestampText(r logRow) string {
	switch t := r.Timestamp.(type) {
	case float64:
		return diagnostics.RFC3339(time.UnixMicro(int64(t)))
	case string:
		if parsed, err := time.Parse(time.RFC3339, t); err == nil {
			return diagnostics.RFC3339(parsed)
		}
		return t
	}
	return "-"
}
