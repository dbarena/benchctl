package schema_test

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/dbarena/benchctl/internal/schema"
)

// ---- FixtureDef YAML parsing ----

func parseFixtureDef(t *testing.T, src string) schema.FixtureDef {
	t.Helper()
	var fd schema.FixtureDef
	if err := yaml.Unmarshal([]byte(src), &fd); err != nil {
		t.Fatalf("UnmarshalYAML: %v", err)
	}
	return fd
}

func TestFixtureDef_ConstantShorthand(t *testing.T) {
	fd := parseFixtureDef(t, "scale: 10")
	if fd.Name != "scale" {
		t.Errorf("Name = %q, want scale", fd.Name)
	}
	if fd.Type != schema.FixtureTypeConstant {
		t.Errorf("Type = %q, want constant", fd.Type)
	}
	if fd.Params["value"] != "10" {
		t.Errorf("Params[value] = %v, want \"10\"", fd.Params["value"])
	}
}

func TestFixtureDef_ConstantShorthand_String(t *testing.T) {
	fd := parseFixtureDef(t, "name: alice")
	if fd.Name != "name" {
		t.Errorf("Name = %q, want name", fd.Name)
	}
	if fd.Type != schema.FixtureTypeConstant {
		t.Errorf("Type = %q, want constant", fd.Type)
	}
	if fd.Params["value"] != "alice" {
		t.Errorf("Params[value] = %v, want alice", fd.Params["value"])
	}
}

func TestFixtureDef_ListShorthand(t *testing.T) {
	fd := parseFixtureDef(t, "protocols: [simple, extended]")
	if fd.Name != "protocols" {
		t.Errorf("Name = %q, want protocols", fd.Name)
	}
	if fd.Type != schema.FixtureTypeList {
		t.Errorf("Type = %q, want list", fd.Type)
	}
	vals, _ := fd.Params["values"].([]any)
	if len(vals) != 2 {
		t.Fatalf("len(values) = %d, want 2", len(vals))
	}
}

func TestFixtureDef_RangeShorthand(t *testing.T) {
	src := "clients:\nfrom: 1\nto: 5"
	fd := parseFixtureDef(t, src)
	if fd.Name != "clients" {
		t.Errorf("Name = %q, want clients", fd.Name)
	}
	if fd.Type != schema.FixtureTypeRange {
		t.Errorf("Type = %q, want range", fd.Type)
	}
	if fd.Params["min"] == nil {
		t.Error("Params[min] is nil")
	}
	if fd.Params["max"] == nil {
		t.Error("Params[max] is nil")
	}
}

func TestFixtureDef_RangeShorthand_WithStep(t *testing.T) {
	src := "clients:\nfrom: 0\nto: 10\nstep: 2"
	fd := parseFixtureDef(t, src)
	if fd.Type != schema.FixtureTypeRange {
		t.Errorf("Type = %q, want range", fd.Type)
	}
	if fd.Params["step"] == nil {
		t.Error("Params[step] is nil, want value")
	}
}

func TestFixtureDef_LongFormConstant(t *testing.T) {
	src := "scale:\n  type: constant\n  params:\n    value: 42"
	fd := parseFixtureDef(t, src)
	if fd.Name != "scale" {
		t.Errorf("Name = %q, want scale", fd.Name)
	}
	if fd.Type != schema.FixtureTypeConstant {
		t.Errorf("Type = %q, want constant", fd.Type)
	}
}

func TestFixtureDef_LongFormRange(t *testing.T) {
	src := "clients:\n  type: range\n  params:\n    min: 1\n    max: 5\n    step: 1"
	fd := parseFixtureDef(t, src)
	if fd.Name != "clients" {
		t.Errorf("Name = %q, want clients", fd.Name)
	}
	if fd.Type != schema.FixtureTypeRange {
		t.Errorf("Type = %q, want range", fd.Type)
	}
	if fd.Params["min"] == nil || fd.Params["max"] == nil {
		t.Error("long form range must have params.min and params.max")
	}
}

