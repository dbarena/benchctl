package engine_test

import (
	"testing"
	"time"

	"github.com/dbarena/benchctl/internal/engine"
	"github.com/dbarena/benchctl/internal/schema"
)

func makeTC() *engine.TemplateContext {
	return &engine.TemplateContext{
		Inputs: schema.ResolvedInputs{
			"warehouses": int64(10),
			"threads":    int64(4),
			"duration":   5 * time.Minute,
			"engine":     "acme",
		},
		TargetOutputs: engine.Outputs{
			"host": "db.example.com",
			"port": "5432",
		},
	}
}

func TestResolveInputInt(t *testing.T) {
	got, err := engine.Resolve("{{ inputs.warehouses }}", makeTC())
	if err != nil {
		t.Fatal(err)
	}
	if got != "10" {
		t.Errorf("got %q, want 10", got)
	}
}

func TestResolveInputDuration(t *testing.T) {
	got, err := engine.Resolve("{{ inputs.duration }}", makeTC())
	if err != nil {
		t.Fatal(err)
	}
	if got != "5m0s" {
		t.Errorf("got %q, want 5m0s", got)
	}
}

func TestResolveTargetOutput(t *testing.T) {
	got, err := engine.Resolve("{{ target.outputs.host }}", makeTC())
	if err != nil {
		t.Fatal(err)
	}
	if got != "db.example.com" {
		t.Errorf("got %q, want db.example.com", got)
	}
}

func TestResolveMixed(t *testing.T) {
	got, err := engine.Resolve("host={{ target.outputs.host }} w={{ inputs.warehouses }}", makeTC())
	if err != nil {
		t.Fatal(err)
	}
	if got != "host=db.example.com w=10" {
		t.Errorf("got %q", got)
	}
}

func TestResolveUnknownInput(t *testing.T) {
	_, err := engine.Resolve("{{ inputs.bogus }}", makeTC())
	if err == nil {
		t.Fatal("expected error for unknown input")
	}
}

func TestResolveUnknownNamespace(t *testing.T) {
	_, err := engine.Resolve("{{ provider.foo }}", makeTC())
	if err == nil {
		t.Fatal("expected error for unknown namespace")
	}
}

func TestResolveMap(t *testing.T) {
	m := map[string]string{
		"warehouses": "{{ inputs.warehouses }}",
		"host":       "{{ target.outputs.host }}",
		"literal":    "no-template",
	}
	got, err := engine.ResolveMap(m, makeTC())
	if err != nil {
		t.Fatal(err)
	}
	if got["warehouses"] != "10" {
		t.Errorf("warehouses = %q, want 10", got["warehouses"])
	}
	if got["host"] != "db.example.com" {
		t.Errorf("host = %q, want db.example.com", got["host"])
	}
	if got["literal"] != "no-template" {
		t.Errorf("literal = %q, want no-template", got["literal"])
	}
}

func TestResolveFixture(t *testing.T) {
	tc := makeTC()
	tc.Fixture = schema.ResolvedFixture{"clients": "4", "protocol": "simple"}
	got, err := engine.Resolve("{{ fixture.clients }}", tc)
	if err != nil {
		t.Fatal(err)
	}
	if got != "4" {
		t.Errorf("fixture.clients = %q, want \"4\"", got)
	}
}

func TestResolveFixture_MultipleInString(t *testing.T) {
	tc := makeTC()
	tc.Fixture = schema.ResolvedFixture{"clients": "3", "protocol": "extended"}
	got, err := engine.Resolve("clients={{ fixture.clients }} proto={{ fixture.protocol }}", tc)
	if err != nil {
		t.Fatal(err)
	}
	if got != "clients=3 proto=extended" {
		t.Errorf("got %q, want \"clients=3 proto=extended\"", got)
	}
}

func TestResolveFixture_UnknownName(t *testing.T) {
	tc := makeTC()
	tc.Fixture = schema.ResolvedFixture{"clients": "4"}
	_, err := engine.Resolve("{{ fixture.unknown }}", tc)
	if err == nil {
		t.Fatal("expected error for unknown fixture name, got nil")
	}
}

func TestResolveFixture_NilContext(t *testing.T) {
	tc := makeTC()
	// Fixture is nil (no fixtures declared)
	_, err := engine.Resolve("{{ fixture.clients }}", tc)
	if err == nil {
		t.Fatal("expected error when Fixture is nil, got nil")
	}
}
