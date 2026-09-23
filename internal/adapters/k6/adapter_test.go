package k6

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/dbarena/benchctl/internal/engine"
	"github.com/dbarena/benchctl/internal/schema"
)

func TestBuildCmd(t *testing.T) {
	tests := []struct {
		name     string
		outputs  engine.Outputs
		step     schema.SuiteStep
		wantArgs []string
	}{
		{
			name:    "no outputs, no extra args",
			outputs: engine.Outputs{},
			step: schema.SuiteStep{
				Command: "test.js",
				Args:    map[string]string{},
			},
			wantArgs: []string{"run", "--summary-export", "summary.json", "--new-machine-readable-summary", "test.js"},
		},
		{
			name:    "target outputs forwarded as env vars",
			outputs: engine.Outputs{"BASE_URI": "https://example.com", "SERVICE_TOKEN": "tok123"},
			step:    schema.SuiteStep{Command: "test.js"},
			wantArgs: []string{
				"run", "--summary-export", "summary.json", "--new-machine-readable-summary",
				"--env", "BASE_URI=https://example.com",
				"--env", "SERVICE_TOKEN=tok123",
				"test.js",
			},
		},
		{
			name:    "step args appended before script",
			outputs: engine.Outputs{},
			step: schema.SuiteStep{
				Command: "test.js",
				Args:    map[string]string{"vus": "10", "duration": "30s"},
			},
			// step.Args map iteration is non-deterministic; only check prefix/suffix
			wantArgs: nil, // tested structurally below
		},
		{
			name:    "bare flag (empty value)",
			outputs: engine.Outputs{},
			step: schema.SuiteStep{
				Command: "test.js",
				Args:    map[string]string{"no-connection-reuse": ""},
			},
			wantArgs: []string{"run", "--summary-export", "summary.json", "--new-machine-readable-summary", "--no-connection-reuse", "test.js"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildCmd(tt.outputs, tt.step)

			if got[0] != "run" {
				t.Errorf("first arg = %q, want %q", got[0], "run")
			}
			if got[len(got)-1] != "test.js" {
				t.Errorf("last arg = %q, want %q", got[len(got)-1], "test.js")
			}

			hasSummaryExport, hasNewSummary := false, false
			for i := 0; i < len(got); i++ {
				if i < len(got)-1 && got[i] == "--summary-export" && got[i+1] == "summary.json" {
					hasSummaryExport = true
				}
				if got[i] == "--new-machine-readable-summary" {
					hasNewSummary = true
				}
			}
			if !hasSummaryExport {
				t.Errorf("missing --summary-export summary.json in args: %v", got)
			}
			if !hasNewSummary {
				t.Errorf("missing --new-machine-readable-summary in args: %v", got)
			}

			if tt.wantArgs != nil {
				if len(got) != len(tt.wantArgs) {
					t.Fatalf("len = %d, want %d\ngot:  %v\nwant: %v", len(got), len(tt.wantArgs), got, tt.wantArgs)
				}
				for i := range tt.wantArgs {
					if got[i] != tt.wantArgs[i] {
						t.Errorf("args[%d] = %q, want %q", i, got[i], tt.wantArgs[i])
					}
				}
			}
		})
	}
}

func TestVersionInfo_Success(t *testing.T) {
	const sample = `{"commit":"devel","go_arch":"arm64","go_os":"darwin","go_version":"go1.26.5","version":"v2.2.0"}`
	adapter := newWithRunner(func(_ context.Context, _ io.Writer, _ string, _ ...string) ([]byte, error) {
		return []byte(sample), nil
	})
	info := adapter.VersionInfo(context.Background())
	if got := info["k6_version"]; got != "v2.2.0" {
		t.Errorf("k6_version = %q, want v2.2.0", got)
	}
}

func TestVersionInfo_Failure(t *testing.T) {
	adapter := newWithRunner(func(_ context.Context, _ io.Writer, _ string, _ ...string) ([]byte, error) {
		return []byte("error output"), errors.New("exit 1")
	})
	if info := adapter.VersionInfo(context.Background()); info != nil {
		t.Errorf("VersionInfo = %v, want nil", info)
	}
}