func TestFixtureDef_LongFormList(t *testing.T) {
	src := "protocols:\n  type: list\n  params:\n    values: [a, b, c]"
	fd := parseFixtureDef(t, src)
	if fd.Name != "protocols" {
		t.Errorf("Name = %q, want protocols", fd.Name)
	}
	if fd.Type != schema.FixtureTypeList {
		t.Errorf("Type = %q, want list", fd.Type)
	}
}

func TestFixtureDef_LongFormMissingType(t *testing.T) {
	src := "scale:\n  params:\n    value: 10"
	var fd schema.FixtureDef
	err := yaml.Unmarshal([]byte(src), &fd)
	if err == nil {
		t.Fatal("expected error for long form without type:, got nil")
	}
}

// ---- ExpandFixtures ----

func TestExpandFixtures_NoFixtures(t *testing.T) {
	combos, err := schema.ExpandFixtures(nil, nil)
	if err != nil {
		t.Fatalf("ExpandFixtures: %v", err)
	}
	if len(combos) != 1 {
		t.Fatalf("len(combos) = %d, want 1 (one empty combo)", len(combos))
	}
	if len(combos[0]) != 0 {
		t.Errorf("combos[0] = %v, want empty map", combos[0])
	}
}

func TestExpandFixtures_OneConstant(t *testing.T) {
	fixtures := []schema.FixtureDef{
		{Name: "scale", Type: schema.FixtureTypeConstant, Params: map[string]any{"value": "10"}},
	}
	combos, err := schema.ExpandFixtures(fixtures, nil)
	if err != nil {
		t.Fatalf("ExpandFixtures: %v", err)
	}
	if len(combos) != 1 {
		t.Fatalf("len(combos) = %d, want 1", len(combos))
	}
	if combos[0]["scale"] != "10" {
		t.Errorf("combos[0][scale] = %q, want \"10\"", combos[0]["scale"])
	}
}

func TestExpandFixtures_Range(t *testing.T) {
	fixtures := []schema.FixtureDef{
		{Name: "clients", Type: schema.FixtureTypeRange, Params: map[string]any{"min": 1, "max": 3}},
	}
	combos, err := schema.ExpandFixtures(fixtures, nil)
	if err != nil {
		t.Fatalf("ExpandFixtures: %v", err)
	}
	if len(combos) != 3 {
		t.Fatalf("len(combos) = %d, want 3", len(combos))
	}
	for i, want := range []string{"1", "2", "3"} {
		if combos[i]["clients"] != want {
			t.Errorf("combos[%d][clients] = %q, want %q", i, combos[i]["clients"], want)
		}
	}
}

func TestExpandFixtures_Range_WithStep(t *testing.T) {
	fixtures := []schema.FixtureDef{
		{Name: "n", Type: schema.FixtureTypeRange, Params: map[string]any{"min": 0, "max": 6, "step": 2}},
	}
	combos, err := schema.ExpandFixtures(fixtures, nil)
	if err != nil {
		t.Fatalf("ExpandFixtures: %v", err)
	}
	if len(combos) != 4 {
		t.Fatalf("len(combos) = %d, want 4", len(combos))
	}
	for i, want := range []string{"0", "2", "4", "6"} {
		if combos[i]["n"] != want {
			t.Errorf("combos[%d][n] = %q, want %q", i, combos[i]["n"], want)
		}
	}
}

func TestExpandFixtures_List(t *testing.T) {
	fixtures := []schema.FixtureDef{
		{Name: "proto", Type: schema.FixtureTypeList, Params: map[string]any{"values": []any{"simple", "extended"}}},
	}
	combos, err := schema.ExpandFixtures(fixtures, nil)
	if err != nil {
		t.Fatalf("ExpandFixtures: %v", err)
	}
	if len(combos) != 2 {
		t.Fatalf("len(combos) = %d, want 2", len(combos))
	}
	if combos[0]["proto"] != "simple" {
		t.Errorf("combos[0][proto] = %q, want simple", combos[0]["proto"])
	}
	if combos[1]["proto"] != "extended" {
		t.Errorf("combos[1][proto] = %q, want extended", combos[1]["proto"])
	}
}

