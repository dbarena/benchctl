package schema_test

import (
	"strings"
	"testing"
	"time"

	"github.com/dbarena/benchctl/internal/schema"
)

func TestLoadAndValidateLocal(t *testing.T) {
	s, err := schema.Load("../../scenarios/postgres-tpcc-local.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if s.Metadata.Name != "postgres-tpcc-local" {
		t.Errorf("name = %q, want postgres-tpcc-local", s.Metadata.Name)
	}
	if s.Target.Provider != "docker-compose" {
		t.Errorf("target.provider = %q, want docker-compose", s.Target.Provider)
	}
	if s.Driver.Provider != "local" {
		t.Errorf("driver.provider = %q, want local", s.Driver.Provider)
	}
	if s.Collector.Provider != "{{ inputs.collector }}" {
		t.Errorf("collector.provider = %q, want {{ inputs.collector }}", s.Collector.Provider)
	}
}

func TestLoadAndValidateEC2(t *testing.T) {
	s, err := schema.Load("../../scenarios/postgres-tpcc-ec2.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if s.Target.Provider != "opentofu" {
		t.Errorf("target.provider = %q, want opentofu", s.Target.Provider)
	}
	if s.Driver.Provider != "ec2" {
		t.Errorf("driver.provider = %q, want ec2", s.Driver.Provider)
	}
	if s.Collector.Provider != "{{ inputs.collector }}" {
		t.Errorf("collector.provider = %q, want {{ inputs.collector }}", s.Collector.Provider)
	}
	if len(s.Suite.Benchmarks) == 0 {
		t.Errorf("suite.benchmarks is empty, want at least one entry")
	}
	if len(s.Suite.BetweenBenchmarks) == 0 {
		t.Errorf("suite.between-benchmarks is empty, want at least one entry")
	}
}

func TestResolveInputsDefaults(t *testing.T) {
	s, err := schema.Load("../../scenarios/postgres-tpcc-local.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	inputs, err := s.ResolveInputs(nil)
	if err != nil {
		t.Fatalf("ResolveInputs: %v", err)
	}
	if inputs["warehouses"] != int64(1) {
		t.Errorf("warehouses = %v (%T), want int64(1)", inputs["warehouses"], inputs["warehouses"])
	}
	if inputs["client_threads"] != int64(2) {
		t.Errorf("client_threads = %v, want int64(2)", inputs["client_threads"])
	}
	if inputs["duration"] != 1*time.Minute {
		t.Errorf("duration = %v, want 1m", inputs["duration"])
	}
}

func TestResolveInputsOverride(t *testing.T) {
	s, err := schema.Load("../../scenarios/postgres-tpcc-local.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	inputs, err := s.ResolveInputs(map[string]string{
		"warehouses": "50",
		"duration":   "10m",
	})
	if err != nil {
		t.Fatalf("ResolveInputs: %v", err)
	}
	if inputs["warehouses"] != int64(50) {
		t.Errorf("warehouses = %v, want int64(50)", inputs["warehouses"])
	}
	if inputs["client_threads"] != int64(2) {
		t.Errorf("client_threads = %v, want int64(2) (unchanged default)", inputs["client_threads"])
	}
	if inputs["duration"] != 10*time.Minute {
		t.Errorf("duration = %v, want 10m", inputs["duration"])
	}
}