func TestMapK6MetricName(t *testing.T) {
	tests := []struct {
		k6Name string
		want   string
	}{
		{"http_reqs", "k6_http_reqs"},
		{"http_req_duration", "k6_http_req_duration"},
		{"vus", "k6_vus"},
		{"iterations", "k6_iterations"},
	}

	for _, tt := range tests {
		t.Run(tt.k6Name, func(t *testing.T) {
			got := mapK6MetricName(tt.k6Name)
			if got != tt.want {
				t.Errorf("mapK6MetricName(%q) = %q, want %q", tt.k6Name, got, tt.want)
			}
		})
	}
}

func TestExtractMetrics(t *testing.T) {
	// Mirrors the --new-machine-readable-summary format.
	summary := map[string]any{
		"metadata": map[string]any{
			"k6_version":   "2.0.0",
			"generated_at": "2026-05-15T12:00:00Z",
		},
		"config": map[string]any{
			"duration":  float64(60),
			"execution": "local",
		},
		"results": map[string]any{
			"metrics": []any{
				map[string]any{
					"name":   "http_reqs",
					"type":   "counter",
					"values": map[string]any{"count": float64(474)},
				},
				map[string]any{
					"name": "http_req_duration",
					"type": "trend",
					"values": map[string]any{
						"avg": float64(195.5),
						"med": float64(163.3),
						"max": nil, // null values must be skipped
					},
				},
				map[string]any{
					"name":   "vus",
					"type":   "gauge",
					"values": map[string]any{"value": float64(10)},
				},
			},
		},
	}

	sm := extractMetrics(summary)

	if sm.InfoLabels["k6_version"] != "2.0.0" {
		t.Errorf("k6_version = %q, want %q", sm.InfoLabels["k6_version"], "2.0.0")
	}
	if sm.InfoLabels["execution"] != "local" {
		t.Errorf("execution = %q, want %q", sm.InfoLabels["execution"], "local")
	}

	families := make(map[string]bool)
	for _, p := range sm.Points {
		families[p.Family] = true
	}
	for _, want := range []string{"k6_http_reqs", "k6_http_req_duration", "k6_vus"} {
		if !families[want] {
			t.Errorf("missing family %q; got %v", want, families)
		}
	}

	for _, p := range sm.Points {
		if p.Family == "k6_http_reqs" && p.Value != 474 {
			t.Errorf("http_reqs count = %v, want 474", p.Value)
		}
		if p.Family == "k6_http_req_duration" {
			if got := p.Labels["expected_response"]; got != "null" {
				t.Errorf("http_req_duration expected_response = %q, want %q", got, "null")
			}
		}
	}
}

func TestExtractMetrics_HttpReqDurationExpectedResponse(t *testing.T) {
	tests := []struct {
		metricName           string
		wantExpectedResponse string
		wantFamily           string
	}{
		{"http_req_duration", "null", "k6_http_req_duration"},
		{"http_req_duration{expected_response:true}", "true", "k6_http_req_duration"},
		{"http_req_duration{expected_response:false}", "false", "k6_http_req_duration"},
	}

	for _, tt := range tests {
		t.Run(tt.metricName, func(t *testing.T) {
			summary := map[string]any{
				"results": map[string]any{
					"metrics": []any{
						map[string]any{
							"name":   tt.metricName,
							"type":   "trend",
							"values": map[string]any{"avg": float64(100)},
						},
					},
				},
			}
			sm := extractMetrics(summary)
			if len(sm.Points) == 0 {
				t.Fatal("expected at least one metric point")
			}
			p := sm.Points[0]
			if p.Family != tt.wantFamily {
				t.Errorf("family = %q, want %q", p.Family, tt.wantFamily)
			}
			if got := p.Labels["expected_response"]; got != tt.wantExpectedResponse {
				t.Errorf("expected_response = %q, want %q", got, tt.wantExpectedResponse)
			}
		})
	}
}

func TestExtractMetrics_MissingResultsKey(t *testing.T) {
	sm := extractMetrics(map[string]any{"metadata": map[string]any{}})
	if len(sm.Points) != 0 {
		t.Errorf("expected no points for summary without results key, got %d", len(sm.Points))
	}
}
