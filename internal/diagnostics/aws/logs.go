package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dbarena/benchctl/internal/diagnostics"
)

const (
	// maxLogEvents bounds a single log fetch. FilterLogEvents is throttled at 5
	// requests per second in several regions, and the CLI paginates
	// transparently, so an unbounded fetch of a chatty run becomes
	// a throttled crawl.
	maxLogEvents = 50000

	// maxLogMessageBytes truncates one event.
	//
	// A single event, such as logged insert statements in bulk loads, can
	// reach CloudWatch's 256 KB cap. The statement text adds no value
	// compared to pi_top_sql.json.
	maxLogMessageBytes = 4096

	// maxLogBytes bounds the rendered log, since maxLogEvents alone does not.
	maxLogBytes = 8 << 20
)

// logGroupName is where RDS exports Postgres logs given
// enabled_cloudwatch_logs_exports = ["postgresql"], which
// deployments/rds/postgres sets.
//
// The exported copy is preferred over rds download-db-log-file: it is not tied
// to the lifetime of the instance and does not add additional load to the
// database.
func logGroupName(instanceID string) string {
	return "/aws/rds/instance/" + instanceID + "/postgresql"
}

// collectLog captures the Postgres log over the whole run as plain text.
//
// The raw API response is deliberately not kept as it would double fetch size
// without providing additional value.
func (c *Collector) collectLog(ctx context.Context, req diagnostics.Request, s span, res *diagnostics.Result) {
	out, argv, err := c.run(ctx,
		"logs", "filter-log-events",
		"--log-group-name", logGroupName(c.instanceID),
		// Milliseconds. logs start-query takes seconds; confusing the two
		// returns zero rows with no error.
		"--start-time", strconv.FormatInt(s.start.UnixMilli(), 10),
		"--end-time", strconv.FormatInt(s.end.UnixMilli(), 10),
		"--max-items", strconv.Itoa(maxLogEvents),
	)
	if err != nil {
		if strings.Contains(err.Error(), "ResourceNotFoundException") {
			err = fmt.Errorf("log group %s does not exist; the instance was provisioned without enabled_cloudwatch_logs_exports", logGroupName(c.instanceID))
		}
		c.save(res, req, req.Dest, "postgresql.log", argv, nil, err)
		return
	}

	text, notes, err := renderLogEvents(out)
	if err != nil {
		c.save(res, req, req.Dest, "postgresql.log", argv, nil, err)
		return
	}
	for _, n := range notes {
		res.Warnings = append(res.Warnings, "postgresql.log: "+n)
	}
	c.save(res, req, req.Dest, "postgresql.log", argv, []byte(text), nil)
}

// renderLogEvents turns a filter-log-events response into text, sorted by
// timestamp: the API guarantees no ordering across streams. Oversized
// messages and an oversized log are truncated, each with a marker in the text
// and a note for the caller to surface.
func renderLogEvents(out []byte) (text string, notes []string, err error) {
	var parsed struct {
		Events []struct {
			Timestamp int64  `json:"timestamp"`
			Message   string `json:"message"`
		} `json:"events"`
		NextToken string `json:"nextToken"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return "", nil, fmt.Errorf("parse filter-log-events response: %w", err)
	}
	events := parsed.Events
	sort.SliceStable(events, func(i, j int) bool { return events[i].Timestamp < events[j].Timestamp })

	var b strings.Builder
	clipped, dropped := 0, 0
	for i, e := range events {
		msg := strings.TrimRight(e.Message, "\n")
		if len(msg) > maxLogMessageBytes {
			msg = msg[:maxLogMessageBytes] + fmt.Sprintf(" ... [benchctl truncated %d bytes]", len(msg)-maxLogMessageBytes)
			clipped++
		}
		if b.Len()+len(msg) > maxLogBytes {
			dropped = len(events) - i
			break
		}
		b.WriteString(time.UnixMilli(e.Timestamp).UTC().Format(time.RFC3339))
		b.WriteByte(' ')
		b.WriteString(msg)
		b.WriteByte('\n')
	}

	if clipped > 0 {
		notes = append(notes, fmt.Sprintf("%d of %d messages exceeded %d bytes and were truncated", clipped, len(events), maxLogMessageBytes))
	}
	if dropped > 0 {
		notes = append(notes, fmt.Sprintf("stopped at %d bytes with %d events unread", maxLogBytes, dropped))
	}
	if parsed.NextToken != "" {
		notes = append(notes, fmt.Sprintf("stopped at the %d event cap; the window is incomplete", maxLogEvents))
	}
	return b.String(), notes, nil
}
