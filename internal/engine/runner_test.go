package engine_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/dbarena/benchctl/internal/engine"
	"github.com/dbarena/benchctl/internal/runstate"
	"github.com/dbarena/benchctl/internal/schema"
)

// ---- stub implementations ----

type stubTarget struct {
	provisionErr            error
	partialProvisionOutputs engine.Outputs // returned alongside provisionErr, if set
	provisionOutputs        engine.Outputs // overrides the default success outputs, if set
	teardownErr             error
	calls                   *[]string
}

func (s *stubTarget) Provision(_ context.Context, _ string, _ map[string]any) (engine.Outputs, error) {
	*s.calls = append(*s.calls, "target.provision")
	if s.provisionErr != nil {
		return s.partialProvisionOutputs, s.provisionErr
	}
	if s.provisionOutputs != nil {
		return s.provisionOutputs, nil
	}
	return engine.Outputs{"host": "localhost", "port": "5432"}, nil
}
func (s *stubTarget) Teardown(_ context.Context, _ engine.Outputs) error {
	*s.calls = append(*s.calls, "target.teardown")
	return s.teardownErr
}

type stubDriver struct {
	provisionErr            error
	partialProvisionOutputs engine.Outputs // returned alongside provisionErr, if set
	setupErr                error
	collectErr              error
	teardownErr             error
	calls                   *[]string
}

func (s *stubDriver) Provision(_ context.Context, _ string, _ map[string]any) (engine.Outputs, error) {
	*s.calls = append(*s.calls, "driver.provision")
	if s.provisionErr != nil {
		return s.partialProvisionOutputs, s.provisionErr
	}
	return engine.Outputs{}, nil
}
func (s *stubDriver) Setup(_ context.Context, _ map[string]any, _ engine.Outputs) error {
	*s.calls = append(*s.calls, "driver.setup")
	return s.setupErr
}
func (s *stubDriver) Collect(_ context.Context, _ map[string]any, _ engine.Outputs, metrics engine.Metrics) (engine.Metrics, error) {
	*s.calls = append(*s.calls, "driver.collect")
	return metrics, s.collectErr
}
func (s *stubDriver) Teardown(_ context.Context, _ engine.Outputs) error {
	*s.calls = append(*s.calls, "driver.teardown")
	return s.teardownErr
}

// stubAsyncDriver wraps stubDriver and also implements AsyncBootstrapper,
// making it a remote-only driver that requires --async mode. It records the
// request it was handed so tests can assert on the state seed.
type stubAsyncDriver struct {
	stubDriver
	bootstrapErr error
	request      engine.BootstrapRequest
}

func (s *stubAsyncDriver) Bootstrap(_ context.Context, req engine.BootstrapRequest) error {
	s.request = req
	return s.bootstrapErr
}

type stubWorkload struct {
	prepareErr error
	runErr     error
	calls      *[]string
}

// Run logs the step it was handed and, like the go-tpc adapter, returns
// metrics only for a measured step; a "prepare" step loads data.
func (s *stubWorkload) Run(_ context.Context, _ engine.Outputs, step schema.SuiteStep) (engine.Metrics, error) {
	if step.Command == "prepare" {
		*s.calls = append(*s.calls, "workload.prepare")
		return engine.Metrics{}, s.prepareErr
	}
	*s.calls = append(*s.calls, "workload.run")
	return engine.Metrics{"tpm": 1234}, s.runErr
}

type stubCollector struct {
	collectErr error
	calls      *[]string
}

func (s *stubCollector) Collect(_ context.Context, _ map[string]any, _ engine.Metrics) error {
	*s.calls = append(*s.calls, "collector.collect")
	return s.collectErr
}

// ---- helpers ----

func minimalScenario() *schema.Scenario {
	return &schema.Scenario{
		APIVersion: "bench/v1",
		Kind:       "Scenario",
		Metadata:   schema.Metadata{Name: "test"},
		Inputs:     map[string]schema.InputDef{"warehouses": {Type: "int", Default: 10}},
		Target: schema.Target{
			Provider: "docker-compose",
		},
		Suite: schema.Suite{
			Benchmarks: []schema.SuiteEntry{
				{
					Name:  "test",
					Using: "test",
					Steps: []schema.SuiteStep{
						{Name: "prepare", Type: "go-tpc", Command: "prepare", Args: map[string]string{"warehouses": "{{ inputs.warehouses }}"}},
						{Name: "benchmark", Type: "go-tpc", Command: "run", Args: map[string]string{"warehouses": "{{ inputs.warehouses }}"}},
					},
				},
			},
		},
		Driver:    schema.Driver{Provider: "local"},
		Collector: schema.Collector{Provider: "stdout"},
	}
}

func makeRunner(calls *[]string, opts ...func(*stubTarget, *stubDriver, *stubWorkload, *stubCollector)) engine.Runner {
	t := &stubTarget{calls: calls}
	d := &stubDriver{calls: calls}
	w := &stubWorkload{calls: calls}
	c := &stubCollector{calls: calls}
	for _, o := range opts {
		o(t, d, w, c)
	}
	return engine.Runner{Target: t, Driver: d, Workloads: map[string]engine.WorkloadAdapter{"go-tpc": w}, Collector: c}
}

