package engine

import (
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dbarena/benchctl/internal/schema"
)

// collectMetadata runs one `type: metadata` step and returns the value to
// record under the step's args.name.
func collectMetadata(ctx context.Context, out io.Writer, outputs Outputs, command, name, query string) (string, error) {
	switch command {
	case "", schema.MetadataCommandSQL:
		return collectTargetInfo(ctx, out, outputs, name, query)
	case schema.MetadataCommandRTT:
		return collectTargetRTT(ctx, out, outputs, name)
	default:
		return "", fmt.Errorf("metadata.%s: unknown command %q (want %q or %q)", name, command, schema.MetadataCommandSQL, schema.MetadataCommandRTT)
	}
}

// collectTargetInfo executes a single SQL query against the target database
// and returns the result string. The result is logged to out (if non-nil).
func collectTargetInfo(ctx context.Context, out io.Writer, outputs Outputs, name, query string) (string, error) {
	connStr, err := buildConnString(outputs)
	if err != nil {
		return "", err
	}
	conn, err := pgx.Connect(ctx, connStr)
	if err != nil {
		return "", fmt.Errorf("connect to target: %w", err)
	}
	defer conn.Close(ctx)

	var val string
	if err := conn.QueryRow(ctx, query).Scan(&val); err != nil {
		return "", fmt.Errorf("info.%s: %w", name, err)
	}
	if out != nil {
		fmt.Fprintf(out, "  %s: %s\n", name, val)
	}
	return val, nil
}

// buildConnString builds a postgres:// URL from target provider outputs.
// All connection params (host, port, user, password, db) are required. A
// missing value is a scenario authoring bug and surfaces as an error rather
// than a silent fallback that connects to the wrong database.
func buildConnString(outputs Outputs) (string, error) {
	required := []string{"host", "port", "user", "password", "db"}
	var missing []string
	for _, k := range required {
		if outputs[k] == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("target connection params missing: %v (declare them in the scenario's service vars)", missing)
	}
	sslmode := outputs["sslmode"]
	if sslmode == "" {
		sslmode = "disable"
	}
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(outputs["user"], outputs["password"]),
		Host:   net.JoinHostPort(outputs["host"], outputs["port"]),
		Path:   "/" + outputs["db"],
	}
	q := u.Query()
	q.Set("sslmode", sslmode)
	if sslnegotiation := outputs["sslnegotiation"]; sslnegotiation != "" {
		q.Set("sslnegotiation", sslnegotiation)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// rttWarmupProbes round trips are discarded before timing starts, so that
// connection setup and pgx's first-use statement preparation don't land in the
// samples. After warm-up a "SELECT 1" is a single round trip.
const rttWarmupProbes = 20

// rttTimedProbes is the number of timed round trips collectTargetRTT takes.
const rttTimedProbes = 200

// collectTargetRTT measures the round trip between wherever benchctl runs (the
// load driver) and the target database, and returns a summary string in the
// same "key=value, ..." shape the pg_settings metadata step produces.
//
// It exists as a colocation check: the driver is meant to be pinned to the same
// zone as the target, and a run where that failed should be visible in the
// published Result rather than inferred afterwards from throughput.
// Values are microseconds, because the differences worth catching are a
// fraction of a millisecond.
func collectTargetRTT(ctx context.Context, out io.Writer, outputs Outputs, name string) (string, error) {
	connStr, err := buildConnString(outputs)
	if err != nil {
		return "", err
	}
	conn, err := pgx.Connect(ctx, connStr)
	if err != nil {
		return "", fmt.Errorf("connect to target: %w", err)
	}
	defer conn.Close(ctx)

	probe := func() error {
		var one int
		return conn.QueryRow(ctx, "SELECT 1").Scan(&one)
	}
	for i := 0; i < rttWarmupProbes; i++ {
		if err := probe(); err != nil {
			return "", fmt.Errorf("rtt.%s: warmup probe %d: %w", name, i, err)
		}
	}
	samples := make([]time.Duration, 0, rttTimedProbes)
	for i := 0; i < rttTimedProbes; i++ {
		start := time.Now()
		if err := probe(); err != nil {
			return "", fmt.Errorf("rtt.%s: probe %d: %w", name, i, err)
		}
		samples = append(samples, time.Since(start))
	}

	val := formatRTT(samples)
	if out != nil {
		fmt.Fprintf(out, "  %s: %s\n", name, val)
	}
	return val, nil
}

// formatRTT summarises round-trip samples as microseconds. It reports min
// alongside the percentiles because min is the figure least polluted by
// scheduling noise on the driver, and so the most comparable across runs.
func formatRTT(samples []time.Duration) string {
	if len(samples) == 0 {
		return "samples=0"
	}
	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	slices.Sort(sorted)

	us := func(d time.Duration) int64 { return d.Microseconds() }
	return fmt.Sprintf("min_us=%d, median_us=%d, p99_us=%d, max_us=%d, samples=%d",
		us(sorted[0]),
		us(percentile(sorted, 0.50)),
		us(percentile(sorted, 0.99)),
		us(sorted[len(sorted)-1]),
		len(sorted))
}

// percentile returns the q-th percentile of an already-sorted slice using the
// nearest-rank method. q is clamped to (0, 1].
func percentile(sorted []time.Duration, q float64) time.Duration {
	if q <= 0 {
		return sorted[0]
	}
	if q > 1 {
		q = 1
	}
	rank := int(math.Ceil(q * float64(len(sorted))))
	return sorted[rank-1]
}
