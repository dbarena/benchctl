package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dbarena/benchctl/internal/diagnostics"
)

// cloudWatchPeriod is the requested resolution, in seconds. 60 is the finest
// RDS publishes.
const cloudWatchPeriod = 60

// rdsMetrics are the AWS/RDS metrics captured.
//
// Absent by choice: BurstBalance (gp2 only), the *LocalStorage family (needs a local-NVMe class), and DBLoad* (captured in pi.go via wait-event detail).
var rdsMetrics = []struct {
	name string
	stat string
}{
	{"CPUUtilization", "Average"},
	// Burstable (db.t*) only, every 5 minutes. Empty elsewhere.
	{"CPUCreditBalance", "Average"},
	{"CPUSurplusCreditBalance", "Average"},
	{"DatabaseConnections", "Average"},
	{"FreeableMemory", "Average"},
	{"FreeStorageSpace", "Average"},
	{"SwapUsage", "Average"},
	{"ReadIOPS", "Average"},
	{"WriteIOPS", "Average"},
	{"ReadThroughput", "Average"},
	{"WriteThroughput", "Average"},
	// Seconds
	{"ReadLatency", "Average"},
	{"WriteLatency", "Average"},
	{"DiskQueueDepth", "Average"},
	{"NetworkReceiveThroughput", "Average"},
	{"NetworkTransmitThroughput", "Average"},
	{"EBSIOBalance%", "Average"},
	{"EBSByteBalance%", "Average"},
	// Postgres-specific.
	{"TransactionLogsDiskUsage", "Average"},
	{"TransactionLogsGeneration", "Average"},
	{"MaximumUsedTransactionIDs", "Maximum"},
}

// metricDataQuery is the shape --metric-data-queries expects.
type metricDataQuery struct {
	ID         string     `json:"Id"`
	Label      string     `json:"Label"`
	MetricStat metricStat `json:"MetricStat"`
}

type metricStat struct {
	Metric metricRef `json:"Metric"`
	Period int       `json:"Period"`
	Stat   string    `json:"Stat"`
	// Unit is never set: CloudWatch does not convert, and a mismatch returns
	// nulls with no error.
}

type metricRef struct {
	Namespace  string            `json:"Namespace"`
	MetricName string            `json:"MetricName"`
	Dimensions []metricDimension `json:"Dimensions"`
}

type metricDimension struct {
	Name  string `json:"Name"`
	Value string `json:"Value"`
}

// buildMetricDataQueries renders the query payload for one instance. Ids must
// start with a lowercase letter and be unique per call, hence m0, m1, ...
func buildMetricDataQueries(instanceID string) ([]metricDataQuery, error) {
	queries := make([]metricDataQuery, 0, len(rdsMetrics))
	for i, m := range rdsMetrics {
		queries = append(queries, metricDataQuery{
			ID:    fmt.Sprintf("m%d", i),
			Label: m.name,
			MetricStat: metricStat{
				Metric: metricRef{
					Namespace:  "AWS/RDS",
					MetricName: m.name,
					Dimensions: []metricDimension{{Name: "DBInstanceIdentifier", Value: instanceID}},
				},
				Period: cloudWatchPeriod,
				Stat:   m.stat,
			},
		})
	}
	if len(queries) > 500 {
		// CloudWatch's per-call limit; unreachable today, but the API error
		// would be cryptic.
		return nil, fmt.Errorf("%d metric queries exceeds CloudWatch's limit of 500 per call", len(queries))
	}
	return queries, nil
}

func (c *Collector) collectCloudWatch(ctx context.Context, req diagnostics.Request, w diagnostics.Window, dir string, res *diagnostics.Result) {
	queries, err := buildMetricDataQueries(c.instanceID)
	if err != nil {
		c.save(res, req, dir, "cloudwatch.json", "", nil, err)
		return
	}
	payload, err := json.Marshal(queries)
	if err != nil {
		c.save(res, req, dir, "cloudwatch.json", "", nil, err)
		return
	}

	out, argv, err := c.run(ctx,
		"cloudwatch", "get-metric-data",
		"--metric-data-queries", string(payload),
		"--start-time", rfc3339(w.Start),
		"--end-time", rfc3339(w.End),
		// The CLI defaults to descending.
		"--scan-by", "TimestampAscending",
	)
	if err == nil {
		err = warnOnPartialMetricData(out, res)
	}
	c.save(res, req, dir, "cloudwatch.json", argv, out, err)
}

// warnOnPartialMetricData surfaces per-query status from an otherwise
// successful response. Forbidden and PartialData are invisible in the exit
// code.
func warnOnPartialMetricData(out []byte, res *diagnostics.Result) error {
	var parsed struct {
		MetricDataResults []struct {
			Label      string `json:"Label"`
			StatusCode string `json:"StatusCode"`
		} `json:"MetricDataResults"`
		Messages []struct {
			Value string `json:"Value"`
		} `json:"Messages"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return fmt.Errorf("parse get-metric-data response: %w", err)
	}
	var bad []string
	for _, r := range parsed.MetricDataResults {
		if r.StatusCode != "" && r.StatusCode != "Complete" {
			bad = append(bad, r.Label+"="+r.StatusCode)
		}
	}
	if len(bad) > 0 {
		res.Warnings = append(res.Warnings, "cloudwatch returned incomplete series: "+strings.Join(bad, ", "))
	}
	for _, m := range parsed.Messages {
		res.Warnings = append(res.Warnings, "cloudwatch: "+m.Value)
	}
	return nil
}
