package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/dbarena/benchctl/internal/runstate"
)

// TestStateWireFormatRoundTrip guards the machine-to-machine format `state
// export` writes and `state import` reads. Every State field has to survive
// it: a field that marshals but does not unmarshal (or one added without a
// JSON tag) would silently disappear when the orchestrator reads a run back
// from its driver instance.
func TestStateWireFormatRoundTrip(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)

	original := &runstate.State{
		RunID:          "run-a",
		ScenarioName:   "scenario",
		ScenarioPath:   "scenarios/x.yaml",
		TargetProvider: "opentofu",
		DriverProvider: "ec2",
		StartedAt:      now,
		Inputs:         map[string]any{"warehouses": float64(10)},
		Phases: map[runstate.Phase]runstate.Status{
			runstate.PhaseProvision:       runstate.StatusCompleted,
			runstate.PhaseWorkloadExecute: runstate.StatusRunning,
		},
		TargetOutputs:    map[string]string{"host": "10.0.0.1"},
		DriverOutputs:    map[string]string{"public_ip": "1.2.3.4"},
		CompletedAt:      &later,
		TerminatedAt:     &later,
		Error:            "boom",
		CreatedBy:        "user-uuid",
		CreatedByEmail:   "user@example.com",
		LastHeartbeat:    &now,
		Metadata:         map[string]string{"effective_date": "2026-03-01"},
		CurrentFixture:   map[string]string{"clients": "8"},
		CurrentIteration: 2,
		CurrentStep:      "benchmark",
		StepStartedAt:    &now,
		TofuState:        "blob",
	}

	// Every persisted field must be set above, or the round trip proves
	// nothing about the ones that are not. Fields tagged json:"-" are
	// deliberately not part of the wire format.
	v := reflect.ValueOf(*original)
	for i := range v.NumField() {
		field := v.Type().Field(i)
		if field.Tag.Get("json") == "-" {
			continue
		}
		if v.Field(i).IsZero() {
			t.Errorf("field %s is unset; add it to this fixture", field.Name)
		}
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(original); err != nil {
		t.Fatalf("encode: %v", err)
	}

	var decoded runstate.State
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(*original, decoded) {
		t.Errorf("round trip changed the record:\n got %+v\nwant %+v", decoded, *original)
	}
}
