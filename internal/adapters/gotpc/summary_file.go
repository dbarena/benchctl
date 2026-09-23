package gotpc

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/dbarena/benchctl/internal/engine"
)

// summaryTransaction mirrors one entry of go-tpc's --summary-file JSON
// (pkg/measurement.OpSummary), read directly field by field with no text
// parsing.
type summaryTransaction struct {
	Transaction   string  `json:"transaction"`
	Status        string  `json:"status"`
	Count         int64   `json:"count"`
	TPM           float64 `json:"tpm"`
	TakesSeconds  float64 `json:"takes_s"`
	AvgLatencyMs  float64 `json:"avg_latency_ms"`
	P50LatencyMs  float64 `json:"p50_latency_ms"`
	P90LatencyMs  float64 `json:"p90_latency_ms"`
	P95LatencyMs  float64 `json:"p95_latency_ms"`
	P99LatencyMs  float64 `json:"p99_latency_ms"`
	P999LatencyMs float64 `json:"p99_9_latency_ms"`
	MaxLatencyMs  float64 `json:"max_latency_ms"`
}

// summaryDoc mirrors go-tpc's --summary-file document. Tpm/TpmTotal/
// EfficiencyPct are intentionally not modeled: they duplicate NEW_ORDER's
// own ok/tpm figure.
type summaryDoc struct {
	Transactions []summaryTransaction `json:"transactions"`
}

// readSummaryFile reads and translates a go-tpc --summary-file into
// engine.Metrics, structured under engine.StructuredKey.
func readSummaryFile(path string) (engine.Metrics, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read summary file %s: %w", path, err)
	}
	var doc summaryDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse summary file %s: %w", path, err)
	}
	return engine.Metrics{engine.StructuredKey: structureSummary(doc)}, nil
}

// structureSummary translates a summaryDoc into engine.StructuredMetrics
// using benchctl's tpcc_* Prometheus-style family names and quantile labels.
func structureSummary(doc summaryDoc) engine.StructuredMetrics {
	var sm engine.StructuredMetrics
	for _, tx := range doc.Transactions {
		point := func(family string, value float64, quantile string) {
			labels := map[string]string{"transaction": tx.Transaction, "status": tx.Status}
			if quantile != "" {
				labels["quantile"] = quantile
			}
			sm.Points = append(sm.Points, engine.MetricPoint{Family: family, Labels: labels, Value: value})
		}
		point("tpcc_tpm", tx.TPM, "")
		point("tpcc_count", float64(tx.Count), "")
		point("tpcc_duration_seconds", tx.TakesSeconds, "")
		point("tpcc_latency_ms", tx.P50LatencyMs, "p50")
		point("tpcc_latency_ms", tx.P90LatencyMs, "p90")
		point("tpcc_latency_ms", tx.P95LatencyMs, "p95")
		point("tpcc_latency_ms", tx.P99LatencyMs, "p99")
		point("tpcc_latency_ms", tx.P999LatencyMs, "p99_9")
		point("tpcc_latency_ms", tx.AvgLatencyMs, "avg")
		point("tpcc_latency_ms", tx.MaxLatencyMs, "max")
	}
	return sm
}