func TestExpandFixtures_CartesianProduct(t *testing.T) {
	fixtures := []schema.FixtureDef{
		{Name: "scale", Type: schema.FixtureTypeConstant, Params: map[string]any{"value": "10"}},
		{Name: "clients", Type: schema.FixtureTypeRange, Params: map[string]any{"min": 1, "max": 2}},
		{Name: "proto", Type: schema.FixtureTypeList, Params: map[string]any{"values": []any{"s", "e"}}},
	}
	combos, err := schema.ExpandFixtures(fixtures, nil)
	if err != nil {
		t.Fatalf("ExpandFixtures: %v", err)
	}
	// 1 × 2 × 2 = 4
	if len(combos) != 4 {
		t.Fatalf("len(combos) = %d, want 4", len(combos))
	}
	// All must have scale=10.
	for i, c := range combos {
		if c["scale"] != "10" {
			t.Errorf("combos[%d][scale] = %q, want \"10\"", i, c["scale"])
		}
	}
}

func TestExpandFixtures_InputReference(t *testing.T) {
	inputs := schema.ResolvedInputs{"max_clients": int64(5)}
	fixtures := []schema.FixtureDef{
		{Name: "clients", Type: schema.FixtureTypeRange, Params: map[string]any{"min": 1, "max": "{{ inputs.max_clients }}"}},
	}
	combos, err := schema.ExpandFixtures(fixtures, inputs)
	if err != nil {
		t.Fatalf("ExpandFixtures: %v", err)
	}
	if len(combos) != 5 {
		t.Fatalf("len(combos) = %d, want 5", len(combos))
	}
	if combos[4]["clients"] != "5" {
		t.Errorf("last combo[clients] = %q, want \"5\"", combos[4]["clients"])
	}
}

func TestExpandFixtures_ListFromInputReference(t *testing.T) {
	inputs := schema.ResolvedInputs{"threads": []string{"1", "2", "4", "8", "16", "24"}}
	fixtures := []schema.FixtureDef{
		{Name: "threads", Type: schema.FixtureTypeList, Params: map[string]any{"values": "{{ inputs.threads }}"}},
	}
	combos, err := schema.ExpandFixtures(fixtures, inputs)
	if err != nil {
		t.Fatalf("ExpandFixtures: %v", err)
	}
	if len(combos) != 6 {
		t.Fatalf("len(combos) = %d, want 6", len(combos))
	}
	for i, want := range []string{"1", "2", "4", "8", "16", "24"} {
		if combos[i]["threads"] != want {
			t.Errorf("combos[%d][threads] = %q, want %q", i, combos[i]["threads"], want)
		}
	}
}

func TestExpandFixtures_ListValuesBadType(t *testing.T) {
	fixtures := []schema.FixtureDef{
		{Name: "threads", Type: schema.FixtureTypeList, Params: map[string]any{"values": 42}},
	}
	_, err := schema.ExpandFixtures(fixtures, nil)
	if err == nil {
		t.Fatal("expected error for non-sequence/non-string list values, got nil")
	}
}

func TestExpandFixtures_Range_InvalidMin(t *testing.T) {
	fixtures := []schema.FixtureDef{
		{Name: "n", Type: schema.FixtureTypeRange, Params: map[string]any{"min": "not-a-number", "max": 5}},
	}
	_, err := schema.ExpandFixtures(fixtures, nil)
	if err == nil {
		t.Fatal("expected error for non-integer min, got nil")
	}
}

// ---- Validate: fixtures ----

func fixtureScenario(fixtures []schema.FixtureDef) *schema.Scenario {
	return &schema.Scenario{
		APIVersion: "bench/v1",
		Kind:       "Scenario",
		Metadata:   schema.Metadata{Name: "test"},
		Target:     schema.Target{Provider: "docker-compose"},
		Driver:     schema.Driver{Provider: "local"},
		Collector:  schema.Collector{Provider: "stdout"},
		Suite: schema.Suite{
			Fixtures: fixtures,
			Benchmarks: []schema.SuiteEntry{
				{Name: "bench", Steps: []schema.SuiteStep{{Name: "run", Type: "go-tpc", Command: "run"}}},
			},
		},
	}
}

func TestValidateFixture_UnknownType(t *testing.T) {
	err := fixtureScenario([]schema.FixtureDef{
		{Name: "x", Type: "bogus", Params: map[string]any{"value": "1"}},
	}).Validate()
	if err == nil {
		t.Fatal("expected error for unknown fixture type, got nil")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error %q should mention bogus type", err)
	}
}

