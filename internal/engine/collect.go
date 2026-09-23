package engine

import (
	"context"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"
)

// collectPGMetrics runs a SQL query against the target database. The query must
// return rows of exactly two columns: (name text, value float8). Each row is
// converted to a MetricPoint with the given family and a "stat" label equal to
// the name column.
//
// Intended for post-benchmark instrumentation steps (type: collect) where the
// caller wants numeric metrics, e.g. pg_stat_checkpointer deltas, recorded
// alongside the workload results in the JSON file.
func collectPGMetrics(ctx context.Context, out io.Writer, outputs Outputs, stepName, family, query string) ([]MetricPoint, error) {
	if query == "" {
		return nil, fmt.Errorf("collect.%s: empty query", stepName)
	}
	connStr, err := buildConnString(outputs)
	if err != nil {
		return nil, err
	}
	conn, err := pgx.Connect(ctx, connStr)
	if err != nil {
		return nil, fmt.Errorf("collect.%s: connect: %w", stepName, err)
	}
	defer conn.Close(ctx)

	rows, err := conn.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("collect.%s: query: %w", stepName, err)
	}
	defer rows.Close()

	var pts []MetricPoint
	for rows.Next() {
		var name string
		var value float64
		if err := rows.Scan(&name, &value); err != nil {
			return nil, fmt.Errorf("collect.%s: scan: %w", stepName, err)
		}
		pts = append(pts, MetricPoint{
			Family: family,
			Labels: map[string]string{"stat": name},
			Value:  value,
		})
	}
	if out != nil {
		fmt.Fprintf(out, "  %s: collected %d metrics (family=%s)\n", stepName, len(pts), family)
	}
	return pts, rows.Err()
}