// ---- tests ----

func TestHappyPath(t *testing.T) {
	var calls []string
	r := makeRunner(&calls)

	if err := r.Run(context.Background(), minimalScenario(), schema.ResolvedInputs{"warehouses": int64(10)}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := []string{
		"target.provision",
		"driver.provision",
		"driver.setup",
		"workload.prepare",
		"workload.run",
		"driver.collect",
		"collector.collect",
		"driver.teardown",
		"target.teardown",
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i, c := range want {
		if calls[i] != c {
			t.Errorf("calls[%d] = %q, want %q", i, calls[i], c)
		}
	}
}

func TestProvisionTargetFails_NoDriverProvisioned(t *testing.T) {
	var calls []string
	r := makeRunner(&calls, func(tgt *stubTarget, _ *stubDriver, _ *stubWorkload, _ *stubCollector) {
		tgt.provisionErr = errors.New("target boom")
	})

	err := r.Run(context.Background(), minimalScenario(), schema.ResolvedInputs{"warehouses": int64(10)})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "provision target") {
		t.Errorf("error %q missing 'provision target'", err)
	}
	// driver must never have been provisioned
	for _, c := range calls {
		if c == "driver.provision" {
			t.Error("driver.provision should not have been called")
		}
	}
	// target teardown must not run (provision failed before teardown was registered)
	for _, c := range calls {
		if c == "target.teardown" {
			t.Error("target.teardown should not run when provision failed")
		}
	}
}

func TestProvisionTargetFailsWithPartialOutputs_TeardownRuns(t *testing.T) {
	var calls []string
	r := makeRunner(&calls, func(tgt *stubTarget, _ *stubDriver, _ *stubWorkload, _ *stubCollector) {
		tgt.provisionErr = errors.New("target boom")
		tgt.partialProvisionOutputs = engine.Outputs{"project_ref": "abc"}
	})

	err := r.Run(context.Background(), minimalScenario(), schema.ResolvedInputs{"warehouses": int64(10)})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "provision target") {
		t.Errorf("error %q missing 'provision target'", err)
	}
	if !contains(calls, "target.teardown") {
		t.Errorf("target.teardown not called for partial provision outputs; calls: %v", calls)
	}
	if contains(calls, "driver.provision") {
		t.Error("driver.provision should not have been called")
	}
}

func TestProvisionDriverFails_TargetTeardown(t *testing.T) {
	var calls []string
	r := makeRunner(&calls, func(_ *stubTarget, drv *stubDriver, _ *stubWorkload, _ *stubCollector) {
		drv.provisionErr = errors.New("driver boom")
	})

	err := r.Run(context.Background(), minimalScenario(), schema.ResolvedInputs{"warehouses": int64(10)})
	if err == nil {
		t.Fatal("expected error")
	}
	// target teardown must have run
	if !contains(calls, "target.teardown") {
		t.Errorf("target.teardown not called after driver provision failure; calls: %v", calls)
	}
	// driver teardown must not run (provision failed)
	if contains(calls, "driver.teardown") {
		t.Error("driver.teardown should not run when driver provision failed")
	}
}

// TestProvisionDriverFails_TeardownPhaseReflectsTeardownOutcomeNotRunOutcome
// guards against a regression where a run's overall failure (driver
// provisioning) got conflated with its teardown outcome: the target's
// teardown here succeeds even though the run as a whole fails, and
// Phases[teardown] must record that teardown succeeded; otherwise
// `benchctl teardown`'s "already completed" guard never fires and a later
// retry re-runs teardown against infrastructure that's already gone.
func TestProvisionDriverFails_TeardownPhaseReflectsTeardownOutcomeNotRunOutcome(t *testing.T) {
	var calls []string
	store := newMemStore()
	r := makeRunner(&calls, func(_ *stubTarget, drv *stubDriver, _ *stubWorkload, _ *stubCollector) {
		drv.provisionErr = errors.New("driver boom")
	})
	r.Store = store
	r.RunID = "test-run"

	err := r.Run(context.Background(), minimalScenario(), schema.ResolvedInputs{"warehouses": int64(10)})
	if err == nil {
		t.Fatal("expected error")
	}

	st, loadErr := store.Load("test-run")
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if st.Phases[runstate.PhaseTeardown] != runstate.StatusCompleted {
		t.Errorf("Phases[teardown] = %q, want %q (target teardown succeeded despite the run's provisioning failure)",
			st.Phases[runstate.PhaseTeardown], runstate.StatusCompleted)
	}
	if st.TerminatedAt == nil {
		t.Error("TerminatedAt not set even though teardown succeeded")
	}
	if st.Error == "" {
		t.Error("Error should still record the run's overall failure")
	}
}

func TestPrepareFails_BothTeardownsRun(t *testing.T) {
	var calls []string
	r := makeRunner(&calls, func(_ *stubTarget, _ *stubDriver, w *stubWorkload, _ *stubCollector) {
		w.prepareErr = errors.New("prepare boom")
	})

	err := r.Run(context.Background(), minimalScenario(), schema.ResolvedInputs{"warehouses": int64(10)})
	if err == nil {
		t.Fatal("expected error")
	}
	if !contains(calls, "driver.teardown") {
		t.Error("driver.teardown not called")
	}
	if !contains(calls, "target.teardown") {
		t.Error("target.teardown not called")
	}
}

