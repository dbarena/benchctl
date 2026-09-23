package engine

import (
	"reflect"
	"testing"
	"time"

	"github.com/dbarena/benchctl/internal/schema"
)

// A "threads" list input with a single override value (--set threads=1) round-trips
// through the run-state store as JSON, which decodes a []string back as
// []any (json.Unmarshal has no way to recover the original element type).
// Without this case, rehydrateInputs left the []any as-is,
// ResolveTemplate's v.([]string) type assertion silently failed,
// and the value fell through to fmt's default formatting, so go-tpc's --threads
// flag received the literal string "[1]" instead of "1" and failed with
// "invalid argument \"[1]\" for \"-T, --threads\" flag: strconv.ParseInt:
// parsing \"[1]\": invalid syntax".
func TestRehydrateInputs_List(t *testing.T) {
	defs := map[string]schema.InputDef{
		"threads": {Type: "list"},
	}

	tests := []struct {
		name string
		raw  []any
		want []string
	}{
		{"single value", []any{"1"}, []string{"1"}},
		{"multiple values", []any{"1", "2", "4", "8", "12"}, []string{"1", "2", "4", "8", "12"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := rehydrateInputs(map[string]any{"threads": tt.raw}, defs)
			got, ok := result["threads"].([]string)
			if !ok {
				t.Fatalf("threads = %#v (%T), want []string", result["threads"], result["threads"])
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("threads = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestRehydrateInputs_ListPreservesNonListTypes guards against regressing the
// existing int/duration/passthrough rehydration behavior while adding the
// list case above.
func TestRehydrateInputs_ListPreservesNonListTypes(t *testing.T) {
	defs := map[string]schema.InputDef{
		"warehouses": {Type: "int"},
		"duration":   {Type: "duration"},
		"name":       {Type: "string"},
	}
	raw := map[string]any{
		"warehouses": float64(160), // JSON numbers decode as float64
		"duration":   float64(30 * time.Second),
		"name":       "medium",
	}

	result := rehydrateInputs(raw, defs)

	if got, ok := result["warehouses"].(int64); !ok || got != 160 {
		t.Errorf("warehouses = %#v, want int64(160)", result["warehouses"])
	}
	if got, ok := result["duration"].(time.Duration); !ok || got != 30*time.Second {
		t.Errorf("duration = %#v, want 30s", result["duration"])
	}
	if result["name"] != "medium" {
		t.Errorf("name = %#v, want \"medium\"", result["name"])
	}
}
