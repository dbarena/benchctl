//go:build integration

package victoriametrics_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/dbarena/benchctl/internal/collectors/victoriametrics"
	"github.com/dbarena/benchctl/internal/config"
	"github.com/dbarena/benchctl/internal/engine"
)

// TestCollect_Integration pushes a synthetic TPC-C result set to the local
// VictoriaMetrics instance (http://127.0.0.1:8428) and verifies the push
// succeeds. Requires the observability stack: mise run observability-up.
func TestCollect_Integration(t *testing.T) {
	const endpoint = "http://127.0.0.1:8428"
	resp, err := http.Get(endpoint + "/health")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("VictoriaMetrics not reachable at %s; run: mise run observability-up", endpoint)
	}
	resp.Body.Close()

	sm := engine.StructuredMetrics{
		InfoLabels: map[string]string{
			"pg_version": "PostgreSQL 17.9 (Debian 17.9-1.pgdg13+1)",
		},
		Points: []engine.MetricPoint{
			{Family: "tpcc_tpm", Labels: map[string]string{"transaction": "NEW_ORDER", "status": "ok"}, Value: 27065.8},
			{Family: "tpcc_tpm", Labels: map[string]string{"transaction": "PAYMENT", "status": "ok"}, Value: 25903.0},
			{Family: "tpcc_tpm", Labels: map[string]string{"transaction": "DELIVERY", "status": "ok"}, Value: 2473.7},
			{Family: "tpcc_latency_ms", Labels: map[string]string{"transaction": "NEW_ORDER", "status": "ok", "quantile": "p99"}, Value: 5.8},
			{Family: "tpcc_latency_ms", Labels: map[string]string{"transaction": "NEW_ORDER", "status": "ok", "quantile": "max"}, Value: 104.9},
			{Family: "tpcc_count", Labels: map[string]string{"transaction": "NEW_ORDER", "status": "ok"}, Value: 27053},
			{Family: "tpcc_duration_seconds", Labels: map[string]string{"transaction": "NEW_ORDER", "status": "ok"}, Value: 60},
			{Family: "tpcc_tpm", Labels: map[string]string{"transaction": "DELIVERY", "status": "error"}, Value: 1},
		},
	}
	metrics := engine.Metrics{engine.StructuredKey: sm}

	cfg := map[string]any{
		"run_id":   "smoke-test-20260427-aabbcc",
		"scenario": "acme-tpcc-local",
		"labels": map[string]any{
			"engine": "acme",
		},
	}

	benchctlCfg := &config.Config{Metrics: config.MetricsConfig{Endpoint: endpoint}}
	c := victoriametrics.New(benchctlCfg)
	if err := c.Collect(context.Background(), cfg, metrics); err != nil {
		t.Fatalf("Collect: %v", err)
	}
}
