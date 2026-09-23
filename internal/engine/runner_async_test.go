package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/dbarena/benchctl/internal/engine"
	"github.com/dbarena/benchctl/internal/runstate"
	"github.com/dbarena/benchctl/internal/schema"
)

// asyncScenario returns a scenario whose driver requires --async, so RunAsync
// can be exercised without hitting the "driver does not implement async
// bootstrap" guard.
func asyncScenario() *schema.Scenario {
	s := minimalScenario()
	s.Driver.Provider = "ec2"
	return s
}

func newAsyncRunner(calls *[]string, store runstate.Store) engine.Runner {
	return engine.Runner{
		Target:    &stubTarget{calls: calls},
		Driver:    &stubAsyncDriver{stubDriver: stubDriver{calls: calls}},
		Workloads: map[string]engine.WorkloadAdapter{"go-tpc": &stubWorkload{calls: calls}},
		Collector: &stubCollector{calls: calls},
		Store:     store,
	}
}

func TestRunAsync_UsesSuppliedRunID(t *testing.T) {
	var calls []string
	store := newMemStore()
	r := newAsyncRunner(&calls, store)
	r.RunID = "my-custom-run-id"

	runID, err := r.RunAsync(context.Background(), asyncScenario(), schema.ResolvedInputs{"warehouses": int64(10)})
	if err != nil {
		t.Fatalf("RunAsync: %v", err)
	}
	if runID != "my-custom-run-id" {
		t.Errorf("runID = %q, want %q", runID, "my-custom-run-id")
	}
	if _, err := store.Load("my-custom-run-id"); err != nil {
		t.Errorf("expected state stored under the supplied run ID: %v", err)
	}
}

func TestRunAsync_GeneratesRunIDWhenNotSupplied(t *testing.T) {
	var calls []string
	r := newAsyncRunner(&calls, newMemStore())

	runID, err := r.RunAsync(context.Background(), asyncScenario(), schema.ResolvedInputs{"warehouses": int64(10)})
	if err != nil {
		t.Fatalf("RunAsync: %v", err)
	}
	if runID == "" {
		t.Error("expected a generated run ID, got empty string")
	}
}

func TestRunAsync_ProvisionTargetFailsWithPartialOutputs_PersistsState(t *testing.T) {
	var calls []string
	store := newMemStore()
	target := &stubTarget{
		calls:                   &calls,
		provisionErr:            errors.New("target boom"),
		partialProvisionOutputs: engine.Outputs{"project_ref": "abc"},
	}
	r := engine.Runner{
		Target:    target,
		Driver:    &stubAsyncDriver{stubDriver: stubDriver{calls: &calls}},
		Workloads: map[string]engine.WorkloadAdapter{"go-tpc": &stubWorkload{calls: &calls}},
		Collector: &stubCollector{calls: &calls},
		Store:     store,
	}

	runID, err := r.RunAsync(context.Background(), asyncScenario(), schema.ResolvedInputs{"warehouses": int64(10)})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "provision target") {
		t.Errorf("error %q missing 'provision target'", err)
	}

	state, loadErr := store.Load(runID)
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if state.TargetOutputs["project_ref"] != "abc" {
		t.Errorf("state.TargetOutputs[project_ref] = %q, want %q", state.TargetOutputs["project_ref"], "abc")
	}
	if state.Phases[runstate.PhaseProvision] != runstate.StatusFailed {
		t.Errorf("PhaseProvision = %v, want %v", state.Phases[runstate.PhaseProvision], runstate.StatusFailed)
	}
	if state.Error == "" {
		t.Error("expected state.Error to be set")
	}
	if state.CompletedAt == nil {
		t.Error("expected state.CompletedAt to be set so the run becomes terminal")
	}
}