func TestValidateFixture_DuplicateName(t *testing.T) {
	err := fixtureScenario([]schema.FixtureDef{
		{Name: "x", Type: schema.FixtureTypeConstant, Params: map[string]any{"value": "1"}},
		{Name: "x", Type: schema.FixtureTypeConstant, Params: map[string]any{"value": "2"}},
	}).Validate()
	if err == nil {
		t.Fatal("expected error for duplicate fixture name, got nil")
	}
}

func TestValidateFixture_RangeMissingMax(t *testing.T) {
	err := fixtureScenario([]schema.FixtureDef{
		{Name: "n", Type: schema.FixtureTypeRange, Params: map[string]any{"min": 1}},
	}).Validate()
	if err == nil {
		t.Fatal("expected error for range missing max, got nil")
	}
}

func TestValidateFixture_ListEmpty(t *testing.T) {
	err := fixtureScenario([]schema.FixtureDef{
		{Name: "p", Type: schema.FixtureTypeList, Params: map[string]any{"values": []any{}}},
	}).Validate()
	if err == nil {
		t.Fatal("expected error for empty list values, got nil")
	}
}

func TestValidateFixture_ListStringReference(t *testing.T) {
	err := fixtureScenario([]schema.FixtureDef{
		{Name: "threads", Type: schema.FixtureTypeList, Params: map[string]any{"values": "{{ inputs.threads }}"}},
	}).Validate()
	if err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestValidateFixture_ListEmptyStringReference(t *testing.T) {
	err := fixtureScenario([]schema.FixtureDef{
		{Name: "threads", Type: schema.FixtureTypeList, Params: map[string]any{"values": "   "}},
	}).Validate()
	if err == nil {
		t.Fatal("expected error for blank string params.values, got nil")
	}
}

func TestValidateFixture_Valid(t *testing.T) {
	err := fixtureScenario([]schema.FixtureDef{
		{Name: "scale", Type: schema.FixtureTypeConstant, Params: map[string]any{"value": "10"}},
		{Name: "clients", Type: schema.FixtureTypeRange, Params: map[string]any{"min": 1, "max": 5}},
		{Name: "proto", Type: schema.FixtureTypeList, Params: map[string]any{"values": []any{"a"}}},
	}).Validate()
	if err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestValidateShellStep_RequiresCommand(t *testing.T) {
	s := &schema.Scenario{
		APIVersion: "bench/v1",
		Kind:       "Scenario",
		Metadata:   schema.Metadata{Name: "test"},
		Target:     schema.Target{Provider: "local"},
		Driver:     schema.Driver{Provider: "local"},
		Collector:  schema.Collector{Provider: "stdout"},
		Suite: schema.Suite{
			Benchmarks: []schema.SuiteEntry{
				{Name: "bench", Steps: []schema.SuiteStep{
					{Name: "cmd", Type: "shell"},
				}},
			},
		},
	}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for shell step without args.command, got nil")
	}
	if !strings.Contains(err.Error(), "args.command") {
		t.Errorf("error %q should mention args.command", err)
	}
}

func TestValidateShellStep_Valid(t *testing.T) {
	s := &schema.Scenario{
		APIVersion: "bench/v1",
		Kind:       "Scenario",
		Metadata:   schema.Metadata{Name: "test"},
		Target:     schema.Target{Provider: "local"},
		Driver:     schema.Driver{Provider: "local"},
		Collector:  schema.Collector{Provider: "stdout"},
		Suite: schema.Suite{
			Benchmarks: []schema.SuiteEntry{
				{Name: "bench", Steps: []schema.SuiteStep{
					{Name: "cmd", Type: "shell", Args: map[string]string{"command": "echo hi"}},
				}},
			},
		},
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

// ---- Load+Validate demo scenario ----

func TestLoadAndValidateEchoFixturesDemo(t *testing.T) {
	s, err := schema.Load("../../scenarios/echo-fixtures-demo.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(s.Suite.Fixtures) != 3 {
		t.Errorf("len(fixtures) = %d, want 3", len(s.Suite.Fixtures))
	}
}
