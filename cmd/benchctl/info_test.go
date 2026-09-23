package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/dbarena/benchctl/internal/schema"
)

func TestFormatResolvedValue(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"string", "abc", "abc"},
		{"int64", int64(10), "10"},
		{"bool", true, "true"},
		{"duration", 30 * time.Minute, "30m0s"},
		{"string list", []string{"1", "2", "4"}, "1,2,4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatResolvedValue(tt.in); got != tt.want {
				t.Errorf("formatResolvedValue(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestFormatFixtureParams(t *testing.T) {
	tests := []struct {
		name string
		fd   schema.FixtureDef
		want string
	}{
		{
			name: "constant",
			fd:   schema.FixtureDef{Name: "scale", Type: schema.FixtureTypeConstant, Params: map[string]any{"value": "10"}},
			want: "value: 10",
		},
		{
			name: "list of values",
			fd:   schema.FixtureDef{Name: "protocol", Type: schema.FixtureTypeList, Params: map[string]any{"values": []any{"simple", "extended"}}},
			want: "values: [simple, extended]",
		},
		{
			name: "range without step",
			fd:   schema.FixtureDef{Name: "clients", Type: schema.FixtureTypeRange, Params: map[string]any{"min": 1, "max": 5}},
			want: "from: 1 to: 5",
		},
		{
			name: "range with step",
			fd:   schema.FixtureDef{Name: "clients", Type: schema.FixtureTypeRange, Params: map[string]any{"min": 1, "max": 10, "step": 2}},
			want: "from: 1 to: 10 step: 2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatFixtureParams(tt.fd); got != tt.want {
				t.Errorf("formatFixtureParams(%+v) = %q, want %q", tt.fd, got, tt.want)
			}
		})
	}
}

func TestFormatFixtureValues(t *testing.T) {
	tests := []struct {
		name     string
		fd       schema.FixtureDef
		resolved schema.ResolvedInputs
		want     string
	}{
		{
			name: "constant",
			fd:   schema.FixtureDef{Name: "scale", Type: schema.FixtureTypeConstant, Params: map[string]any{"value": "10"}},
			want: "10",
		},
		{
			name: "list of literal values",
			fd:   schema.FixtureDef{Name: "protocol", Type: schema.FixtureTypeList, Params: map[string]any{"values": []any{"simple", "extended"}}},
			want: "simple, extended",
		},
		{
			name:     "list resolved from an input reference",
			fd:       schema.FixtureDef{Name: "threads", Type: schema.FixtureTypeList, Params: map[string]any{"values": "{{ inputs.threads }}"}},
			resolved: schema.ResolvedInputs{"threads": []string{"1", "2", "4"}},
			want:     "1, 2, 4",
		},
		{
			name: "range summarized as bounds and count",
			fd:   schema.FixtureDef{Name: "clients", Type: schema.FixtureTypeRange, Params: map[string]any{"min": 1, "max": 5}},
			want: "1..5 (5 values)",
		},
		{
			name:     "range resolved from an input reference",
			fd:       schema.FixtureDef{Name: "clients", Type: schema.FixtureTypeRange, Params: map[string]any{"min": 1, "max": "{{ inputs.max_clients }}"}},
			resolved: schema.ResolvedInputs{"max_clients": int64(3)},
			want:     "1..3 (3 values)",
		},
		{
			name: "unresolvable falls back to raw params",
			fd:   schema.FixtureDef{Name: "clients", Type: schema.FixtureTypeRange, Params: map[string]any{"min": 1, "max": "{{ inputs.max_clients }}"}},
			want: "from: 1 to: {{ inputs.max_clients }} (unresolved)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatFixtureValues(tt.fd, tt.resolved); got != tt.want {
				t.Errorf("formatFixtureValues(%+v, %v) = %q, want %q", tt.fd, tt.resolved, got, tt.want)
			}
		})
	}
}

func TestFormatStep(t *testing.T) {
	tests := []struct {
		name string
		step schema.SuiteStep
		want string
	}{
		{
			name: "with command",
			step: schema.SuiteStep{Name: "prepare-warehouses", Type: "go-tpc", Command: "prepare"},
			want: "prepare-warehouses (go-tpc: prepare)",
		},
		{
			name: "without command",
			step: schema.SuiteStep{Name: "print-combo", Type: "shell"},
			want: "print-combo (shell)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatStep(tt.step); got != tt.want {
				t.Errorf("formatStep(%+v) = %q, want %q", tt.step, got, tt.want)
			}
		})
	}
}