// TestRunAsync_ProvisionTargetFailsWithoutPartialOutputs_StillFinalizesState covers
// the common real-world case (auth failure, quota rejection, etc. before any
// infrastructure exists) that previously left the run stuck showing
// "running (provision)"/"pending" forever: no phase, Error, or CompletedAt was
// ever written because the old code only persisted anything inside the
// `len(rawTargetOutputs) > 0` branch.
func TestRunAsync_ProvisionTargetFailsWithoutPartialOutputs_StillFinalizesState(t *testing.T) {
	var calls []string
	store := newMemStore()
	target := &stubTarget{
		calls:        &calls,
		provisionErr: errors.New("target boom"),
	}
	r := engine.Runner{
		Target:    target,
		Driver:    &stubAsyncDriver{stubDriver: stubDriver{calls: &calls}},
		Workloads: map[string]engine.WorkloadAdapter{"go-tpc": &stubWorkload{calls: &calls}},
		Collector: &stubCollector{calls: &calls},
		Store:     store,
	}

	runID, err := r.RunAsync(context.Background(), asyncScenario(), schema.ResolvedInputs{"warehouses": int64(10)})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	state, loadErr := store.Load(runID)
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if state.Phases[runstate.PhaseProvision] != runstate.StatusFailed {
		t.Errorf("PhaseProvision = %v, want %v", state.Phases[runstate.PhaseProvision], runstate.StatusFailed)
	}
	if state.Error == "" {
		t.Error("expected state.Error to be set")
	}
	if state.CompletedAt == nil {
		t.Error("expected state.CompletedAt to be set so the run becomes terminal")
	}
}

func TestRunAsync_ProvisionDriverFails_FinalizesState(t *testing.T) {
	var calls []string
	store := newMemStore()
	driver := &stubAsyncDriver{stubDriver: stubDriver{calls: &calls, provisionErr: errors.New("driver boom")}}
	r := engine.Runner{
		Target:    &stubTarget{calls: &calls},
		Driver:    driver,
		Workloads: map[string]engine.WorkloadAdapter{"go-tpc": &stubWorkload{calls: &calls}},
		Collector: &stubCollector{calls: &calls},
		Store:     store,
	}

	runID, err := r.RunAsync(context.Background(), asyncScenario(), schema.ResolvedInputs{"warehouses": int64(10)})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "provision driver") {
		t.Errorf("error %q missing 'provision driver'", err)
	}

	state, loadErr := store.Load(runID)
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if state.Phases[runstate.PhaseProvision] != runstate.StatusFailed {
		t.Errorf("PhaseProvision = %v, want %v", state.Phases[runstate.PhaseProvision], runstate.StatusFailed)
	}
	if state.Error == "" {
		t.Error("expected state.Error to be set")
	}
	if state.CompletedAt == nil {
		t.Error("expected state.CompletedAt to be set so the run becomes terminal")
	}
}

// TestRunAsync_BootstrapFails_FinalizesStateEvenThoughNoPhaseWasRunning is the
// direct regression test for the real incident this fix addresses: target and
// driver provisioning both succeed (Phases[PhaseProvision] is already
// StatusCompleted, and RunAsync itself never sets any later phase to
// StatusRunning, which is Resume's job on the remote driver), then bootstrap
// fails. Previously nothing was written to state on this path at all, leaving
// the run non-terminal with real infrastructure and no indication anything
// was wrong ("pending" in `benchctl status`, since the phase scan finds
// PhaseProvision=Completed and nothing after it, with no explicit
// Running/Failed phase to report).
func TestRunAsync_BootstrapFails_FinalizesStateEvenThoughNoPhaseWasRunning(t *testing.T) {
	var calls []string
	store := newMemStore()
	driver := &stubAsyncDriver{stubDriver: stubDriver{calls: &calls}, bootstrapErr: errors.New("bootstrap boom")}
	r := engine.Runner{
		Target:    &stubTarget{calls: &calls},
		Driver:    driver,
		Workloads: map[string]engine.WorkloadAdapter{"go-tpc": &stubWorkload{calls: &calls}},
		Collector: &stubCollector{calls: &calls},
		Store:     store,
	}

	runID, err := r.RunAsync(context.Background(), asyncScenario(), schema.ResolvedInputs{"warehouses": int64(10)})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "bootstrap driver") {
		t.Errorf("error %q missing 'bootstrap driver'", err)
	}

	state, loadErr := store.Load(runID)
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if state.Phases[runstate.PhaseProvision] != runstate.StatusCompleted {
		t.Errorf("PhaseProvision = %v, want %v (provisioning itself succeeded)", state.Phases[runstate.PhaseProvision], runstate.StatusCompleted)
	}
	if state.Error == "" {
		t.Error("expected state.Error to be set even though no phase was left Running")
	}
	if state.CompletedAt == nil {
		t.Error("expected state.CompletedAt to be set so the run becomes terminal instead of showing \"pending\" forever")
	}
}

