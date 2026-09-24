package hostmetrics

import (
	"fmt"
	"io"
	"math"
	"os"
	"time"

	"github.com/dbarena/benchctl/internal/engine"
)

// DefaultLogPath is where Vector's file sink writes host metrics, per the
// path hardcoded in deployments/*/vector.yaml.tftpl.
const DefaultLogPath = "/var/log/vector/metrics.log"

// quantiles are the percentiles reported for each metric family, paired
// with the label value used to tag them.
var quantiles = []struct {
	label string
	q     float64
}{
	{"0.99", 0.99},
	{"0.999", 0.999},
	{"0.9999", 0.9999},
}

// Cursor reduces Vector's cumulative host metrics log to CPU/network
// utilization percentiles, once per call. Driver.Collect fires once per
// benchmark entry, per fixture, per iteration within a single run -- all
// reading the same ever-growing log file -- so a Cursor remembers the
// latest sample timestamp it has already reported on and only considers
// samples strictly after it on the next call. Without this, every fixture
// after the first would get percentiles diluted by every fixture that ran
// before it, instead of numbers scoped to its own execution window. The
// zero value starts from the beginning of the log, so a fresh Cursor per
// run (one per driver Provider instance) behaves correctly on its first
// call.
type Cursor struct {
	lastSeen time.Time
}

// Collect reads Vector's local metrics log at path and reduces the samples
// observed since the previous call on this Cursor (or since the beginning
// of the log, on the first call) to p99/p99.9/p99.99 MetricPoints. It
// returns (nil, nil), not an error, when path does not exist: Vector may be
// disabled, may have failed to install (cloud-init treats it as a
// best-effort sidecar), or the caller may be a driver type nobody
// configured for metrics -- none of those are failures of the benchmark
// run.
func (c *Cursor) Collect(path string) ([]engine.MetricPoint, error) {
	if path == "" {
		path = DefaultLogPath
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}

	all, err := ParseFile(path)
	if err != nil {
		return nil, fmt.Errorf("hostmetrics: %w", err)
	}

	since := c.lastSeen
	samples := make([]Sample, 0, len(all))
	for _, s := range all {
		if s.Timestamp.After(since) {
			samples = append(samples, s)
		}
		if s.Timestamp.After(c.lastSeen) {
			c.lastSeen = s.Timestamp
		}
	}

	// Rounded to the precision that's actually meaningful: a fraction of a
	// percentage point of utilization, and whole bytes/sec (sub-byte
	// throughput isn't a real quantity).
	const utilizationDecimals = 4
	const throughputDecimals = 0

	var points []engine.MetricPoint
	points = append(points, quantilePoints("driver_cpu_utilization", nil, CPUUtilization(samples), utilizationDecimals)...)
	rx, tx := NetworkThroughput(samples)
	points = append(points, quantilePoints("driver_network_throughput_bytes_per_sec", map[string]string{"direction": "receive"}, rx, throughputDecimals)...)
	points = append(points, quantilePoints("driver_network_throughput_bytes_per_sec", map[string]string{"direction": "transmit"}, tx, throughputDecimals)...)
	return points, nil
}

func quantilePoints(family string, extraLabels map[string]string, values []float64, decimals int) []engine.MetricPoint {
	if len(values) == 0 {
		return nil
	}
	pts := make([]engine.MetricPoint, 0, len(quantiles))
	for _, qq := range quantiles {
		labels := map[string]string{"quantile": qq.label}
		for k, v := range extraLabels {
			labels[k] = v
		}
		pts = append(pts, engine.MetricPoint{
			Family: family,
			Labels: labels,
			Value:  round(percentile(values, qq.q), decimals),
		})
	}
	return pts
}

// round rounds v to the given number of decimal places.
func round(v float64, decimals int) float64 {
	p := math.Pow(10, float64(decimals))
	return math.Round(v*p) / p
}

// AppendTo runs Collect against path (DefaultLogPath if empty) and folds
// the resulting points into metrics' StructuredMetrics, in the same
// append-not-replace style as engine's own step-to-step metric merging.
// Read/parse errors are logged to warn (which may be nil to discard them)
// and never surface as an error: a driver without Vector configured is the
// common case, not a failure, and this must never fail driver.collect.
func (c *Cursor) AppendTo(metrics engine.Metrics, path string, warn io.Writer) engine.Metrics {
	points, err := c.Collect(path)
	if err != nil {
		if warn != nil {
			fmt.Fprintf(warn, "warning: hostmetrics: %v\n", err)
		}
		return metrics
	}
	if len(points) == 0 {
		return metrics
	}
	if metrics == nil {
		metrics = engine.Metrics{}
	}
	sm, _ := metrics[engine.StructuredKey].(engine.StructuredMetrics)
	sm.Points = append(sm.Points, points...)
	metrics[engine.StructuredKey] = sm
	return metrics
}