func TestResolveInputsUnknown(t *testing.T) {
	s, err := schema.Load("../../scenarios/postgres-tpcc-local.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err = s.ResolveInputs(map[string]string{"bogus": "x"})
	if err == nil {
		t.Fatal("expected error for unknown input, got nil")
	}
}

func TestResolveInputsBadType(t *testing.T) {
	s, err := schema.Load("../../scenarios/postgres-tpcc-local.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err = s.ResolveInputs(map[string]string{"warehouses": "not-a-number"})
	if err == nil {
		t.Fatal("expected error for bad int value, got nil")
	}
}

func TestValidateMissingAPIVersion(t *testing.T) {
	s := &schema.Scenario{Kind: "Scenario"}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestValidateMinimalSuiteIsValid(t *testing.T) {
	s := &schema.Scenario{
		APIVersion: "bench/v1",
		Kind:       "Scenario",
		Metadata:   schema.Metadata{Name: "test"},
		Target:     schema.Target{Provider: "docker-compose"},
		Driver:     schema.Driver{Provider: "local"},
		Collector:  schema.Collector{Provider: "stdout"},
		Suite: schema.Suite{
			Benchmarks: []schema.SuiteEntry{
				{Name: "test", Steps: []schema.SuiteStep{{Name: "run", Type: "go-tpc", Command: "run"}}},
			},
		},
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestResolveInputsRequiredMissing(t *testing.T) {
	s := &schema.Scenario{
		APIVersion: "bench/v1",
		Kind:       "Scenario",
		Metadata:   schema.Metadata{Name: "test"},
		Inputs: map[string]schema.InputDef{
			"endpoint": {Type: "string", Required: true},
		},
		Target:    schema.Target{Provider: "docker-compose"},
		Driver:    schema.Driver{Provider: "local"},
		Collector: schema.Collector{Provider: "stdout"},
	}
	_, err := s.ResolveInputs(nil)
	if err == nil {
		t.Fatal("expected error for missing required input, got nil")
	}
}

func TestResolveInputsRequiredProvided(t *testing.T) {
	s := &schema.Scenario{
		APIVersion: "bench/v1",
		Kind:       "Scenario",
		Metadata:   schema.Metadata{Name: "test"},
		Inputs: map[string]schema.InputDef{
			"endpoint": {Type: "string", Required: true},
		},
		Target:    schema.Target{Provider: "docker-compose"},
		Driver:    schema.Driver{Provider: "local"},
		Collector: schema.Collector{Provider: "stdout"},
	}
	inputs, err := s.ResolveInputs(map[string]string{"endpoint": "https://example.com"})
	if err != nil {
		t.Fatalf("ResolveInputs: %v", err)
	}
	if inputs["endpoint"] != "https://example.com" {
		t.Errorf("endpoint = %v, want https://example.com", inputs["endpoint"])
	}
}

func TestResolveInputsRequiredWithDefault(t *testing.T) {
	// A required input that also declares a default: the default satisfies the requirement.
	s := &schema.Scenario{
		APIVersion: "bench/v1",
		Kind:       "Scenario",
		Metadata:   schema.Metadata{Name: "test"},
		Inputs: map[string]schema.InputDef{
			"threads": {Type: "int", Default: 4, Required: true},
		},
		Target:    schema.Target{Provider: "docker-compose"},
		Driver:    schema.Driver{Provider: "local"},
		Collector: schema.Collector{Provider: "stdout"},
	}
	inputs, err := s.ResolveInputs(nil)
	if err != nil {
		t.Fatalf("ResolveInputs: %v", err)
	}
	if inputs["threads"] != int64(4) {
		t.Errorf("threads = %v, want int64(4)", inputs["threads"])
	}
}

func TestResolveInputsList_DefaultFromSequence(t *testing.T) {
	s := &schema.Scenario{
		APIVersion: "bench/v1",
		Kind:       "Scenario",
		Metadata:   schema.Metadata{Name: "test"},
		Inputs: map[string]schema.InputDef{
			"threads": {Type: "list", Default: []any{1, 2, 4, 8, 16, 24}},
		},
		Target:    schema.Target{Provider: "docker-compose"},
		Driver:    schema.Driver{Provider: "local"},
		Collector: schema.Collector{Provider: "stdout"},
	}
	inputs, err := s.ResolveInputs(nil)
	if err != nil {
		t.Fatalf("ResolveInputs: %v", err)
	}
	want := []string{"1", "2", "4", "8", "16", "24"}
	got, ok := inputs["threads"].([]string)
	if !ok {
		t.Fatalf("threads = %v (%T), want []string", inputs["threads"], inputs["threads"])
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("threads = %v, want %v", got, want)
	}
}

func TestResolveInputsList_Override(t *testing.T) {
	s := &schema.Scenario{
		APIVersion: "bench/v1",
		Kind:       "Scenario",
		Metadata:   schema.Metadata{Name: "test"},
		Inputs: map[string]schema.InputDef{
			"threads": {Type: "list", Default: []any{1, 2, 4}},
		},
		Target:    schema.Target{Provider: "docker-compose"},
		Driver:    schema.Driver{Provider: "local"},
		Collector: schema.Collector{Provider: "stdout"},
	}
	inputs, err := s.ResolveInputs(map[string]string{"threads": "1, 2, 4, 8, 16, 24"})
	if err != nil {
		t.Fatalf("ResolveInputs: %v", err)
	}
	got, ok := inputs["threads"].([]string)
	if !ok {
		t.Fatalf("threads = %v (%T), want []string", inputs["threads"], inputs["threads"])
	}
	want := []string{"1", "2", "4", "8", "16", "24"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("threads = %v, want %v (whitespace around commas should be trimmed)", got, want)
	}
}

func TestResolveInputsList_EmptyRejected(t *testing.T) {
	s := &schema.Scenario{
		APIVersion: "bench/v1",
		Kind:       "Scenario",
		Metadata:   schema.Metadata{Name: "test"},
		Inputs: map[string]schema.InputDef{
			"threads": {Type: "list", Default: []any{1}},
		},
		Target:    schema.Target{Provider: "docker-compose"},
		Driver:    schema.Driver{Provider: "local"},
		Collector: schema.Collector{Provider: "stdout"},
	}
	_, err := s.ResolveInputs(map[string]string{"threads": "  "})
	if err == nil {
		t.Fatal("expected error for empty list override, got nil")
	}
}

func TestValidateSQLStepRequiresQuery(t *testing.T) {
	s := &schema.Scenario{
		APIVersion: "bench/v1",
		Kind:       "Scenario",
		Metadata:   schema.Metadata{Name: "test"},
		Target:     schema.Target{Provider: "docker-compose"},
		Driver:     schema.Driver{Provider: "local"},
		Collector:  schema.Collector{Provider: "stdout"},
		Suite: schema.Suite{
			Benchmarks: []schema.SuiteEntry{
				{Name: "test", Steps: []schema.SuiteStep{{Name: "checkpoint", Type: "sql"}}},
			},
		},
	}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for sql step missing args.query, got nil")
	}
	if !strings.Contains(err.Error(), "args.query") {
		t.Errorf("error %q should mention args.query", err)
	}
}

func TestValidateSQLStepWithQuery(t *testing.T) {
	s := &schema.Scenario{
		APIVersion: "bench/v1",
		Kind:       "Scenario",
		Metadata:   schema.Metadata{Name: "test"},
		Target:     schema.Target{Provider: "docker-compose"},
		Driver:     schema.Driver{Provider: "local"},
		Collector:  schema.Collector{Provider: "stdout"},
		Suite: schema.Suite{
			Benchmarks: []schema.SuiteEntry{
				{Name: "test", Steps: []schema.SuiteStep{
					{Name: "checkpoint", Type: "sql", Args: map[string]string{"query": "CHECKPOINT"}},
				}},
			},
		},
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestValidateBadInputType(t *testing.T) {
	s := &schema.Scenario{
		APIVersion: "bench/v1",
		Kind:       "Scenario",
		Metadata:   schema.Metadata{Name: "test"},
		Inputs:     map[string]schema.InputDef{"x": {Type: "float"}},
		Target:     schema.Target{Provider: "docker-compose"},
		Driver:     schema.Driver{Provider: "local"},
		Collector:  schema.Collector{Provider: "stdout"},
	}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for invalid input type, got nil")
	}
}

func TestValidateUsingReferencesUndeclaredService(t *testing.T) {
	s := &schema.Scenario{
		APIVersion: "bench/v1",
		Kind:       "Scenario",
		Metadata:   schema.Metadata{Name: "test"},
		Target: schema.Target{
			Provider: "docker-compose",
			Config: map[string]any{
				"services": []any{
					map[string]any{"name": "acme", "definition": "./services/acme/docker-compose.yml"},
				},
			},
		},
		Driver:    schema.Driver{Provider: "local"},
		Collector: schema.Collector{Provider: "stdout"},
		Suite: schema.Suite{
			Benchmarks: []schema.SuiteEntry{
				{Name: "acme", Using: "acme", Steps: []schema.SuiteStep{{Name: "run", Type: "go-tpc", Command: "run"}}},
				{Name: "postgres", Using: "postgres", Steps: []schema.SuiteStep{{Name: "run", Type: "go-tpc", Command: "run"}}},
			},
		},
	}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for using: reference to undeclared service, got nil")
	}
	if !strings.Contains(err.Error(), "postgres") {
		t.Errorf("error %q should name the missing service", err)
	}
}

func TestValidateUsingReferencesKnownService(t *testing.T) {
	s := &schema.Scenario{
		APIVersion: "bench/v1",
		Kind:       "Scenario",
		Metadata:   schema.Metadata{Name: "test"},
		Target: schema.Target{
			Provider: "docker-compose",
			Config: map[string]any{
				"services": []any{
					map[string]any{"name": "acme", "definition": "./services/acme/docker-compose.yml"},
					map[string]any{"name": "postgres", "definition": "./services/postgres/docker-compose.yml"},
				},
			},
		},
		Driver:    schema.Driver{Provider: "local"},
		Collector: schema.Collector{Provider: "stdout"},
		Suite: schema.Suite{
			Benchmarks: []schema.SuiteEntry{
				{Name: "acme", Using: "acme", Steps: []schema.SuiteStep{{Name: "run", Type: "go-tpc", Command: "run"}}},
				{Name: "postgres", Using: "postgres", Steps: []schema.SuiteStep{{Name: "run", Type: "go-tpc", Command: "run"}}},
			},
		},
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

// ---- between-benchmarks validation ----

func betweenBenchmarksScenario(scope, command, args string) *schema.Scenario {
	return &schema.Scenario{
		APIVersion: "bench/v1",
		Kind:       "Scenario",
		Metadata:   schema.Metadata{Name: "test"},
		Target:     schema.Target{Provider: "docker-compose"},
		Driver:     schema.Driver{Provider: "local"},
		Collector:  schema.Collector{Provider: "stdout"},
		Suite: schema.Suite{
			Benchmarks: []schema.SuiteEntry{
				{Name: "bench", Steps: []schema.SuiteStep{{Name: "run", Type: "go-tpc", Command: "run"}}},
			},
			BetweenBenchmarks: []schema.BetweenBenchmarksStep{
				{Name: "step", Scope: scope, Command: command, Args: args},
			},
		},
	}
}

func TestValidateBetweenBenchmarks_RebootOnDriver(t *testing.T) {
	err := betweenBenchmarksScenario("driver", "reboot", "").Validate()
	if err == nil {
		t.Fatal("expected error for reboot on driver scope, got nil")
	}
	if !strings.Contains(err.Error(), "reboot") {
		t.Errorf("error %q should mention reboot", err)
	}
}

func TestValidateBetweenBenchmarks_ShellRequiresArgs(t *testing.T) {
	err := betweenBenchmarksScenario("target", "shell", "").Validate()
	if err == nil {
		t.Fatal("expected error for shell with empty args, got nil")
	}
}

func TestValidateBetweenBenchmarks_ValidTargetShell(t *testing.T) {
	if err := betweenBenchmarksScenario("target", "shell", "rm -rf /tmp/foo").Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestValidateBetweenBenchmarks_ValidTargetReboot(t *testing.T) {
	if err := betweenBenchmarksScenario("target", "reboot", "").Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestValidateBetweenBenchmarks_UnknownCommand(t *testing.T) {
	err := betweenBenchmarksScenario("target", "frobnicate", "").Validate()
	if err == nil {
		t.Fatal("expected error for unknown between-benchmarks command, got nil")
	}
}

func TestValidateBetweenBenchmarks_ValidTargetTrim(t *testing.T) {
	if err := betweenBenchmarksScenario("target", "trim", "").Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestValidateBetweenBenchmarks_TrimOnDriver(t *testing.T) {
	err := betweenBenchmarksScenario("driver", "trim", "").Validate()
	if err == nil {
		t.Fatal("expected error for trim on driver scope, got nil")
	}
	if !strings.Contains(err.Error(), "trim") {
		t.Errorf("error %q should mention trim", err)
	}
}

func TestValidateBetweenBenchmarks_TrimBadTimeout(t *testing.T) {
	s := betweenBenchmarksScenario("target", "trim", "")
	s.Suite.BetweenBenchmarks[0].Timeout = "notaduration"
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for invalid trim timeout, got nil")
	}
}

func TestValidateBetweenBenchmarks_TrimValidTimeout(t *testing.T) {
	for _, tc := range []string{"30m", "1800s", "-1s", "-1m"} {
		s := betweenBenchmarksScenario("target", "trim", "")
		s.Suite.BetweenBenchmarks[0].Timeout = tc
		if err := s.Validate(); err != nil {
			t.Errorf("timeout %q: Validate() = %v, want nil", tc, err)
		}
	}
}

// ---- metadata step validation ----

func metadataStepScenario(step schema.SuiteStep) *schema.Scenario {
	return &schema.Scenario{
		APIVersion: "bench/v1",
		Kind:       "Scenario",
		Metadata:   schema.Metadata{Name: "test"},
		Target:     schema.Target{Provider: "docker-compose"},
		Driver:     schema.Driver{Provider: "local"},
		Collector:  schema.Collector{Provider: "stdout"},
		Suite: schema.Suite{
			Benchmarks: []schema.SuiteEntry{
				{Name: "bench", Steps: []schema.SuiteStep{
					step,
					{Name: "run", Type: "go-tpc", Command: "run"},
				}},
			},
		},
	}
}

func TestValidateMetadata_SQLIsTheDefaultCommand(t *testing.T) {
	// Scenarios written before `command` existed omit it and must keep working.
	step := schema.SuiteStep{Name: "pg-version", Type: "metadata",
		Args: map[string]string{"name": "pg_version", "query": "SELECT version()"}}
	if err := metadataStepScenario(step).Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestValidateMetadata_SQLRequiresQuery(t *testing.T) {
	step := schema.SuiteStep{Name: "pg-version", Type: "metadata",
		Args: map[string]string{"name": "pg_version"}}
	err := metadataStepScenario(step).Validate()
	if err == nil {
		t.Fatal("expected error for metadata/sql with no query, got nil")
	}
	if !strings.Contains(err.Error(), "args.query") {
		t.Errorf("error %q should mention args.query", err)
	}
}

func TestValidateMetadata_RTTNeedsNoQuery(t *testing.T) {
	step := schema.SuiteStep{Name: "rtt", Type: "metadata", Command: "rtt",
		Args: map[string]string{"name": "network_rtt"}}
	if err := metadataStepScenario(step).Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestValidateMetadata_RequiresName(t *testing.T) {
	step := schema.SuiteStep{Name: "rtt", Type: "metadata", Command: "rtt"}
	err := metadataStepScenario(step).Validate()
	if err == nil {
		t.Fatal("expected error for metadata with no name, got nil")
	}
	if !strings.Contains(err.Error(), "args.name") {
		t.Errorf("error %q should mention args.name", err)
	}
}

func TestValidateMetadata_UnknownCommand(t *testing.T) {
	step := schema.SuiteStep{Name: "rtt", Type: "metadata", Command: "traceroute",
		Args: map[string]string{"name": "network_rtt"}}
	err := metadataStepScenario(step).Validate()
	if err == nil {
		t.Fatal("expected error for unknown metadata command, got nil")
	}
	if !strings.Contains(err.Error(), "traceroute") {
		t.Errorf("error %q should name the bad command", err)
	}
}