func TestRunAsync_RejectsInvalidRunID(t *testing.T) {
	var calls []string
	r := newAsyncRunner(&calls, newMemStore())
	r.RunID = "../escape"

	_, err := r.RunAsync(context.Background(), asyncScenario(), schema.ResolvedInputs{"warehouses": int64(10)})
	if err == nil {
		t.Fatal("expected error for invalid --run-id, got nil")
	}
	if !strings.Contains(err.Error(), "run-id") {
		t.Errorf("error %q should mention --run-id", err)
	}
	if len(calls) != 0 {
		t.Errorf("no provider calls expected before the id is validated, got: %v", calls)
	}
}

// ---- Run() also honors RunID, not just RunAsync ----

func TestRun_UsesSuppliedRunID(t *testing.T) {
	var calls []string
	store := newMemStore()
	r := makeRunner(&calls)
	r.Store = store
	r.RunID = "my-sync-run-id"

	if err := r.Run(context.Background(), minimalScenario(), schema.ResolvedInputs{"warehouses": int64(10)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := store.Load("my-sync-run-id"); err != nil {
		t.Errorf("expected state stored under the supplied run ID: %v", err)
	}
}

func TestRun_RejectsInvalidRunID(t *testing.T) {
	var calls []string
	r := makeRunner(&calls)
	r.RunID = "../escape"

	err := r.Run(context.Background(), minimalScenario(), schema.ResolvedInputs{"warehouses": int64(10)})
	if err == nil {
		t.Fatal("expected error for invalid --run-id, got nil")
	}
	if !strings.Contains(err.Error(), "run-id") {
		t.Errorf("error %q should mention --run-id", err)
	}
	if len(calls) != 0 {
		t.Errorf("no provider calls expected before the id is validated, got: %v", calls)
	}
}

// deployServicesFailScenario returns an asyncScenario whose target config has
// an unparsable "services" entry, so deployRemoteServices fails immediately
// on schema.ParseServiceDefs, before any SSH/network call, while still
// reaching the code path that runs after target provisioning succeeds with a
// public_ip.
func deployServicesFailScenario() *schema.Scenario {
	s := asyncScenario()
	s.Target.Config = map[string]any{"services": "not-a-list"}
	return s
}

func targetWithPublicIP(calls *[]string) *stubTarget {
	return &stubTarget{
		calls:            calls,
		provisionOutputs: engine.Outputs{"host": "10.0.0.1", "public_ip": "1.2.3.4", "ssh_user": "ubuntu"},
	}
}

// TestRunAsync_DeployServicesFails_TearsDownTargetAndRecordsTermination is the
// direct regression test for the reported incident: cloud-init/deploy failure
// after target provisioning succeeds triggers self-teardown, and that
// self-teardown succeeding must be reflected in state (TerminatedAt set,
// PhaseTeardown completed) so `benchctl status` doesn't keep reporting the
// (now-destroyed) environment as "running".
func TestRunAsync_DeployServicesFails_TearsDownTargetAndRecordsTermination(t *testing.T) {
	var calls []string
	store := newMemStore()
	target := targetWithPublicIP(&calls)
	r := engine.Runner{
		Target:    target,
		Driver:    &stubAsyncDriver{stubDriver: stubDriver{calls: &calls}},
		Workloads: map[string]engine.WorkloadAdapter{"go-tpc": &stubWorkload{calls: &calls}},
		Collector: &stubCollector{calls: &calls},
		Store:     store,
	}

	runID, err := r.RunAsync(context.Background(), deployServicesFailScenario(), schema.ResolvedInputs{"warehouses": int64(10)})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "deploy services") {
		t.Errorf("error %q missing 'deploy services'", err)
	}
	if !contains(calls, "target.teardown") {
		t.Errorf("expected target.teardown to run, got calls: %v", calls)
	}
	if contains(calls, "driver.provision") {
		t.Errorf("driver should never be provisioned when deploy services fails first, got calls: %v", calls)
	}

	state, loadErr := store.Load(runID)
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if state.TerminatedAt == nil {
		t.Error("expected TerminatedAt to be set once self-teardown succeeds")
	}
	if state.Phases[runstate.PhaseTeardown] != runstate.StatusCompleted {
		t.Errorf("Phases[teardown] = %v, want %v", state.Phases[runstate.PhaseTeardown], runstate.StatusCompleted)
	}
}

func TestRunAsync_DeployServicesFails_TeardownFails_RecordsFailedPhaseNotTerminated(t *testing.T) {
	var calls []string
	store := newMemStore()
	target := targetWithPublicIP(&calls)
	target.teardownErr = errors.New("teardown boom")
	r := engine.Runner{
		Target:    target,
		Driver:    &stubAsyncDriver{stubDriver: stubDriver{calls: &calls}},
		Workloads: map[string]engine.WorkloadAdapter{"go-tpc": &stubWorkload{calls: &calls}},
		Collector: &stubCollector{calls: &calls},
		Store:     store,
	}

	runID, err := r.RunAsync(context.Background(), deployServicesFailScenario(), schema.ResolvedInputs{"warehouses": int64(10)})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "teardown boom") {
		t.Errorf("error %q missing joined teardown failure", err)
	}

	state, loadErr := store.Load(runID)
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if state.TerminatedAt != nil {
		t.Error("expected TerminatedAt to stay nil when self-teardown itself failed")
	}
	if state.Phases[runstate.PhaseTeardown] != runstate.StatusFailed {
		t.Errorf("Phases[teardown] = %v, want %v", state.Phases[runstate.PhaseTeardown], runstate.StatusFailed)
	}
}

