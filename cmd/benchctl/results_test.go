package main

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func sample(labels map[string]string, v float64) vmSample {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	raw, _ := json.Marshal(s)
	return vmSample{Metric: labels, Value: [2]json.RawMessage{json.RawMessage("0"), raw}}
}

func pgbenchSample(name, step, clients string, extra map[string]string, v float64) vmSample {
	labels := map[string]string{"__name__": name, "benchmark": "postgres", "step": step, "fixture_clients": clients}
	for k, val := range extra {
		labels[k] = val
	}
	return sample(labels, v)
}

func TestRenderResults_Pgbench(t *testing.T) {
	samples := []vmSample{
		pgbenchSample("pgbench_tps", "read-only", "8", nil, 12345.678),
		pgbenchSample("pgbench_transactions", "read-only", "8", nil, 740740),
		pgbenchSample("pgbench_failed_transactions", "read-only", "8", nil, 0),
		pgbenchSample("pgbench_latency_ms", "read-only", "8", map[string]string{"stat": "avg"}, 0.648),
		pgbenchSample("pgbench_latency_ms", "read-only", "8", map[string]string{"stat": "stddev"}, 0.120),
		pgbenchSample("pgbench_duration_seconds", "read-only", "8", nil, 60),
		pgbenchSample("pgbench_tps", "read-only", "16", nil, 20000),
		pgbenchSample("pgbench_transactions", "read-only", "16", nil, 1200000),
	}
	var buf bytes.Buffer
	renderResults(&buf, "run-1", samples)
	out := buf.String()

	for _, want := range []string{
		"benchmark: postgres",
		"Step",
		"read-only [clients=8]",
		"12345.7",
		"0.648",
		"0.120",
		"740740",
		"60s",
		"read-only [clients=16]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// A row whose pgbench printed no latency or failed count shows "-", not 0.
	var row []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "read-only [clients=16]") {
			row = strings.Fields(line)
		}
	}
	// read-only, [clients=16], TPS, avg, stddev, transactions, failed
	if len(row) != 7 || row[3] != "-" || row[4] != "-" || row[6] != "-" {
		t.Errorf("row without latency/failed figures = %q, want '-' in their columns", row)
	}
	if strings.Contains(out, "Transaction\tTPM") || strings.Contains(out, "NEW_ORDER") {
		t.Errorf("TPC-C table rendered for a pgbench-only run:\n%s", out)
	}
}
