package engine

import (
	"testing"
)

func TestMergeInfoIntoMetrics_PopulatesInfoLabels(t *testing.T) {
	info := map[string]string{
		"pg_version":        "PostgreSQL 17.1",
		"extension_version": "acme 1.2",
	}
	sm := StructuredMetrics{
		Points: []MetricPoint{{Family: "tpcc_tpm", Value: 100}},
	}
	m := Metrics{StructuredKey: sm}

	mergeInfoIntoMetrics(info, &m)

	got, ok := m[StructuredKey].(StructuredMetrics)
	if !ok {
		t.Fatal("StructuredKey missing after merge")
	}
	for k, want := range info {
		if got.InfoLabels[k] != want {
			t.Errorf("InfoLabels[%q] = %q, want %q", k, got.InfoLabels[k], want)
		}
		if m[k] != want {
			t.Errorf("flat metrics[%q] = %v, want %q", k, m[k], want)
		}
	}
}

func TestMergeInfoIntoMetrics_NilMetrics(t *testing.T) {
	info := map[string]string{"pg_version": "PostgreSQL 17.1"}
	var m Metrics

	mergeInfoIntoMetrics(info, &m)

	if m["pg_version"] != "PostgreSQL 17.1" {
		t.Errorf("flat metrics[pg_version] = %v, want PostgreSQL 17.1", m["pg_version"])
	}
}

func TestMergeInfoIntoMetrics_EmptyInfo(t *testing.T) {
	m := Metrics{"existing": "value"}
	mergeInfoIntoMetrics(nil, &m)
	if len(m) != 1 {
		t.Errorf("metrics should be unchanged, got %v", m)
	}
}

func TestMergeInfoIntoMetrics_NoStructuredKey(t *testing.T) {
	info := map[string]string{"pg_version": "PostgreSQL 17.1"}
	m := Metrics{}

	mergeInfoIntoMetrics(info, &m)

	if m["pg_version"] != "PostgreSQL 17.1" {
		t.Errorf("flat metrics[pg_version] = %v", m["pg_version"])
	}
	if _, ok := m[StructuredKey]; ok {
		t.Error("StructuredKey should not be created when absent")
	}
}

// TestMergeMetrics_MultipleStepsStayDistinguishable reproduces a benchmark
// entry with a "warm-up" go-tpc step followed by a "benchmark" go-tpc step:
// both contribute a tpcc_tpm point for NEW_ORDER with identical labels
// before step tagging. Without labelStepMetrics, mergeMetrics's append would
// leave two indistinguishable points.
func TestMergeMetrics_MultipleStepsStayDistinguishable(t *testing.T) {
	warmup := Metrics{
		StructuredKey: StructuredMetrics{
			Points: []MetricPoint{{Family: "tpcc_tpm", Labels: map[string]string{"transaction": "NEW_ORDER", "status": "ok"}, Value: 50620.9}},
		},
	}
	benchmark := Metrics{
		StructuredKey: StructuredMetrics{
			Points: []MetricPoint{{Family: "tpcc_tpm", Labels: map[string]string{"transaction": "NEW_ORDER", "status": "ok"}, Value: 59452.0}},
		},
	}

	labelStepMetrics(warmup, "warm-up")
	labelStepMetrics(benchmark, "benchmark")

	var dst Metrics
	mergeMetrics(&dst, warmup)
	mergeMetrics(&dst, benchmark)

	sm, ok := dst[StructuredKey].(StructuredMetrics)
	if !ok || len(sm.Points) != 2 {
		t.Fatalf("expected 2 merged points, got %#v", dst[StructuredKey])
	}
	byStep := map[string]float64{}
	for _, p := range sm.Points {
		byStep[p.Labels["step"]] = p.Value
	}
	if byStep["warm-up"] != 50620.9 || byStep["benchmark"] != 59452.0 {
		t.Errorf("points not distinguishable by step label: %#v", sm.Points)
	}
}

// TestMergeMetrics_RawSamplesCSVKeptSeparatePerStep reproduces the same
// warm-up/benchmark pairing for raw_samples_csv: without labelStepMetrics
// namespacing the key by step, mergeMetrics's flat-key overwrite would
// silently drop the warm-up step's CSV once the benchmark step merges in.
func TestMergeMetrics_RawSamplesCSVKeptSeparatePerStep(t *testing.T) {
	warmup := Metrics{"raw_samples_csv": "t_seconds,transaction,tpm\n1.0,NEW_ORDER,500\n2.0,NEW_ORDER,510\n"}
	benchmark := Metrics{"raw_samples_csv": "t_seconds,transaction,tpm\n11.0,NEW_ORDER,590\n"}

	labelStepMetrics(warmup, "warm-up")
	labelStepMetrics(benchmark, "benchmark")

	var dst Metrics
	mergeMetrics(&dst, warmup)
	mergeMetrics(&dst, benchmark)

	if _, ok := dst["raw_samples_csv"]; ok {
		t.Errorf("bare raw_samples_csv key should not survive labelStepMetrics, got %v", dst["raw_samples_csv"])
	}
	if got, _ := dst[RawSamplesCSVKey("warm-up")].(string); got != "t_seconds,transaction,tpm\n1.0,NEW_ORDER,500\n2.0,NEW_ORDER,510\n" {
		t.Errorf("warm-up raw_samples_csv = %q", got)
	}
	if got, _ := dst[RawSamplesCSVKey("benchmark")].(string); got != "t_seconds,transaction,tpm\n11.0,NEW_ORDER,590\n" {
		t.Errorf("benchmark raw_samples_csv = %q", got)
	}
}