func TestRunAsync_DeployServicesFails_NoTeardownFlag_SkipsSelfTeardown(t *testing.T) {
	var calls []string
	store := newMemStore()
	target := targetWithPublicIP(&calls)
	r := engine.Runner{
		Target:     target,
		Driver:     &stubAsyncDriver{stubDriver: stubDriver{calls: &calls}},
		Workloads:  map[string]engine.WorkloadAdapter{"go-tpc": &stubWorkload{calls: &calls}},
		Collector:  &stubCollector{calls: &calls},
		Store:      store,
		NoTeardown: true,
	}

	runID, err := r.RunAsync(context.Background(), deployServicesFailScenario(), schema.ResolvedInputs{"warehouses": int64(10)})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if contains(calls, "target.teardown") {
		t.Errorf("--no-teardown must skip self-teardown, got calls: %v", calls)
	}

	state, loadErr := store.Load(runID)
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if state.TerminatedAt != nil {
		t.Error("expected TerminatedAt to stay nil when teardown was skipped via --no-teardown")
	}
	if state.TargetOutputs == nil {
		t.Error("expected target outputs to remain in state so a later `benchctl teardown` can find them")
	}
}

// TestRunAsync_BootstrapFails_TearsDownDriverThenTarget verifies that a
// failure at the very last early-failure point, after both target and
// driver infrastructure exist, tears down both (driver first, since it may
// depend on the target) purely because both teardown closures were
// registered as their provisioning succeeded; no bootstrap-specific teardown
// code is needed.
func TestRunAsync_BootstrapFails_TearsDownDriverThenTarget(t *testing.T) {
	var calls []string
	store := newMemStore()
	driver := &stubAsyncDriver{stubDriver: stubDriver{calls: &calls}, bootstrapErr: errors.New("bootstrap boom")}
	r := engine.Runner{
		Target:    &stubTarget{calls: &calls},
		Driver:    driver,
		Workloads: map[string]engine.WorkloadAdapter{"go-tpc": &stubWorkload{calls: &calls}},
		Collector: &stubCollector{calls: &calls},
		Store:     store,
	}

	runID, err := r.RunAsync(context.Background(), asyncScenario(), schema.ResolvedInputs{"warehouses": int64(10)})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !contains(calls, "target.teardown") || !contains(calls, "driver.teardown") {
		t.Errorf("expected both target.teardown and driver.teardown, got calls: %v", calls)
	}

	state, loadErr := store.Load(runID)
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if state.TerminatedAt == nil {
		t.Error("expected TerminatedAt to be set once self-teardown succeeds")
	}
	if state.Phases[runstate.PhaseTeardown] != runstate.StatusCompleted {
		t.Errorf("Phases[teardown] = %v, want %v", state.Phases[runstate.PhaseTeardown], runstate.StatusCompleted)
	}
}

