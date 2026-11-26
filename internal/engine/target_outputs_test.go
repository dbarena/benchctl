package engine_test

import (
	"context"
	"maps"
	"strings"
	"testing"

	"github.com/dbarena/benchctl/internal/engine"
	"github.com/dbarena/benchctl/internal/runstate"
	"github.com/dbarena/benchctl/internal/schema"
)

// rawOutputs are returned by outputsTarget.Provision. "vendor" stands for a
// key only the provider reads, "_internal" for one with the special "_" prefix.
func rawOutputs() engine.Outputs {
	return engine.Outputs{"host": "10.0.0.1", "vendor": "acme", "_internal": "x"}
}

// mappedOutputs are rawOutputs after the mapping in mappedScenario.
func mappedOutputs() engine.Outputs {
	return engine.Outputs{"db_host": "10.0.0.1", "_internal": "x"}
}

// mappedScenario returns a scenario with a target.outputs block.
func mappedScenario(s *schema.Scenario) *schema.Scenario {
	s.Target.Outputs = map[string]string{"db_host": "{{ provider.host }}"}
	return s
}

// outputsTarget records the outputs that teardown and RunMetadata get.
type outputsTarget struct {
	stubTarget
	teardownOutputs engine.Outputs
	metadataOutputs engine.Outputs
}

func (o *outputsTarget) Provision(ctx context.Context, runID string, cfg map[string]any) (engine.Outputs, error) {
	if _, err := o.stubTarget.Provision(ctx, runID, cfg); err != nil {
		return nil, err
	}
	return rawOutputs(), nil
}

func (o *outputsTarget) Teardown(ctx context.Context, outputs engine.Outputs) error {
	o.teardownOutputs = maps.Clone(outputs)
	return o.stubTarget.Teardown(ctx, outputs)
}

func (o *outputsTarget) RunMetadata(outputs engine.Outputs) map[string]string {
	o.metadataOutputs = maps.Clone(outputs)
	return nil
}

// outputsDriver records the target outputs that driver setup gets.
type outputsDriver struct {
	stubDriver
	setupOutputs engine.Outputs
}

func (o *outputsDriver) Setup(ctx context.Context, cfg map[string]any, targetOutputs engine.Outputs) error {
	o.setupOutputs = maps.Clone(targetOutputs)
	return o.stubDriver.Setup(ctx, cfg, targetOutputs)
}

func newOutputsRunner(calls *[]string, store runstate.Store) (engine.Runner, *outputsTarget, *outputsDriver) {
	target := &outputsTarget{stubTarget: stubTarget{calls: calls}}
	driver := &outputsDriver{stubDriver: stubDriver{calls: calls}}
	return engine.Runner{
		Target:    target,
		Driver:    driver,
		Workloads: map[string]engine.WorkloadAdapter{"go-tpc": &stubWorkload{calls: calls}},
		Collector: &stubCollector{calls: calls},
		Store:     store,
		RunID:     "outputs-run",
	}, target, driver
}

func assertOutputs(t *testing.T, what string, got, want engine.Outputs) {
	t.Helper()
	if !maps.Equal(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// With a target.outputs block, the scenario sees the mapped outputs, but the
// provider and the run state keep the raw ones.
func TestRun_TargetOutputsMapping_ProviderGetsRawOutputs(t *testing.T) {
	var calls []string
	store := newMemStore()
	r, target, driver := newOutputsRunner(&calls, store)

	if err := r.Run(context.Background(), mappedScenario(minimalScenario()), schema.ResolvedInputs{"warehouses": int64(10)}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	assertOutputs(t, "driver setup outputs", driver.setupOutputs, mappedOutputs())
	assertOutputs(t, "teardown outputs", target.teardownOutputs, rawOutputs())
	assertOutputs(t, "RunMetadata outputs", target.metadataOutputs, rawOutputs())
	st, err := store.Load(r.RunID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertOutputs(t, "saved target outputs", st.TargetOutputs, rawOutputs())
}

// A target.outputs block that does not resolve fails the run, and the target
// is still torn down.
func TestRun_TargetOutputsMappingFails_TearsDownTarget(t *testing.T) {
	var calls []string
	r, target, _ := newOutputsRunner(&calls, newMemStore())
	s := minimalScenario()
	s.Target.Outputs = map[string]string{"db_host": "{{ provider.missing }}"}

	err := r.Run(context.Background(), s, schema.ResolvedInputs{"warehouses": int64(10)})
	if err == nil || !strings.Contains(err.Error(), "resolve target outputs") {
		t.Fatalf("Run error = %v, want a resolve target outputs error", err)
	}
	assertOutputs(t, "teardown outputs", target.teardownOutputs, rawOutputs())
}

func TestRunAsync_TargetOutputsMapping_SavesRawOutputs(t *testing.T) {
	var calls []string
	store := newMemStore()
	r, _, _ := newOutputsRunner(&calls, store)
	r.Driver = &stubAsyncDriver{stubDriver: stubDriver{calls: &calls}}

	runID, err := r.RunAsync(context.Background(), mappedScenario(asyncScenario()), schema.ResolvedInputs{"warehouses": int64(10)})
	if err != nil {
		t.Fatalf("RunAsync: %v", err)
	}

	st, err := store.Load(runID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertOutputs(t, "saved target outputs", st.TargetOutputs, rawOutputs())
}

// Resume maps the saved raw outputs again, as Run did.
func TestResume_TargetOutputsMapping_MapsSavedRawOutputs(t *testing.T) {
	var calls []string
	store := newMemStore()
	r, target, driver := newOutputsRunner(&calls, store)

	initial := runstate.NewState(r.RunID, "test", "", map[string]any{"warehouses": int64(10)})
	initial.TargetOutputs = map[string]string(rawOutputs())
	initial.DriverOutputs = map[string]string{}
	if err := store.Create(initial); err != nil {
		t.Fatalf("Create: %v", err)
	}
	loaded, err := store.Load(r.RunID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if err := r.Resume(context.Background(), mappedScenario(minimalScenario()), loaded); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	assertOutputs(t, "driver setup outputs", driver.setupOutputs, mappedOutputs())
	assertOutputs(t, "RunMetadata outputs", target.metadataOutputs, rawOutputs())
}
