package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/dbarena/benchctl/internal/diagnostics"
)

// alignmentPeriod is the resolution requested from Cloud Monitoring. 60s
// matches what the AWS collector asks CloudWatch for, so the two line up.
const alignmentPeriod = "60s"

// metricPrefix is the namespace every Cloud SQL metric lives under.
const metricPrefix = "cloudsql.googleapis.com/database/"

// cloudSQLMetrics are the metric types worth collecting, each with the
// aligner its kind requires. Intentionally absent: the insights/* families,
// which need Query Insights enabled on the instance (off by default in
// deployments/gcp-cloudsql/postgres, since it samples queries on the
// instance under test).
var cloudSQLMetrics = []struct {
	// suffix is appended to metricPrefix.
	suffix string
	// aligner follows the metric's kind and value type: ALIGN_MEAN for a
	// numeric gauge, ALIGN_RATE for a DELTA counter (giving a per-second rate
	// comparable to CloudWatch's ReadIOPS), and ALIGN_FRACTION_TRUE for a
	// BOOL gauge, which the other two are rejected for.
	aligner string
	// perDatabase marks a metric carrying a `database` label. Cloud SQL
	// reports these for every database on the instance, so without a filter
	// each one arrives four times over: the benchmark database plus
	// cloudsqladmin, postgres and template1.
	perDatabase bool
}{
	// Saturation.
	{suffix: "cpu/utilization", aligner: "ALIGN_MEAN"},
	{suffix: "memory/usage", aligner: "ALIGN_MEAN"},
	{suffix: "memory/components", aligner: "ALIGN_MEAN"},
	{suffix: "disk/utilization", aligner: "ALIGN_MEAN"},
	{suffix: "disk/bytes_used", aligner: "ALIGN_MEAN"},

	// IO.
	{suffix: "disk/read_ops_count", aligner: "ALIGN_RATE"},
	{suffix: "disk/write_ops_count", aligner: "ALIGN_RATE"},
	{suffix: "network/received_bytes_count", aligner: "ALIGN_RATE"},
	{suffix: "network/sent_bytes_count", aligner: "ALIGN_RATE"},

	// Postgres internals, the reason for collecting at all: these are what
	// CloudWatch's equivalents cannot show.
	{suffix: "postgresql/num_backends_by_state", aligner: "ALIGN_MEAN", perDatabase: true},
	{suffix: "postgresql/backends_in_wait", aligner: "ALIGN_MEAN"},
	{suffix: "postgresql/transaction_count", aligner: "ALIGN_RATE", perDatabase: true},
	{suffix: "postgresql/statements_executed_count", aligner: "ALIGN_RATE", perDatabase: true},
	{suffix: "postgresql/checkpoint_count", aligner: "ALIGN_RATE"},
	{suffix: "postgresql/write_ahead_log/written_bytes_count", aligner: "ALIGN_RATE"},
	{suffix: "postgresql/write_ahead_log/flushed_bytes_count", aligner: "ALIGN_RATE"},
	{suffix: "postgresql/blocks_read_count", aligner: "ALIGN_RATE", perDatabase: true},
	{suffix: "postgresql/tuples_processed_count", aligner: "ALIGN_RATE", perDatabase: true},
	{suffix: "postgresql/temp_bytes_written_count", aligner: "ALIGN_RATE", perDatabase: true},
	{suffix: "postgresql/temp_files_written_count", aligner: "ALIGN_RATE", perDatabase: true},
	{suffix: "postgresql/deadlock_count", aligner: "ALIGN_RATE", perDatabase: true},
	{suffix: "postgresql/vacuum/oldest_transaction_age", aligner: "ALIGN_MEAN"},

	// Sanity: whether the instance was serving for the whole window. Not a
	// fixed parameter like the quotas below, so a dip is a real finding.
	{suffix: "up", aligner: "ALIGN_MEAN"},
}

// Intentionally absent, because benchmarks fix them and the values are
// therefore constant for the whole run. Every one is recorded in
// instance.json, which is the source of truth for a setting:
//
//	cpu/reserved_cores            the tier
//	memory/quota                  the tier
//	disk/quota                    disk_size_gb, with autoresize pinned off
//	disk/provisioning/iops        disk_provisioned_iops
//	disk/provisioning/throughput  disk_provisioned_throughput_mibps
//	instance_state                constant RUNNING; `up` covers a real dip
//	postgresql/num_backends       subsumed by num_backends_by_state
//	transaction_id_utilization    wraparound risk

// monitoringConcurrency bounds in-flight metric requests per window.
const monitoringConcurrency = 8

// maxTimeSeriesPages bounds pagination. A single metric over one benchmark
// window is a handful of series; anything approaching this means the filter
// is wrong.
const maxTimeSeriesPages = 20

