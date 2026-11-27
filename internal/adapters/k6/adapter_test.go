package k6

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"testing"

	"github.com/dbarena/benchctl/internal/engine"
	"github.com/dbarena/benchctl/internal/schema"
)

func TestBuildCmd(t *testing.T) {
	prefix := []string{"run", "--summary-export", "summary.json", "--new-machine-readable-summary", "--include-system-env-vars"}
	tests := []struct {
		name     string
		step     schema.SuiteStep
		wantTail []string
	}{
		{
			name:     "no step args",
			step:     schema.SuiteStep{Command: "test.js", Args: map[string]string{}},
			wantTail: []string{"test.js"},
		},
		{
			name: "step args in sorted order before the script",
			step: schema.SuiteStep{
				Command: "test.js",
				Args:    map[string]string{"vus": "10", "duration": "30s", "iterations": "5"},
			},
			wantTail: []string{"--duration", "30s", "--iterations", "5", "--vus", "10", "test.js"},
		},
		{
			name: "bare flag (empty value)",
			step: schema.SuiteStep{
				Command: "test.js",
				Args:    map[string]string{"no-connection-reuse": ""},
			},
			wantTail: []string{"--no-connection-reuse", "test.js"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildCmd(tt.step)
			want := append(append([]string{}, prefix...), tt.wantTail...)
			if !slices.Equal(got, want) {
				t.Errorf("buildCmd() =\n  %v\nwant\n  %v", got, want)
			}
		})
	}
}

func TestBuildEnv(t *testing.T) {
	outputs := engine.Outputs{
		"password":            "s3cret",
		"host":                "db.example.com",
		"api_url_abc":         "https://abc.example.com",
		"projects_manifest":   "/tmp/run/projects.jsonl",
		"BASE_URI":            "https://example.com",
		"value_has_equals_ok": "a=b",
	}
	want := []string{
		"BASE_URI=https://example.com",
		"api_url_abc=https://abc.example.com",
		"host=db.example.com",
		"password=s3cret",
		"projects_manifest=/tmp/run/projects.jsonl",
		"value_has_equals_ok=a=b",
	}
	if got := buildEnv(outputs); !slices.Equal(got, want) {
		t.Errorf("buildEnv() =\n  %v\nwant\n  %v", got, want)
	}
}

// TestRun_PassesOutputsAsEnv checks that Run gives the target outputs to the
// runner as environment variables.
func TestRun_PassesOutputsAsEnv(t *testing.T) {
	t.Chdir(t.TempDir())
	var gotEnv []string
	adapter := newWithRunner(func(_ context.Context, _ io.Writer, env []string, _ string, _ ...string) ([]byte, error) {
		gotEnv = env
		return nil, os.WriteFile("summary.json", []byte(`{"results":{"metrics":[]}}`), 0o644)
	})
	outputs := engine.Outputs{"host": "db.example.com", "password": "s3cret"}
	if _, err := adapter.Run(context.Background(), outputs, schema.SuiteStep{Command: "test.js"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := []string{"host=db.example.com", "password=s3cret"}; !slices.Equal(gotEnv, want) {
		t.Errorf("env = %v, want %v", gotEnv, want)
	}
}

// TestExecRun_EnvOverridesInherited runs a real child process: it gets the
// parent's environment plus env, and a pair in env takes precedence over an
// inherited variable of the same name.
func TestExecRun_EnvOverridesInherited(t *testing.T) {
	t.Setenv("host", "inherited")
	t.Setenv("BENCHCTL_K6_TEST_INHERITED", "kept")
	out, err := execRun(context.Background(), io.Discard, []string{"host=from-outputs"},
		"sh", "-c", `printf '%s %s' "$host" "$BENCHCTL_K6_TEST_INHERITED"`)
	if err != nil {
		t.Fatalf("execRun: %v (output %q)", err, out)
	}
	if got, want := string(out), "from-outputs kept"; got != want {
		t.Errorf("child saw %q, want %q", got, want)
	}
}

func TestVersionInfo_Success(t *testing.T) {
	const sample = `{"commit":"devel","go_arch":"arm64","go_os":"darwin","go_version":"go1.26.5","version":"v2.2.0"}`
	adapter := newWithRunner(func(_ context.Context, _ io.Writer, _ []string, _ string, _ ...string) ([]byte, error) {
		return []byte(sample), nil
	})
	info := adapter.VersionInfo(context.Background())
	if got := info["k6_version"]; got != "v2.2.0" {
		t.Errorf("k6_version = %q, want v2.2.0", got)
	}
}

func TestVersionInfo_Failure(t *testing.T) {
	adapter := newWithRunner(func(_ context.Context, _ io.Writer, _ []string, _ string, _ ...string) ([]byte, error) {
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
