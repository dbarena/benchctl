package engine_test

import (
	"context"
	"testing"

	"github.com/dbarena/benchctl/internal/schema"
)

func fixtureScenario2(fixtures []schema.FixtureDef, benchmarks []schema.SuiteEntry) *schema.Scenario {
	return &schema.Scenario{
		APIVersion: "bench/v1",
		Kind:       "Scenario",
		Metadata:   schema.Metadata{Name: "test"},
		Target:     schema.Target{Provider: "local"},
		Suite: schema.Suite{
			Fixtures:   fixtures,
			Benchmarks: benchmarks,
		},
		Driver:    schema.Driver{Provider: "local"},
		Collector: schema.Collector{Provider: "stdout"},
	}
}

func TestFixtures_BackwardsCompatible_NoFixtures(t *testing.T) {
	// Without fixtures the runner must behave exactly as before: Collect called once.
	var calls []string
	r, cap := makeCapturingRunner(&calls)
	s := fixtureScenario2(nil, []schema.SuiteEntry{
		{Name: "bench", Steps: []schema.SuiteStep{{Name: "run", Type: "go-tpc", Command: "run"}}},
	})
	if err := r.Run(context.Background(), s, schema.ResolvedInputs{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(cap.labels) != 1 {
		t.Errorf("Collect called %d times, want 1 (no fixtures)", len(cap.labels))
	}
}

func TestFixtures_CartesianProductCount(t *testing.T) {
	// 2 clients × 3 protocols = 6 invocations.
	var calls []string
	r, cap := makeCapturingRunner(&calls)
	s := fixtureScenario2(
		[]schema.FixtureDef{
			{Name: "clients", Type: schema.FixtureTypeRange, Params: map[string]any{"min": 1, "max": 2}},
			{Name: "proto", Type: schema.FixtureTypeList, Params: map[string]any{"values": []any{"a", "b", "c"}}},
		},
		[]schema.SuiteEntry{
			{Name: "bench", Steps: []schema.SuiteStep{{Name: "run", Type: "go-tpc", Command: "run"}}},
		},
	)
	if err := r.Run(context.Background(), s, schema.ResolvedInputs{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(cap.labels) != 6 {
		t.Errorf("Collect called %d times, want 6 (2×3 fixture combos)", len(cap.labels))
	}
}

func TestFixtures_LabelsInjected(t *testing.T) {
	// Each collect call must include fixture_clients and fixture_proto labels.
	var calls []string
	r, cap := makeCapturingRunner(&calls)
	s := fixtureScenario2(
		[]schema.FixtureDef{
			{Name: "clients", Type: schema.FixtureTypeConstant, Params: map[string]any{"value": "4"}},
			{Name: "proto", Type: schema.FixtureTypeConstant, Params: map[string]any{"value": "simple"}},
		},
		[]schema.SuiteEntry{
			{Name: "bench", Steps: []schema.SuiteStep{{Name: "run", Type: "go-tpc", Command: "run"}}},
		},
	)
	if err := r.Run(context.Background(), s, schema.ResolvedInputs{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(cap.labels) != 1 {
		t.Fatalf("Collect called %d times, want 1", len(cap.labels))
	}
	if cap.labels[0]["fixture_clients"] != "4" {
		t.Errorf("fixture_clients label = %v, want \"4\"", cap.labels[0]["fixture_clients"])
	}
	if cap.labels[0]["fixture_proto"] != "simple" {
		t.Errorf("fixture_proto label = %v, want \"simple\"", cap.labels[0]["fixture_proto"])
	}
}

func TestFixtures_MultipleBenchmarks(t *testing.T) {
	// 2 fixture combos × 2 benchmarks = 4 collect calls.
	var calls []string
	r, cap := makeCapturingRunner(&calls)
	s := fixtureScenario2(
		[]schema.FixtureDef{
			{Name: "n", Type: schema.FixtureTypeList, Params: map[string]any{"values": []any{"1", "2"}}},
		},
		[]schema.SuiteEntry{
			{Name: "b1", Steps: []schema.SuiteStep{{Name: "run", Type: "go-tpc", Command: "run"}}},
			{Name: "b2", Steps: []schema.SuiteStep{{Name: "run", Type: "go-tpc", Command: "run"}}},
		},
	)
	if err := r.Run(context.Background(), s, schema.ResolvedInputs{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 2 combos × 2 benchmarks = 4
	if len(cap.labels) != 4 {
		t.Errorf("Collect called %d times, want 4 (2 fixtures × 2 benchmarks)", len(cap.labels))
	}
}