func TestExecuteFails_BothTeardownsRun(t *testing.T) {
	var calls []string
	r := makeRunner(&calls, func(_ *stubTarget, _ *stubDriver, w *stubWorkload, _ *stubCollector) {
		w.runErr = errors.New("run boom")
	})

	err := r.Run(context.Background(), minimalScenario(), schema.ResolvedInputs{"warehouses": int64(10)})
	if err == nil {
		t.Fatal("expected error")
	}
	if !contains(calls, "driver.teardown") || !contains(calls, "target.teardown") {
		t.Errorf("teardowns not called; calls: %v", calls)
	}
}

func TestCollectFails_BothTeardownsRun(t *testing.T) {
	var calls []string
	r := makeRunner(&calls, func(_ *stubTarget, _ *stubDriver, _ *stubWorkload, c *stubCollector) {
		c.collectErr = errors.New("collect boom")
	})

	err := r.Run(context.Background(), minimalScenario(), schema.ResolvedInputs{"warehouses": int64(10)})
	if err == nil {
		t.Fatal("expected error")
	}
	if !contains(calls, "driver.teardown") || !contains(calls, "target.teardown") {
		t.Errorf("teardowns not called; calls: %v", calls)
	}
}

func TestDriverSetupFails_BothTeardownsRun(t *testing.T) {
	var calls []string
	r := makeRunner(&calls, func(_ *stubTarget, drv *stubDriver, _ *stubWorkload, _ *stubCollector) {
		drv.setupErr = errors.New("setup boom")
	})

	err := r.Run(context.Background(), minimalScenario(), schema.ResolvedInputs{"warehouses": int64(10)})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "driver setup") {
		t.Errorf("error %q missing 'driver setup'", err)
	}
	if !contains(calls, "driver.teardown") {
		t.Error("driver.teardown not called")
	}
	if !contains(calls, "target.teardown") {
		t.Error("target.teardown not called")
	}
	if contains(calls, "workload.prepare") {
		t.Error("workload.prepare should not run after driver setup failure")
	}
}

func TestDriverCollectFails_BothTeardownsRun(t *testing.T) {
	var calls []string
	r := makeRunner(&calls, func(_ *stubTarget, drv *stubDriver, _ *stubWorkload, _ *stubCollector) {
		drv.collectErr = errors.New("driver collect boom")
	})

	err := r.Run(context.Background(), minimalScenario(), schema.ResolvedInputs{"warehouses": int64(10)})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "driver collect") {
		t.Errorf("error %q missing 'driver collect'", err)
	}
	if !contains(calls, "driver.teardown") || !contains(calls, "target.teardown") {
		t.Errorf("teardowns not called; calls: %v", calls)
	}
}

