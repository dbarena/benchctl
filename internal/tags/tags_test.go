package tags

import (
	"testing"

	"github.com/dbarena/benchctl/internal/config"
)

func TestInject(t *testing.T) {
	const id = "my-scenario-20260617-120000-abc123"

	tests := []struct {
		name     string
		vars     map[string]any
		cfgTags  map[string]string
		wantVars map[string]any // top-level vars keys that must survive
		wantTags map[string]any // tags keys that must survive
	}{
		{
			name: "nil vars",
			vars: nil,
		},
		{
			name: "vars without tags",
			vars: map[string]any{"instance_type": "c8gd.xlarge"},
			wantVars: map[string]any{
				"instance_type": "c8gd.xlarge",
			},
		},
		{
			name: "vars with existing tags",
			vars: map[string]any{
				"tags": map[string]any{
					"org":  "perf-eng",
					"team": "engops",
				},
			},
			wantTags: map[string]any{
				"org":  "perf-eng",
				"team": "engops",
			},
		},
		{
			name: "cfg tags flow through verbatim",
			vars: map[string]any{},
			cfgTags: map[string]string{
				"foo": "bar",
			},
			wantTags: map[string]any{
				"foo": "bar",
			},
		},
		{
			name: "cfg tags cannot override the forced keys",
			vars: map[string]any{},
			cfgTags: map[string]string{
				"created-by": "someone-else",
				"run-id":     "not-the-real-run-id",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Inject(tt.vars, id, &config.Config{Tags: tt.cfgTags})

			tags, ok := got["tags"].(map[string]any)
			if !ok {
				t.Fatalf("vars[\"tags\"] is not map[string]any: %T", got["tags"])
			}
			if tags["run-id"] != id {
				t.Errorf("tags[\"run-id\"] = %q, want %q", tags["run-id"], id)
			}
			if tags["created-by"] != "benchctl" {
				t.Errorf("tags[\"created-by\"] = %q, want %q", tags["created-by"], "benchctl")
			}
			for k, want := range tt.wantVars {
				if got[k] != want {
					t.Errorf("vars[%q] = %v, want %v", k, got[k], want)
				}
			}
			for k, want := range tt.wantTags {
				if tags[k] != want {
					t.Errorf("tags[%q] = %v, want %v", k, tags[k], want)
				}
			}
		})
	}
}
