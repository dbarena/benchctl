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
