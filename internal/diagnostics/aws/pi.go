package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/dbarena/benchctl/internal/diagnostics"
)

const (
	// piPeriodSeconds is the aggregation grid. Only 1, 60, 300, 3600 and
	// 86400 are accepted; 60 lines up with CloudWatch.
	piPeriodSeconds = "60"

	// piMaxMetricQueries is the hard limit on --metric-queries per call.
	piMaxMetricQueries = 15

	// piTopN is how many SQL digests to rank. The API caps it at 25.
	piTopN = 25
)

// piCounters are the counters worth collecting on top of CloudWatch: Postgres
// internals (checkpoints, deadlocks, temp spill, transaction rates) plus the
// CPU breakdown and OOM-kill count AWS/RDS has no equivalent for.
//
// Total CPU, load average and free memory are absent: CPUUtilization and
// FreeableMemory already cover them.
//
// Every name here is one PI accepts; it rejects an unknown one outright with
// InvalidArgumentException, failing that batch. Some return no values on a
// given engine version (the db.Checkpoint family is empty on PG 17, which
// moved those counters to pg_stat_checkpointer), which is harmless.
var piCounters = []string{
	"os.cpuUtilization.user.avg",
	"os.cpuUtilization.wait.avg",
	"os.tasks.blocked.avg",
	"db.Transactions.xact_commit.avg",
	"db.Transactions.xact_rollback.avg",
	"db.Transactions.active_transactions.avg",
	"db.Transactions.blocked_transactions.avg",
	"db.Checkpoint.checkpoints_timed.avg",
	"db.Checkpoint.checkpoints_req.avg",
	"db.Checkpoint.checkpoint_write_latency.avg",
	"db.Checkpoint.buffers_checkpoint.avg",
	"db.IO.blks_read.avg",
	"db.IO.read_latency.avg",
	"db.IO.buffers_backend.avg",
	"db.Temp.temp_bytes.avg",
	"db.Temp.temp_files.avg",
	"db.Concurrency.deadlocks.avg",
	"db.User.numbackends.avg",
	"db.WAL.archived_count.avg",
}

// piMetricQuery is one entry of --metric-queries.
type piMetricQuery struct {
	Metric  string        `json:"Metric"`
	GroupBy *piDimensions `json:"GroupBy,omitempty"`
}

type piDimensions struct {
	Group      string   `json:"Group"`
	Dimensions []string `json:"Dimensions,omitempty"`
	Limit      int      `json:"Limit,omitempty"`
}

// piArgs is the common prefix for every pi call against this instance.
func (c *Collector) piArgs(op string, w diagnostics.Window) []string {
	return []string{
		"pi", op,
		"--service-type", "RDS",
		"--identifier", c.resourceID,
		"--start-time", rfc3339(w.Start),
		"--end-time", rfc3339(w.End),
	}
}

// collectPILoad captures average active sessions sliced by wait event, the
// console's stacked DB Load chart. It is what turns "throughput dropped" into
// "it was waiting on IO:DataFileRead".
func (c *Collector) collectPILoad(ctx context.Context, req diagnostics.Request, w diagnostics.Window, dir string, res *diagnostics.Result) {
	query := []piMetricQuery{{
		Metric: "db.load.avg",
		GroupBy: &piDimensions{
			Group:      "db.wait_event",
			Dimensions: []string{"db.wait_event.name", "db.wait_event.type"},
			Limit:      7,
		},
	}}
	payload, err := json.Marshal(query)
	if err != nil {
		c.save(res, req, dir, "pi_db_load.json", "", nil, err)
		return
	}
	args := append(c.piArgs("get-resource-metrics", w),
		"--metric-queries", string(payload),
		"--period-in-seconds", piPeriodSeconds,
		// The default stamps each point with its bucket's end, shifting
		// every series a minute late.
		"--period-alignment", "START_TIME",
	)
	out, argv, err := c.run(ctx, args...)
	c.save(res, req, dir, "pi_db_load.json", argv, out, err)
}

// collectPITopSQL ranks SQL digests by load, with call rate and latency.
//
// Every response value is truncated at 500 bytes, so a long statement comes
// back clipped; the digest id is recorded so get-dimension-key-details can
// recover the full text by hand.
func (c *Collector) collectPITopSQL(ctx context.Context, req diagnostics.Request, w diagnostics.Window, dir string, res *diagnostics.Result) {
	groupBy, err := json.Marshal(piDimensions{
		Group:      "db.sql_tokenized",
		Dimensions: []string{"db.sql_tokenized.id", "db.sql_tokenized.statement"},
		Limit:      piTopN,
	})
	if err != nil {
		c.save(res, req, dir, "pi_top_sql.json", "", nil, err)
		return
	}
	args := append(c.piArgs("describe-dimension-keys", w),
		// Only db.load.avg and db.sampledload.avg are accepted here.
		"--metric", "db.load.avg",
		"--group-by", string(groupBy),
		"--period-in-seconds", piPeriodSeconds,
		"--additional-metrics",
		"db.sql_tokenized.stats.calls_per_sec.avg",
		"db.sql_tokenized.stats.avg_latency_per_call.avg",
		"db.sql_tokenized.stats.rows_per_sec.avg",
		"--max-results", strconv.Itoa(piTopN),
	)
	out, argv, err := c.run(ctx, args...)
	c.save(res, req, dir, "pi_top_sql.json", argv, out, err)
}

// collectPICounters captures counter metrics, batched to the API's
// 15-per-call limit.
func (c *Collector) collectPICounters(ctx context.Context, req diagnostics.Request, w diagnostics.Window, dir string, res *diagnostics.Result) {
	for i, batch := range chunk(piCounters, piMaxMetricQueries) {
		queries := make([]piMetricQuery, 0, len(batch))
		for _, m := range batch {
			queries = append(queries, piMetricQuery{Metric: m})
		}
		payload, err := json.Marshal(queries)
		name := fmt.Sprintf("pi_counters_%d.json", i+1)
		if err != nil {
			c.save(res, req, dir, name, "", nil, err)
			continue
		}
		args := append(c.piArgs("get-resource-metrics", w),
			"--metric-queries", string(payload),
			"--period-in-seconds", piPeriodSeconds,
			"--period-alignment", "START_TIME",
		)
		out, argv, err := c.run(ctx, args...)
		c.save(res, req, dir, name, argv, out, err)
	}
}

// collectPIMetadata records which features the instance has, so an empty
// top-SQL artifact can be told apart from a disabled one.
func (c *Collector) collectPIMetadata(ctx context.Context, req diagnostics.Request, res *diagnostics.Result) {
	out, argv, err := c.run(ctx, "pi", "get-resource-metadata", "--service-type", "RDS", "--identifier", c.resourceID)
	c.save(res, req, req.Dest, "pi_resource_metadata.json", argv, out, err)
}

func chunk(s []string, size int) [][]string {
	var out [][]string
	for i := 0; i < len(s); i += size {
		end := min(i+size, len(s))
		out = append(out, s[i:end])
	}
	return out
}