func TestRunAsync_RejectsCollidingRunID(t *testing.T) {
	var calls []string
	store := newMemStore()
	if err := store.Create(runstate.NewState("dup-id", "test", "scenarios/x.yaml", nil)); err != nil {
		t.Fatalf("seed Create: %v", err)
	}

	r := newAsyncRunner(&calls, store)
	r.RunID = "dup-id"

	_, err := r.RunAsync(context.Background(), asyncScenario(), schema.ResolvedInputs{"warehouses": int64(10)})
	if err == nil {
		t.Fatal("expected error for colliding --run-id, got nil")
	}
	if contains(calls, "target.provision") {
		t.Error("target.provision should not run when the run ID already exists in the store")
	}
}

// ---- handoff to the driver instance ----

// TestRunAsync_SeedsAndDelegates_WithUnsharedStore covers the handoff a store
// only this machine can read needs: the driver instance gets a copy of the
// record (otherwise `benchctl resume` there has nothing to load), and the
// local copy is marked as no longer authoritative so later reads go there.
func TestRunAsync_SeedsAndDelegates_WithUnsharedStore(t *testing.T) {
	var calls []string
	store := newUnsharedMemStore()
	driver := &stubAsyncDriver{stubDriver: stubDriver{calls: &calls}}
	r := newAsyncRunner(&calls, store)
	r.Driver = driver
	r.RunID = "run-a"

	if _, err := r.RunAsync(context.Background(), asyncScenario(), schema.ResolvedInputs{"warehouses": int64(10)}); err != nil {
		t.Fatalf("RunAsync: %v", err)
	}

	if len(driver.request.StateSeed) == 0 {
		t.Fatal("expected a state seed for a store the driver instance cannot read")
	}
	var seeded runstate.State
	if err := json.Unmarshal(driver.request.StateSeed, &seeded); err != nil {
		t.Fatalf("parse seed: %v", err)
	}
	if seeded.RunID != "run-a" {
		t.Errorf("seeded run ID = %q, want run-a", seeded.RunID)
	}
	// The seed has to carry the outputs written during provisioning, or the
	// driver cannot reach the target it is meant to benchmark.
	if len(seeded.TargetOutputs) == 0 {
		t.Error("seed should carry target outputs")
	}
	if got := store.delegated; len(got) != 1 || got[0] != "run-a" {
		t.Errorf("delegated = %v, want [run-a]", got)
	}
}

// TestRunAsync_SharedStoreNeedsNoHandoff: with a shared store the driver reads
// the same record the orchestrator wrote, so there is nothing to copy and
// nothing to hand over.
func TestRunAsync_SharedStoreNeedsNoHandoff(t *testing.T) {
	var calls []string
	store := newMemStore()
	driver := &stubAsyncDriver{stubDriver: stubDriver{calls: &calls}}
	r := newAsyncRunner(&calls, store)
	r.Driver = driver

	if _, err := r.RunAsync(context.Background(), asyncScenario(), schema.ResolvedInputs{"warehouses": int64(10)}); err != nil {
		t.Fatalf("RunAsync: %v", err)
	}
	if len(driver.request.StateSeed) != 0 {
		t.Error("a shared store should not seed the driver instance")
	}
	if len(store.delegated) != 0 {
		t.Errorf("a shared store should not delegate ownership, got %v", store.delegated)
	}
}

// TestRunAsync_BootstrapFails_KeepsOwnership: the deferred finalizer records
// the failure in this store, so the local record must stay authoritative;
// there is no driver process to read it back from.
func TestRunAsync_BootstrapFails_KeepsOwnership(t *testing.T) {
	var calls []string
	store := newUnsharedMemStore()
	driver := &stubAsyncDriver{stubDriver: stubDriver{calls: &calls}, bootstrapErr: errors.New("bootstrap boom")}
	r := newAsyncRunner(&calls, store)
	r.Driver = driver

	runID, err := r.RunAsync(context.Background(), asyncScenario(), schema.ResolvedInputs{"warehouses": int64(10)})
	if err == nil {
		t.Fatal("expected the bootstrap error")
	}
	if len(store.delegated) != 0 {
		t.Errorf("ownership should not be handed over when bootstrap fails, got %v", store.delegated)
	}
	state, loadErr := store.Load(runID)
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if !state.IsTerminal() {
		t.Error("expected the failed run to be terminal")
	}
}