func TestTeardownReversOrder(t *testing.T) {
	var calls []string
	r := makeRunner(&calls)

	if err := r.Run(context.Background(), minimalScenario(), schema.ResolvedInputs{"warehouses": int64(10)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// driver was provisioned after target, so its teardown must come first
	di := indexOf(calls, "driver.teardown")
	ti := indexOf(calls, "target.teardown")
	if di == -1 || ti == -1 {
		t.Fatalf("missing teardown calls: %v", calls)
	}
	if di >= ti {
		t.Errorf("driver.teardown (%d) should precede target.teardown (%d)", di, ti)
	}
}

func TestTeardownErrorSurfaced(t *testing.T) {
	var calls []string
	r := makeRunner(&calls, func(tgt *stubTarget, _ *stubDriver, _ *stubWorkload, _ *stubCollector) {
		tgt.teardownErr = errors.New("teardown boom")
	})

	err := r.Run(context.Background(), minimalScenario(), schema.ResolvedInputs{"warehouses": int64(10)})
	if err == nil {
		t.Fatal("expected teardown error to be surfaced")
	}
	if !strings.Contains(err.Error(), "teardown") {
		t.Errorf("error %q missing 'teardown'", err)
	}
}

// TestDriverTeardownFails_TargetTeardownSkipped guards against a regression
// where a failed driver teardown didn't stop the target's teardown from
// running anyway. Co-located scenarios run the driver inside the target's
// own VPC/subnet (e.g. deployments/rds/postgres handing its subnet_id to
// deployments/ec2/loaddriver_gotpc), so a driver instance left behind by a
// failed teardown is still attached when the target tries to tear down its
// network. Proceeding anyway trades one failure for a worse one (a
// half-destroyed VPC via an EC2 DependencyViolation deleting the Internet
// Gateway/subnet).
func TestDriverTeardownFails_TargetTeardownSkipped(t *testing.T) {
	var calls []string
	r := makeRunner(&calls, func(_ *stubTarget, drv *stubDriver, _ *stubWorkload, _ *stubCollector) {
		drv.teardownErr = errors.New("driver teardown boom")
	})

	err := r.Run(context.Background(), minimalScenario(), schema.ResolvedInputs{"warehouses": int64(10)})
	if err == nil {
		t.Fatal("expected teardown error to be surfaced")
	}
	if !contains(calls, "driver.teardown") {
		t.Error("driver.teardown should have been attempted")
	}
	if contains(calls, "target.teardown") {
		t.Error("target.teardown must not run after driver teardown failed")
	}
}

func TestTemplateResolutionInArgs(t *testing.T) {
	var calls []string

	// Use minimalScenario to verify inputs resolve into args.
	s := minimalScenario()
	inputs, _ := s.ResolveInputs(map[string]string{"warehouses": "42"})

	var preparedStep schema.SuiteStep
	tw := &trackingWorkload{calls: &calls, onPrepare: func(step schema.SuiteStep) {
		preparedStep = step
	}}
	r := engine.Runner{
		Target:    &stubTarget{calls: &calls},
		Driver:    &stubDriver{calls: &calls},
		Workloads: map[string]engine.WorkloadAdapter{"go-tpc": tw},
		Collector: &stubCollector{calls: &calls},
	}

	if err := r.Run(context.Background(), s, inputs); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if preparedStep.Args["warehouses"] != "42" {
		t.Errorf("prepare args warehouses = %q, want 42", preparedStep.Args["warehouses"])
	}
}

// trackingWorkload captures the resolved prepare step for inspection.
type trackingWorkload struct {
	calls     *[]string
	onPrepare func(schema.SuiteStep)
}

func (tw *trackingWorkload) Run(_ context.Context, _ engine.Outputs, step schema.SuiteStep) (engine.Metrics, error) {
	if step.Command == "prepare" {
		*tw.calls = append(*tw.calls, "workload.prepare")
		if tw.onPrepare != nil {
			tw.onPrepare(step)
		}
		return engine.Metrics{}, nil
	}
	*tw.calls = append(*tw.calls, "workload.run")
	return engine.Metrics{}, nil
}

func TestLingerOnFailure_SkipsTeardownOnError(t *testing.T) {
	var calls []string
	r := makeRunner(&calls, func(_ *stubTarget, _ *stubDriver, w *stubWorkload, _ *stubCollector) {
		w.prepareErr = errors.New("prepare boom")
	})
	r.LingerOnFailure = true

	err := r.Run(context.Background(), minimalScenario(), schema.ResolvedInputs{"warehouses": int64(10)})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if contains(calls, "driver.teardown") {
		t.Error("driver.teardown should not run when LingerOnFailure=true and run failed")
	}
	if contains(calls, "target.teardown") {
		t.Error("target.teardown should not run when LingerOnFailure=true and run failed")
	}
}

func TestLingerOnFailure_TeardownRunsOnSuccess(t *testing.T) {
	var calls []string
	r := makeRunner(&calls)
	r.LingerOnFailure = true

	if err := r.Run(context.Background(), minimalScenario(), schema.ResolvedInputs{"warehouses": int64(10)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !contains(calls, "driver.teardown") {
		t.Error("driver.teardown should run on success even with LingerOnFailure=true")
	}
	if !contains(calls, "target.teardown") {
		t.Error("target.teardown should run on success even with LingerOnFailure=true")
	}
}

func TestLingerOnFailure_LogsRunID(t *testing.T) {
	var calls []string
	var out strings.Builder
	r := makeRunner(&calls, func(_ *stubTarget, _ *stubDriver, w *stubWorkload, _ *stubCollector) {
		w.prepareErr = errors.New("prepare boom")
	})
	r.LingerOnFailure = true
	r.Out = &out

	r.Run(context.Background(), minimalScenario(), schema.ResolvedInputs{"warehouses": int64(10)}) //nolint:errcheck
	if !strings.Contains(out.String(), "benchctl teardown") {
		t.Errorf("expected linger message with 'benchctl teardown', got: %q", out.String())
	}
}

func TestAsyncOnlyDriverRejectedInSyncMode(t *testing.T) {
	var calls []string
	r := engine.Runner{
		Target:       &stubTarget{calls: &calls},
		Driver:       &stubAsyncDriver{stubDriver: stubDriver{calls: &calls}},
		Workloads:    map[string]engine.WorkloadAdapter{"go-tpc": &stubWorkload{calls: &calls}},
		Collector:    &stubCollector{calls: &calls},
		ScenarioPath: "scenarios/postgres-tpcc-ec2.yaml",
	}
	s := minimalScenario()
	s.Driver.Provider = "ec2"

	err := r.Run(context.Background(), s, schema.ResolvedInputs{})
	if err == nil {
		t.Fatal("expected error for async-only driver in sync mode, got nil")
	}
	if !strings.Contains(err.Error(), "--async") {
		t.Errorf("error %q should mention --async", err)
	}
	if len(calls) != 0 {
		t.Errorf("no provider calls expected before the guard, got: %v", calls)
	}
}

// ---- opentofu work-dir archiving tests ----

// stubTofuTarget is a TargetProvider that returns a real temp dir as
// _tofu_work_dir to exercise the pack path.
type stubTofuTarget struct {
	calls   *[]string
	workDir string
}

func (s *stubTofuTarget) Provision(_ context.Context, _ string, _ map[string]any) (engine.Outputs, error) {
	*s.calls = append(*s.calls, "target.provision")
	return engine.Outputs{
		"host":                      "localhost",
		"port":                      "5432",
		engine.OutputKeyTofuWorkDir: s.workDir,
	}, nil
}
func (s *stubTofuTarget) Teardown(_ context.Context, _ engine.Outputs) error {
	*s.calls = append(*s.calls, "target.teardown")
	return nil
}

// memStore is an in-memory Store that also satisfies runstate.SharedStore.
type memStore struct {
	states map[string]*runstate.State
	shared bool
	// delegated records the runs handed off to another machine, in order.
	delegated []string
}

// newMemStore returns a store whose records other machines can read, like
// SupabaseStore.
func newMemStore() *memStore { return &memStore{states: map[string]*runstate.State{}, shared: true} }

// newUnsharedMemStore returns a store only this machine can read, like
// LocalStore.
func newUnsharedMemStore() *memStore { return &memStore{states: map[string]*runstate.State{}} }

func (m *memStore) IsShared() bool { return m.shared }
func (m *memStore) Delegate(runID string) error {
	m.delegated = append(m.delegated, runID)
	return nil
}
func (m *memStore) Claim(string) error { return nil }
func (m *memStore) Create(st *runstate.State) error {
	if _, exists := m.states[st.RunID]; exists {
		return fmt.Errorf("run %q already exists", st.RunID)
	}
	cp := *st
	m.states[st.RunID] = &cp
	return nil
}
func (m *memStore) Update(id string, fn func(*runstate.State)) error {
	st, ok := m.states[id]
	if !ok {
		return errors.New("not found")
	}
	fn(st)
	return nil
}
func (m *memStore) Load(id string) (*runstate.State, error) {
	st, ok := m.states[id]
	if !ok {
		return nil, errors.New("not found")
	}
	cp := *st
	return &cp, nil
}
func (m *memStore) List() ([]*runstate.State, error) {
	var out []*runstate.State
	for _, st := range m.states {
		cp := *st
		out = append(out, &cp)
	}
	return out, nil
}

// TestTofuTarget_RunsWithoutAnyStore covers the no-persistence path: an
// OpenTofu target used with Store == nil must still run. benchctl used to
// refuse this outright, on the theory that the work dir was unrecoverable,
// but it lives at a stable path under $HOME, so a later `benchctl teardown`
// on this machine finds it.
func TestTofuTarget_RunsWithoutAnyStore(t *testing.T) {
	var calls []string
	r := engine.Runner{
		Target:    &stubTofuTarget{calls: &calls, workDir: t.TempDir()},
		Driver:    &stubDriver{calls: &calls},
		Workloads: map[string]engine.WorkloadAdapter{"go-tpc": &stubWorkload{calls: &calls}},
		Collector: &stubCollector{calls: &calls},
		Store:     nil,
	}

	if err := r.Run(context.Background(), minimalScenario(), schema.ResolvedInputs{"warehouses": int64(10)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !contains(calls, "target.provision") {
		t.Error("target.provision should have been called")
	}
}

// TestTofuStateNotPacked_WithUnsharedStore pins the other half of
// packTofuState's guard: with a store no other machine can read, the work dir
// is already on this machine, so archiving a copy of it into the record would
// be pure duplication.
func TestTofuStateNotPacked_WithUnsharedStore(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(workDir+"/terraform.tfstate", []byte(`{"version":4}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var calls []string
	store := newUnsharedMemStore()
	r := engine.Runner{
		Target:    &stubTofuTarget{calls: &calls, workDir: workDir},
		Driver:    &stubDriver{calls: &calls},
		Workloads: map[string]engine.WorkloadAdapter{"go-tpc": &stubWorkload{calls: &calls}},
		Collector: &stubCollector{calls: &calls},
		Store:     store,
	}

	if err := r.Run(context.Background(), minimalScenario(), schema.ResolvedInputs{"warehouses": int64(10)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	states, _ := store.List()
	if len(states) != 1 {
		t.Fatalf("expected 1 state, got %d", len(states))
	}
	if states[0].TofuState != "" {
		t.Error("TofuState should stay empty when the store is not shared")
	}
}

func TestTofuStatePacked_AfterProvision(t *testing.T) {
	workDir := t.TempDir()
	// Write a state file so the pack produces a non-trivial blob.
	if err := os.WriteFile(workDir+"/terraform.tfstate", []byte(`{"version":4}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var calls []string
	store := newMemStore()
	r := engine.Runner{
		Target:    &stubTofuTarget{calls: &calls, workDir: workDir},
		Driver:    &stubDriver{calls: &calls},
		Workloads: map[string]engine.WorkloadAdapter{"go-tpc": &stubWorkload{calls: &calls}},
		Collector: &stubCollector{calls: &calls},
		Store:     store,
	}

	if err := r.Run(context.Background(), minimalScenario(), schema.ResolvedInputs{"warehouses": int64(10)}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Find the run that was created and verify TofuState was populated.
	states, _ := store.List()
	if len(states) != 1 {
		t.Fatalf("expected 1 state, got %d", len(states))
	}
	if states[0].TofuState == "" {
		t.Error("TofuState should be non-empty after provision with opentofu target")
	}
}

func TestTofuStateNotPacked_WithoutTofuWorkDir(t *testing.T) {
	var calls []string
	store := newMemStore()
	// stubTarget returns no _tofu_work_dir: it is not an opentofu provider.
	r := engine.Runner{
		Target:    &stubTarget{calls: &calls},
		Driver:    &stubDriver{calls: &calls},
		Workloads: map[string]engine.WorkloadAdapter{"go-tpc": &stubWorkload{calls: &calls}},
		Collector: &stubCollector{calls: &calls},
		Store:     store,
	}

	if err := r.Run(context.Background(), minimalScenario(), schema.ResolvedInputs{"warehouses": int64(10)}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	states, _ := store.List()
	if len(states) != 1 {
		t.Fatalf("expected 1 state, got %d", len(states))
	}
	if states[0].TofuState != "" {
		t.Error("TofuState should be empty for non-opentofu providers")
	}
}

// ---- per-benchmark collection tests ----

// capturingCollector records a snapshot of the labels map from each Collect call.
type capturingCollector struct {
	calls  *[]string
	labels []map[string]any
}

func (c *capturingCollector) Collect(_ context.Context, cfg map[string]any, _ engine.Metrics) error {
	*c.calls = append(*c.calls, "collector.collect")
	labels, _ := cfg["labels"].(map[string]any)
	snapshot := make(map[string]any, len(labels))
	for k, v := range labels {
		snapshot[k] = v
	}
	c.labels = append(c.labels, snapshot)
	return nil
}

func makeCapturingRunner(calls *[]string) (engine.Runner, *capturingCollector) {
	return makeCapturingRunnerWithTarget(calls, &stubTarget{calls: calls})
}

func makeCapturingRunnerWithTarget(calls *[]string, target engine.TargetProvider) (engine.Runner, *capturingCollector) {
	cap := &capturingCollector{calls: calls}
	r := engine.Runner{
		Target:    target,
		Driver:    &stubDriver{calls: calls},
		Workloads: map[string]engine.WorkloadAdapter{"go-tpc": &stubWorkload{calls: calls}},
		Collector: cap,
	}
	return r, cap
}

// stubTargetWithMetadata wraps stubTarget and also implements
// engine.TargetMetadata, mimicking a provider like Supabase that contributes
// extra collector labels derived from its own Outputs.
type stubTargetWithMetadata struct {
	stubTarget
	meta map[string]string
}

func (s *stubTargetWithMetadata) RunMetadata(_ engine.Outputs) map[string]string {
	return s.meta
}

func TestPerBenchmarkCollection_BenchmarkLabel(t *testing.T) {
	var calls []string
	r, cap := makeCapturingRunner(&calls)
	s := minimalScenario()
	s.Suite.Benchmarks[0].Name = "my-benchmark"
	if err := r.Run(context.Background(), s, schema.ResolvedInputs{"warehouses": int64(10)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(cap.labels) != 1 {
		t.Fatalf("expected 1 collect call, got %d", len(cap.labels))
	}
	if got := cap.labels[0]["benchmark"]; got != "my-benchmark" {
		t.Errorf("labels[benchmark] = %v, want my-benchmark", got)
	}
}

func TestPerBenchmarkCollection_ServiceLabel(t *testing.T) {
	var calls []string
	r, cap := makeCapturingRunner(&calls)
	s := minimalScenario()
	s.Suite.Benchmarks[0].Using = "my-service"
	if err := r.Run(context.Background(), s, schema.ResolvedInputs{"warehouses": int64(10)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := cap.labels[0]["service"]; got != "my-service" {
		t.Errorf("labels[service] = %v, want my-service", got)
	}
}

func TestPerBenchmarkCollection_ServiceLabelFallsBackToBenchmarkName(t *testing.T) {
	var calls []string
	r, cap := makeCapturingRunner(&calls)
	s := minimalScenario()
	s.Suite.Benchmarks[0].Using = ""
	if err := r.Run(context.Background(), s, schema.ResolvedInputs{"warehouses": int64(10)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := cap.labels[0]["service"]; got != s.Suite.Benchmarks[0].Name {
		t.Errorf("labels[service] = %v, want benchmark name %q", got, s.Suite.Benchmarks[0].Name)
	}
}

func TestPerBenchmarkCollection_UserLabelsPreserved(t *testing.T) {
	var calls []string
	r, cap := makeCapturingRunner(&calls)
	s := minimalScenario()
	s.Collector.Config = map[string]any{
		"labels": map[string]any{"region": "us-east-1"},
	}
	if err := r.Run(context.Background(), s, schema.ResolvedInputs{"warehouses": int64(10)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := cap.labels[0]["region"]; got != "us-east-1" {
		t.Errorf("user label region = %v, want us-east-1", got)
	}
	if cap.labels[0]["benchmark"] == nil {
		t.Error("auto label benchmark should also be present")
	}
}

func TestPerBenchmarkCollection_TargetMetadataLabel(t *testing.T) {
	var calls []string
	target := &stubTargetWithMetadata{stubTarget: stubTarget{calls: &calls}, meta: map[string]string{"project_id": "abcxyz"}}
	r, cap := makeCapturingRunnerWithTarget(&calls, target)
	s := minimalScenario()
	if err := r.Run(context.Background(), s, schema.ResolvedInputs{"warehouses": int64(10)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := cap.labels[0]["project_id"]; got != "abcxyz" {
		t.Errorf("labels[project_id] = %v, want abcxyz", got)
	}
}

func TestPerBenchmarkCollection_NoTargetMetadataInterfaceLeavesLabelsUnaffected(t *testing.T) {
	var calls []string
	r, cap := makeCapturingRunner(&calls)
	s := minimalScenario()
	if err := r.Run(context.Background(), s, schema.ResolvedInputs{"warehouses": int64(10)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := cap.labels[0]["project_id"]; ok {
		t.Errorf("labels[project_id] = %v, want absent", cap.labels[0]["project_id"])
	}
}

func TestPerBenchmarkCollection_CalledPerBenchmark(t *testing.T) {
	var calls []string
	r, cap := makeCapturingRunner(&calls)
	s := minimalScenario()
	s.Suite = schema.Suite{
		Benchmarks: []schema.SuiteEntry{
			{Name: "bench-a", Steps: []schema.SuiteStep{{Name: "run", Type: "go-tpc", Command: "run"}}},
			{Name: "bench-b", Steps: []schema.SuiteStep{{Name: "run", Type: "go-tpc", Command: "run"}}},
		},
	}
	if err := r.Run(context.Background(), s, schema.ResolvedInputs{"warehouses": int64(10)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(cap.labels) != 2 {
		t.Fatalf("collector called %d times, want 2", len(cap.labels))
	}
	if cap.labels[0]["benchmark"] != "bench-a" {
		t.Errorf("first collect benchmark = %v, want bench-a", cap.labels[0]["benchmark"])
	}
	if cap.labels[1]["benchmark"] != "bench-b" {
		t.Errorf("second collect benchmark = %v, want bench-b", cap.labels[1]["benchmark"])
	}
}

// ---- between-benchmarks execution tests ----

type iterRecord struct {
	benchmark string
	iteration any
}

type iterCapCollector struct {
	calls   *[]string
	records *[]iterRecord
}

func (c *iterCapCollector) Collect(_ context.Context, cfg map[string]any, _ engine.Metrics) error {
	*c.calls = append(*c.calls, "collector.collect")
	labels, _ := cfg["labels"].(map[string]any)
	*c.records = append(*c.records, iterRecord{
		benchmark: fmt.Sprint(labels["benchmark"]),
		iteration: cfg["iteration"],
	})
	return nil
}

func TestIterations_RepeatsAndLabels(t *testing.T) {
	var calls []string
	var out strings.Builder
	var records []iterRecord

	r := engine.Runner{
		Target:    &stubTarget{calls: &calls},
		Driver:    &stubDriver{calls: &calls},
		Workloads: map[string]engine.WorkloadAdapter{"go-tpc": &stubWorkload{calls: &calls}},
		Collector: &iterCapCollector{calls: &calls, records: &records},
		Out:       &out,
	}

	s := minimalScenario()
	s.Suite = schema.Suite{
		Iterations: "3",
		Benchmarks: []schema.SuiteEntry{
			{Name: "bench-a", Steps: []schema.SuiteStep{{Name: "run", Type: "go-tpc", Command: "run"}}},
			{Name: "bench-b", Steps: []schema.SuiteStep{{Name: "run", Type: "go-tpc", Command: "run"}}},
		},
		BetweenBenchmarks: []schema.BetweenBenchmarksStep{
			{Name: "cleanup", Scope: "driver", Command: "shell", Args: "true"},
		},
	}

	if err := r.Run(context.Background(), s, schema.ResolvedInputs{"warehouses": int64(10)}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 2 benchmarks × 3 iterations = 6 collect calls.
	if len(records) != 6 {
		t.Fatalf("collector called %d time(s), want 6", len(records))
	}

	// Verify order: bench-a iter1, bench-b iter1, bench-a iter2, …
	want := []iterRecord{
		{"bench-a", 1}, {"bench-b", 1},
		{"bench-a", 2}, {"bench-b", 2},
		{"bench-a", 3}, {"bench-b", 3},
	}
	for idx, w := range want {
		if records[idx].benchmark != w.benchmark {
			t.Errorf("records[%d].benchmark = %q, want %q", idx, records[idx].benchmark, w.benchmark)
		}
		if records[idx].iteration != w.iteration {
			t.Errorf("records[%d].iteration = %v, want %v", idx, records[idx].iteration, w.iteration)
		}
	}

	// between-benchmarks: 5 gaps in a flat sequence of 6 (not after the last).
	count := strings.Count(out.String(), "Running between-benchmarks step")
	if count != 5 {
		t.Errorf("between-benchmarks ran %d time(s), want 5; output:\n%s", count, out.String())
	}
}

func TestBetweenBenchmarks_RunsBetweenNotAfterLast(t *testing.T) {
	var calls []string
	var out strings.Builder
	r := makeRunner(&calls)
	r.Out = &out

	s := minimalScenario()
	s.Suite = schema.Suite{
		Benchmarks: []schema.SuiteEntry{
			{Name: "bench-a", Steps: []schema.SuiteStep{{Name: "run", Type: "go-tpc", Command: "run"}}},
			{Name: "bench-b", Steps: []schema.SuiteStep{{Name: "run", Type: "go-tpc", Command: "run"}}},
		},
		BetweenBenchmarks: []schema.BetweenBenchmarksStep{
			{Name: "cleanup", Scope: "driver", Command: "shell", Args: "true"},
		},
	}
	if err := r.Run(context.Background(), s, schema.ResolvedInputs{"warehouses": int64(10)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// one step between bench-a and bench-b, none after bench-b
	count := strings.Count(out.String(), "Running between-benchmarks step")
	if count != 1 {
		t.Errorf("between-benchmarks step ran %d time(s), want 1; output:\n%s", count, out.String())
	}
}

// multiStepWorkload is a WorkloadAdapter fake that returns a distinct
// tpcc_tpm point value and raw_samples_csv row per step name, so a test can
// verify that two go-tpc "run" steps in one benchmark entry (e.g. a
// "warm-up" step followed by "benchmark") stay distinguishable after
// mergeMetrics folds them together.
type multiStepWorkload struct {
	calls *[]string
}

func (w *multiStepWorkload) Run(_ context.Context, _ engine.Outputs, step schema.SuiteStep) (engine.Metrics, error) {
	*w.calls = append(*w.calls, "workload.run:"+step.Name)
	value := 100.0
	if step.Name == "benchmark" {
		value = 200.0
	}
	return engine.Metrics{
		engine.StructuredKey: engine.StructuredMetrics{
			Points: []engine.MetricPoint{{Family: "tpcc_tpm", Labels: map[string]string{"transaction": "NEW_ORDER", "status": "ok"}, Value: value}},
		},
		"raw_samples_csv": fmt.Sprintf("t_seconds,transaction,tpm\n1.0,NEW_ORDER,%v\n", value),
	}, nil
}

// metricsCapturingCollector records the final Metrics passed to Collect, for
// tests that need to inspect merged structured points or flat keys rather
// than just the collector's config labels.
type metricsCapturingCollector struct {
	calls   *[]string
	metrics engine.Metrics
}

func (c *metricsCapturingCollector) Collect(_ context.Context, _ map[string]any, m engine.Metrics) error {
	*c.calls = append(*c.calls, "collector.collect")
	c.metrics = m
	return nil
}

func TestWarmupAndBenchmarkSteps_StayDistinguishableAfterMerge(t *testing.T) {
	var calls []string
	cap := &metricsCapturingCollector{calls: &calls}
	r := engine.Runner{
		Target:    &stubTarget{calls: &calls},
		Driver:    &stubDriver{calls: &calls},
		Workloads: map[string]engine.WorkloadAdapter{"go-tpc": &multiStepWorkload{calls: &calls}},
		Collector: cap,
	}

	s := minimalScenario()
	s.Suite = schema.Suite{
		Benchmarks: []schema.SuiteEntry{
			{
				Name: "supabase",
				Steps: []schema.SuiteStep{
					{Name: "warm-up", Type: "go-tpc", Command: "run"},
					{Name: "benchmark", Type: "go-tpc", Command: "run"},
				},
			},
		},
	}

	if err := r.Run(context.Background(), s, schema.ResolvedInputs{"warehouses": int64(10)}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	sm, ok := cap.metrics[engine.StructuredKey].(engine.StructuredMetrics)
	if !ok || len(sm.Points) != 2 {
		t.Fatalf("expected 2 structured points, got %#v", cap.metrics[engine.StructuredKey])
	}
	byStep := map[string]float64{}
	for _, p := range sm.Points {
		byStep[p.Labels["step"]] = p.Value
	}
	if byStep["warm-up"] != 100 || byStep["benchmark"] != 200 {
		t.Errorf("points not distinguishable by step label: %#v", sm.Points)
	}

	if _, ok := cap.metrics["raw_samples_csv"]; ok {
		t.Errorf("bare raw_samples_csv key should not survive the runner, got %v", cap.metrics["raw_samples_csv"])
	}
	warmupRaw, _ := cap.metrics[engine.RawSamplesCSVKey("warm-up")].(string)
	if !strings.Contains(warmupRaw, "1.0,NEW_ORDER,100") {
		t.Errorf("warm-up raw_samples_csv = %q", warmupRaw)
	}
	benchmarkRaw, _ := cap.metrics[engine.RawSamplesCSVKey("benchmark")].(string)
	if !strings.Contains(benchmarkRaw, "1.0,NEW_ORDER,200") {
		t.Errorf("benchmark raw_samples_csv = %q", benchmarkRaw)
	}
}

func contains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

func indexOf(slice []string, s string) int {
	for i, v := range slice {
		if v == s {
			return i
		}
	}
	return -1
}