func TestPrintIterations(t *testing.T) {
	tests := []struct {
		name       string
		iterations string
		resolved   schema.ResolvedInputs
		want       string
	}{
		{"unset", "", nil, "Iterations: 1 (default)\n"},
		{"literal", "3", nil, "Iterations: 3\n"},
		{"resolved from an input reference", "{{ inputs.benchmark_iterations }}", schema.ResolvedInputs{"benchmark_iterations": int64(5)}, "Iterations: 5\n"},
		{"unresolvable reference", "{{ inputs.benchmark_iterations }}", nil, "Iterations: {{ inputs.benchmark_iterations }} (unresolved)\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			printIterations(&buf, tt.iterations, tt.resolved)
			if got := buf.String(); got != tt.want {
				t.Errorf("printIterations(%q, %v) = %q, want %q", tt.iterations, tt.resolved, got, tt.want)
			}
		})
	}
}

func TestTreeConnector(t *testing.T) {
	tests := []struct {
		i, n int
		want string
	}{
		{0, 1, "└── "},
		{0, 3, "├── "},
		{1, 3, "├── "},
		{2, 3, "└── "},
	}
	for _, tt := range tests {
		if got := treeConnector(tt.i, tt.n); got != tt.want {
			t.Errorf("treeConnector(%d, %d) = %q, want %q", tt.i, tt.n, got, tt.want)
		}
	}
}

func TestInputOrder(t *testing.T) {
	s, err := schema.Load("../../scenarios/echo-fixtures-demo.yaml")
	if err != nil {
		t.Fatalf("schema.Load: %v", err)
	}
	// echo-fixtures-demo.yaml declares max_clients before protocol.
	if got := inputOrder(s); len(got) != 2 || got[0] != "max_clients" || got[1] != "protocol" {
		t.Errorf("inputOrder = %v, want [max_clients protocol]", got)
	}
}

func TestInputOrder_FallsBackToSortedWhenStale(t *testing.T) {
	s := &schema.Scenario{
		Inputs: map[string]schema.InputDef{
			"zebra": {Type: "string"},
			"alpha": {Type: "string"},
		},
		// InputOrder deliberately left empty/mismatched.
	}
	got := inputOrder(s)
	if len(got) != 2 || got[0] != "alpha" || got[1] != "zebra" {
		t.Errorf("inputOrder = %v, want [alpha zebra] (sorted fallback)", got)
	}
}

func TestRunInfoEchoFixturesDemo(t *testing.T) {
	s, err := schema.Load("../../scenarios/echo-fixtures-demo.yaml")
	if err != nil {
		t.Fatalf("schema.Load: %v", err)
	}
	resolved, resolveErrs := s.ResolveInputsPartial(nil)

	var buf bytes.Buffer
	buf.WriteString("Scenario: " + s.Metadata.Name + "\n\n")
	printInputsTable(&buf, s, resolved, resolveErrs, nil)
	buf.WriteString("\n")
	printFixtures(&buf, s.Suite.Fixtures, resolved)
	buf.WriteString("\n")
	printIterations(&buf, s.Suite.Iterations, resolved)
	buf.WriteString("\n")
	printBenchmarks(&buf, s.Suite.Benchmarks)

	out := buf.String()
	for _, want := range []string{
		"Scenario: echo-fixtures-demo",
		"Inputs:",
		"max_clients",
		"Fixtures:",
		"clients (range)",
		"1..4 (4 values)", // resolved from default max_clients=4, not the literal template
		"Iterations: 1 (default)",
		"Benchmarks:",
		"echo",
		"print-combo (shell)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

func TestRunInfoEchoFixturesDemo_WithSetOverride(t *testing.T) {
	s, err := schema.Load("../../scenarios/echo-fixtures-demo.yaml")
	if err != nil {
		t.Fatalf("schema.Load: %v", err)
	}
	overrides := map[string]string{"max_clients": "2"}
	resolved, resolveErrs := s.ResolveInputsPartial(overrides)

	var buf bytes.Buffer
	printInputsTable(&buf, s, resolved, resolveErrs, overrides)
	buf.WriteString("\n")
	printFixtures(&buf, s.Suite.Fixtures, resolved)

	out := buf.String()
	for _, want := range []string{
		"2 *", // overridden max_clients value, marked
		"* overridden via --set",
		"1..2 (2 values)", // clients fixture now reflects the override
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
}
