// Package hostmetrics reduces Vector's host_metrics output on the load
// driver to CPU/network utilization percentiles for a benchmark run. It
// only understands Vector's file-sink JSON encoding (see
// deployments/*/vector.yaml.tftpl) and is agnostic to which workload adapter
// (go-tpc, k6, ...) is running: activation is gated purely on whether the
// log file exists, not on driver type.
package hostmetrics

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

// Sample is one host_metrics counter observation relevant to CPU/network
// utilization, decoded from a line of Vector's file-sink JSON output.
type Sample struct {
	Name      string
	Tags      map[string]string
	Timestamp time.Time
	Value     float64
}

// relevantMetricNames are the host_metrics counter names this package
// understands; every other line in the log (disk, memory, process,
// internal_metrics, ...) is ignored.
var relevantMetricNames = map[string]bool{
	metricCPUSecondsTotal:           true,
	metricNetworkReceiveBytesTotal:  true,
	metricNetworkTransmitBytesTotal: true,
}

// rawEvent mirrors the JSON shape Vector's file sink emits for a metric
// event with encoding.codec: json, confirmed by running Vector locally
// against deployments/ec2/loaddriver_gotpc/vector.yaml.tftpl, e.g.:
//
//	{"name":"cpu_seconds_total","namespace":"host",
//	 "tags":{"collector":"cpu","cpu":"0","host":"...","mode":"idle"},
//	 "timestamp":"2026-09-24T12:11:02.540933Z","kind":"absolute",
//	 "counter":{"value":182184.54}}
type rawEvent struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	Tags      map[string]string `json:"tags"`
	Timestamp time.Time         `json:"timestamp"`
	Counter   *struct {
		Value float64 `json:"value"`
	} `json:"counter"`
}

// ParseFile streams path -- Vector's NDJSON file-sink output -- and returns
// every host_metrics cpu/network counter sample it contains.
func ParseFile(path string) ([]Sample, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	return Parse(f)
}

// Parse is the io.Reader-based core of ParseFile, split out so tests can
// exercise it against in-memory fixtures. Lines that aren't valid JSON,
// aren't a "host" namespace counter, or aren't a relevant metric name are
// skipped rather than failing the whole parse.
func Parse(r io.Reader) ([]Sample, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var samples []Sample
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev rawEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.Namespace != "host" || ev.Counter == nil || !relevantMetricNames[ev.Name] {
			continue
		}
		samples = append(samples, Sample{
			Name:      ev.Name,
			Tags:      ev.Tags,
			Timestamp: ev.Timestamp,
			Value:     ev.Counter.Value,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}
	return samples, nil
}