// timeSeriesResponse is the part of a timeSeries.list reply this cares about.
type timeSeriesResponse struct {
	TimeSeries    []json.RawMessage `json:"timeSeries"`
	NextPageToken string            `json:"nextPageToken"`
}

// collectMonitoring writes one artifact per window holding every metric's
// series, keyed by metric suffix.
//
// Cloud Monitoring takes one metric type per request, because the aligner has
// to match the metric's kind, so this is a request per metric merged into one
// file. Metrics that fail individually are reported and the rest are kept.
func (c *Collector) collectMonitoring(ctx context.Context, req diagnostics.Request, w diagnostics.Window, dir string, res *diagnostics.Result) {
	// Queried concurrently: one request per metric type, 31 per window, and
	// sequentially that was most of a 31-second fetch. dbarenactl holds the
	// environment and keeps paying for it until fetch returns, so the
	// wall-clock cost is real money. Well inside the 6000 queries/minute
	// quota even at the widest fixture matrix.
	type outcome struct {
		series []json.RawMessage
		err    error
	}
	outcomes := make([]outcome, len(cloudSQLMetrics))
	sem := make(chan struct{}, monitoringConcurrency)
	var wg sync.WaitGroup
	for i, m := range cloudSQLMetrics {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			series, err := c.timeSeries(ctx, metricPrefix+m.suffix, m.aligner, m.perDatabase, w.Start, w.End)
			outcomes[i] = outcome{series, err}
		}()
	}
	wg.Wait()

	// Assembled in table order so the artifact and any warning read the same
	// way on every run.
	merged := make(map[string][]json.RawMessage, len(cloudSQLMetrics))
	var failed []string
	var withData int
	for i, m := range cloudSQLMetrics {
		if outcomes[i].err != nil {
			failed = append(failed, m.suffix)
			continue
		}
		// Recorded even when empty, so a reader can tell a metric this
		// instance does not publish from one that was never requested.
		merged[m.suffix] = outcomes[i].series
		if len(outcomes[i].series) > 0 {
			withData++
		}
	}

	if len(failed) > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("monitoring: %d of %d metrics failed for window %s: %v",
			len(failed), len(cloudSQLMetrics), w.Slug(), failed))
	}
	command := fmt.Sprintf("GET %s/v3/projects/%s/timeSeries (one request per metric type, %s aligned)",
		c.monitoringHost, c.projectID, alignmentPeriod)
	if withData == 0 {
		req.Save(res, dir, "monitoring.json", command, nil,
			fmt.Errorf("no data for window %s; Cloud Monitoring publishes about 165s late, so a window that just closed can still be empty", w.Slug()))
		return
	}

	data, err := json.MarshalIndent(merged, "", "  ")
	req.Save(res, dir, "monitoring.json", command, append(data, '\n'), err)
}

// timeSeries reads one metric type over one window, following pagination.
func (c *Collector) timeSeries(ctx context.Context, metricType, aligner string, perDatabase bool, start, end time.Time) ([]json.RawMessage, error) {
	database := ""
	if perDatabase {
		database = c.database
	}
	endpoint := c.monitoringHost + "/v3/projects/" + c.projectID + "/timeSeries"
	var all []json.RawMessage
	var token string
	for page := 0; page < maxTimeSeriesPages; page++ {
		body, _, err := c.get(ctx, endpoint, timeSeriesParams(
			c.databaseID, metricType, aligner, database,
			diagnostics.RFC3339(start), diagnostics.RFC3339(end), token))
		if err != nil {
			return nil, err
		}
		var parsed timeSeriesResponse
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("parse timeSeries response: %w", err)
		}
		all = append(all, parsed.TimeSeries...)
		if parsed.NextPageToken == "" {
			return all, nil
		}
		token = parsed.NextPageToken
	}
	return all, fmt.Errorf("stopped after %d pages", maxTimeSeriesPages)
}

// timeSeriesParams builds the query for one metric type and window.
func timeSeriesParams(databaseID, metricType, aligner, database, start, end, pageToken string) url.Values {
	filter := fmt.Sprintf(`metric.type = %q AND resource.labels.database_id = %q`, metricType, databaseID)
	// Narrow a per-database metric to the benchmark database, dropping the
	// three Cloud SQL keeps alongside it. Skipped when the database is
	// unknown: collecting all four beats collecting none.
	if database != "" {
		filter += fmt.Sprintf(` AND metric.labels.database = %q`, database)
	}
	v := url.Values{}
	v.Set("filter", filter)
	// Half-open at the start: Cloud Monitoring excludes startTime.
	v.Set("interval.startTime", start)
	v.Set("interval.endTime", end)
	v.Set("aggregation.alignmentPeriod", alignmentPeriod)
	v.Set("aggregation.perSeriesAligner", aligner)
	if pageToken != "" {
		v.Set("pageToken", pageToken)
	}
	return v
}
