package victoriametrics

import (
	"strings"
	"testing"

	"github.com/dbarena/benchctl/internal/engine"
)

// toPrometheusLines only ever reads metrics[engine.StructuredKey]. It never
// iterates flat keys such as "raw_output" and "raw_samples_csv" (a large CSV
// blob, see internal/adapters/gotpc), so they can't leak into a
// pushed metric name, label, or value regardless of what an adapter stuffs
// into the flat side of an engine.Metrics map.
func TestToPrometheusLines_IgnoresFlatKeys(t *testing.T) {
	metrics := engine.Metrics{
		"raw_output":      "some multi-line\ngo-tpc stdout capture",
		"raw_samples_csv": "t_seconds,transaction,status,tpm\n1.0,NEW_ORDER,ok,600\n",
		engine.StructuredKey: engine.StructuredMetrics{
			Points: []engine.MetricPoint{
				{Family: "tpcc_tpm", Labels: map[string]string{"transaction": "NEW_ORDER", "status": "ok"}, Value: 600},
			},
		},
	}

	lines, infoLabels := toPrometheusLines(metrics, nil, 1000)
	if len(lines) != 1 {
		t.Fatalf("want 1 line (from the structured point only), got %d: %v", len(lines), lines)
	}
	if !strings.HasPrefix(lines[0], "tpcc_tpm{") {
		t.Errorf("line = %q, want it to start with tpcc_tpm{", lines[0])
	}
	for _, line := range lines {
		if strings.Contains(line, "raw_samples_csv") || strings.Contains(line, "t_seconds") {
			t.Errorf("flat key content leaked into a pushed line: %q", line)
		}
	}
	if len(infoLabels) != 0 {
		t.Errorf("infoLabels = %v, want empty (no InfoLabels set on the StructuredMetrics)", infoLabels)
	}
}

func TestToPrometheusLines_NoStructuredMetrics_ReturnsEmpty(t *testing.T) {
	metrics := engine.Metrics{"raw_output": "text", "raw_samples_csv": "csv"}
	lines, infoLabels := toPrometheusLines(metrics, nil, 1000)
	if len(lines) != 0 {
		t.Errorf("lines = %v, want empty when no StructuredMetrics present", lines)
	}
	if len(infoLabels) != 0 {
		t.Errorf("infoLabels = %v, want empty", infoLabels)
	}
}
