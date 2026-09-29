package engine_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dbarena/benchctl/internal/engine"
	"github.com/dbarena/benchctl/internal/runstate"
	"github.com/dbarena/benchctl/internal/schema"
)

// snapshottingStore wraps memStore and records a copy of the run's state
// after every Update call, so tests can assert on the sequence of states a
// run passed through, not just its final state.
type snapshottingStore struct {
	*memStore
	mu        sync.Mutex
	snapshots []*runstate.State
}

func newSnapshottingStore() *snapshottingStore {
	return &snapshottingStore{memStore: newMemStore()}
}

func (s *snapshottingStore) Update(id string, fn func(*runstate.State)) error {
	if err := s.memStore.Update(id, fn); err != nil {
		return err
	}
	// memStore.Load shallow-copies State, but Phases is a map: without a deep
	// copy here, every recorded snapshot would alias the same live map and
	// retroactively "see" later mutations, defeating the point of snapshotting.
	st, err := s.memStore.Load(id)
	if err != nil {
		return err
	}
	phases := make(map[runstate.Phase]runstate.Status, len(st.Phases))
	for k, v := range st.Phases {
		phases[k] = v
	}
	st.Phases = phases
	// Same aliasing hazard as Phases above, but for CurrentFixture.
	if st.CurrentFixture != nil {
		fixture := make(map[string]string, len(st.CurrentFixture))
		for k, v := range st.CurrentFixture {
			fixture[k] = v
		}
		st.CurrentFixture = fixture
	}
	s.mu.Lock()
	s.snapshots = append(s.snapshots, st)
	s.mu.Unlock()
	return nil
}

// multiIterationScenario returns a scenario with 2 iterations x 2 benchmark
// entries, matching the shape used to reproduce the "phase shows completed
// mid-run" bug (a status snapshot taken between entries used to show
// workload.execute/driver.collect as Completed even though more entries
// remained).
func multiIterationScenario() *schema.Scenario {
	s := minimalScenario()
	s.Suite = schema.Suite{
		Iterations: "2",
		Benchmarks: []schema.SuiteEntry{
			{Name: "bench-a", Steps: []schema.SuiteStep{{Name: "run", Type: "go-tpc", Command: "run"}}},
			{Name: "bench-b", Steps: []schema.SuiteStep{{Name: "run", Type: "go-tpc", Command: "run"}}},
		},
	}
	return s
}

// TestWorkloadExecuteAndCollect_OnlyCompleteOnFinalEntry verifies that, across
// a multi-iteration/multi-benchmark run, workload.execute and driver.collect
// only ever read StatusCompleted in the snapshot for the very last entry;
// never in between, where the old code flipped them to Completed after each
// entry and back to Running at the start of the next.
func TestWorkloadExecuteAndCollect_OnlyCompleteOnFinalEntry(t *testing.T) {
	var calls []string
	store := newSnapshottingStore()
	r := engine.Runner{
		Target:    &stubTarget{calls: &calls},
		Driver:    &stubDriver{calls: &calls},
		Workloads: map[string]engine.WorkloadAdapter{"go-tpc": &stubWorkload{calls: &calls}},
		Collector: &stubCollector{calls: &calls},
		Store:     store,
	}

	s := multiIterationScenario()
	if err := r.Run(context.Background(), s, schema.ResolvedInputs{"warehouses": int64(10)}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Once both phases read Completed together, they must never flip back to
	// non-completed afterward. The pre-fix code violated this: it flipped
	// both to Completed after each entry, then back to Running at the start
	// of the next entry, producing multiple separate "islands" of
	// completedness instead of one final, permanent one.
	firstBothCompleted := -1
	for i, snap := range store.snapshots {
		both := snap.Phases[runstate.PhaseWorkloadExecute] == runstate.StatusCompleted &&
			snap.Phases[runstate.PhaseDriverCollect] == runstate.StatusCompleted
		switch {
		case both && firstBothCompleted == -1:
			firstBothCompleted = i
		case !both && firstBothCompleted != -1:
			t.Errorf("snapshot %d: workload.execute/driver.collect flipped back to non-completed after both read completed at snapshot %d", i, firstBothCompleted)
		}
	}
	if firstBothCompleted == -1 {
		t.Fatal("workload.execute and driver.collect were never both observed completed")
	}
	// That first occurrence must not be before the run's last entry: with 2
	// iterations x 2 benchmarks (4 entries total), any earlier occurrence
	// means a phase was marked completed prematurely.
	if firstBothCompleted == 0 {
		t.Error("workload.execute/driver.collect completed on the very first state snapshot; suggests entries aren't being distinguished at all")
	}
}

// TestResume_CompletedAtAtomicWithFinalCollect verifies that CompletedAt is
// never observed set without driver.collect also reading Completed, and vice
// versa; the two used to be written in separate Store.Update transactions.
func TestResume_CompletedAtAtomicWithFinalCollect(t *testing.T) {
	var calls []string
	store := newSnapshottingStore()
	runID := "resume-test"

	initial := runstate.NewState(runID, "test", "", map[string]any{"warehouses": int64(10)})
	initial.Phases[runstate.PhaseDriverSetup] = runstate.StatusCompleted
	initial.TargetOutputs = map[string]string{}
	initial.DriverOutputs = map[string]string{}
	if err := store.Create(initial); err != nil {
		t.Fatalf("Create: %v", err)
	}

	r := engine.Runner{
		Target:    &stubTarget{calls: &calls},
		Driver:    &stubDriver{calls: &calls},
		Workloads: map[string]engine.WorkloadAdapter{"go-tpc": &stubWorkload{calls: &calls}},
		Collector: &stubCollector{calls: &calls},
		Store:     store,
	}

	s := multiIterationScenario()
	loaded, err := store.Load(runID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := r.Resume(context.Background(), s, loaded); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	for i, snap := range store.snapshots {
		hasCompletedAt := snap.CompletedAt != nil
		collectDone := snap.Phases[runstate.PhaseDriverCollect] == runstate.StatusCompleted
		if hasCompletedAt != collectDone {
			t.Errorf("snapshot %d: CompletedAt set = %v, driver.collect completed = %v; must always agree", i, hasCompletedAt, collectDone)
		}
	}

	final, err := store.Load(runID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if final.CompletedAt == nil {
		t.Error("final state: CompletedAt is nil, want set")
	}
	if final.Phases[runstate.PhaseDriverCollect] != runstate.StatusCompleted {
		t.Errorf("final state: driver.collect = %v, want completed", final.Phases[runstate.PhaseDriverCollect])
	}
}

// fixtureIterationScenario returns a scenario with a two-value "clients"
// list fixture, two iterations, and a single benchmark entry with a prepare
// and a benchmark step, enough combinations to exercise every axis
// (fixture, iteration, step) that CurrentFixture/CurrentIteration/CurrentStep
// need to track independently.
func fixtureIterationScenario() *schema.Scenario {
	s := minimalScenario()
	s.Suite.Iterations = "2"
	s.Suite.Fixtures = []schema.FixtureDef{
		{Name: "clients", Type: schema.FixtureTypeList, Params: map[string]any{"values": []any{"1", "2"}}},
	}
	return s
}

// TestCurrentStepState_TracksFixtureIterationAndStep verifies that as the
// run advances through fixtures x iterations x steps, state.json's
// CurrentFixture/CurrentIteration/CurrentStep/StepStartedAt reflect exactly
// the step being executed at each point, the detail external tooling (e.g.
// a pprof-capture script polling `benchctl status -o json`) needs to know
// when a specific fixture's benchmark step began.
func TestCurrentStepState_TracksFixtureIterationAndStep(t *testing.T) {
	var calls []string
	store := newSnapshottingStore()
	r := engine.Runner{
		Target:    &stubTarget{calls: &calls},
		Driver:    &stubDriver{calls: &calls},
		Workloads: map[string]engine.WorkloadAdapter{"go-tpc": &stubWorkload{calls: &calls}},
		Collector: &stubCollector{calls: &calls},
		Store:     store,
	}

	s := fixtureIterationScenario()
	before := time.Now().UTC()
	if err := r.Run(context.Background(), s, schema.ResolvedInputs{"warehouses": int64(10)}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	type want struct {
		iteration int
		clients   string
		step      string
	}
	// 2 iterations x 2 fixture values x 2 steps (prepare, benchmark) = 8
	// step transitions, in strict nesting order: iteration outermost,
	// fixture next, step innermost.
	wantSeq := []want{
		{0, "1", "prepare"}, {0, "1", "benchmark"},
		{0, "2", "prepare"}, {0, "2", "benchmark"},
		{1, "1", "prepare"}, {1, "1", "benchmark"},
		{1, "2", "prepare"}, {1, "2", "benchmark"},
	}

	var gotSeq []want
	for _, snap := range store.snapshots {
		if snap.CurrentStep == "" {
			continue // pre-workload snapshots (provision, driver.setup, ...)
		}
		if snap.StepStartedAt == nil {
			t.Errorf("snapshot with CurrentStep=%q has nil StepStartedAt", snap.CurrentStep)
		} else if snap.StepStartedAt.Before(before) {
			t.Errorf("StepStartedAt %v is before the run started (%v)", snap.StepStartedAt, before)
		}
		// Each step entry triggers several Store.Update calls (setting
		// CurrentStep, then flipping Phases running/completed) that all
		// carry the same (iteration, fixture, step), so collapse consecutive
		// duplicates down to one entry per distinct step transition.
		cur := want{snap.CurrentIteration, snap.CurrentFixture["clients"], snap.CurrentStep}
		if len(gotSeq) == 0 || gotSeq[len(gotSeq)-1] != cur {
			gotSeq = append(gotSeq, cur)
		}
	}

	if len(gotSeq) != len(wantSeq) {
		t.Fatalf("got %d step transitions, want %d\ngot:  %+v\nwant: %+v", len(gotSeq), len(wantSeq), gotSeq, wantSeq)
	}
	for i, w := range wantSeq {
		if gotSeq[i] != w {
			t.Errorf("transition %d = %+v, want %+v", i, gotSeq[i], w)
		}
	}
}

// countCalls returns how many entries of calls equal want.
func countCalls(calls []string, want string) int {
	n := 0
	for _, c := range calls {
		if c == want {
			n++
		}
	}
	return n
}

// prepareIterationScenario returns a scenario whose single benchmark entry
// loads data and then measures, repeated three times.
func prepareIterationScenario() *schema.Scenario {
	s := minimalScenario()
	s.Suite.Iterations = "3"
	return s
}

// TestPrepareRunsEveryIteration pins down that the data-load step belongs to
// the iteration, not to the run: every iteration re-creates the data it
// measures, because suite.between-benchmarks steps (reboot, wipe) are free to
// destroy it. Both entry points are covered, since Resume() carries the extra
// hazard of deciding what to skip from a phase map.
func TestPrepareRunsEveryIteration(t *testing.T) {
	t.Run("Run", func(t *testing.T) {
		var calls []string
		r := makeRunner(&calls)
		r.Store = newMemStore()
		if err := r.Run(context.Background(), prepareIterationScenario(), schema.ResolvedInputs{"warehouses": int64(10)}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if got := countCalls(calls, "workload.prepare"); got != 3 {
			t.Errorf("prepare ran %d times, want 3: %v", got, calls)
		}
	})

	t.Run("Resume", func(t *testing.T) {
		var calls []string
		store := newMemStore()
		runID := "prepare-per-iteration"
		initial := runstate.NewState(runID, "test", "", map[string]any{"warehouses": int64(10)})
		initial.Phases[runstate.PhaseDriverSetup] = runstate.StatusCompleted
		if err := store.Create(initial); err != nil {
			t.Fatalf("Create: %v", err)
		}
		r := makeRunner(&calls)
		r.Store = store
		loaded, err := store.Load(runID)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if err := r.Resume(context.Background(), prepareIterationScenario(), loaded); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if got := countCalls(calls, "workload.prepare"); got != 3 {
			t.Errorf("prepare ran %d times, want 3: %v", got, calls)
		}
	})
}

// TestResume_LegacyPreparePhaseIgnored covers a record written by a benchctl
// that still tracked a separate "workload.prepare" phase. The key survives in
// the stored phase map, and a resume must ignore it rather than read it as
// "the data is already loaded".
func TestResume_LegacyPreparePhaseIgnored(t *testing.T) {
	var calls []string
	store := newMemStore()
	runID := "legacy-prepare-phase"
	initial := runstate.NewState(runID, "test", "", map[string]any{"warehouses": int64(10)})
	initial.Phases[runstate.PhaseDriverSetup] = runstate.StatusCompleted
	initial.Phases["workload.prepare"] = runstate.StatusCompleted
	if err := store.Create(initial); err != nil {
		t.Fatalf("Create: %v", err)
	}
	r := makeRunner(&calls)
	r.Store = store
	loaded, err := store.Load(runID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := r.Resume(context.Background(), minimalScenario(), loaded); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got := countCalls(calls, "workload.prepare"); got != 1 {
		t.Errorf("prepare ran %d times, want 1: %v", got, calls)
	}
}

// TestResume_SkipsWorkloadWhenAlreadyCompleted guards the workloadPending
// hook that runSuiteStep uses to keep Run and Resume on one code path: a
// Resume whose workload.execute phase already completed on an earlier attempt
// must not re-run the load generator, but must still reach collection. Run
// has no such gate and always executes, which the other tests here cover.
func TestResume_SkipsWorkloadWhenAlreadyCompleted(t *testing.T) {
	var calls []string
	store := newSnapshottingStore()
	runID := "resume-skip-workload"

	initial := runstate.NewState(runID, "test", "", map[string]any{"warehouses": int64(10)})
	initial.Phases[runstate.PhaseDriverSetup] = runstate.StatusCompleted
	initial.Phases[runstate.PhaseWorkloadExecute] = runstate.StatusCompleted
	initial.TargetOutputs = map[string]string{}
	initial.DriverOutputs = map[string]string{}
	if err := store.Create(initial); err != nil {
		t.Fatalf("Create: %v", err)
	}

	r := engine.Runner{
		Target:    &stubTarget{calls: &calls},
		Driver:    &stubDriver{calls: &calls},
		Workloads: map[string]engine.WorkloadAdapter{"go-tpc": &stubWorkload{calls: &calls}},
		Collector: &stubCollector{calls: &calls},
		Store:     store,
	}

	loaded, err := store.Load(runID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := r.Resume(context.Background(), multiIterationScenario(), loaded); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	var collected bool
	for _, c := range calls {
		if c == "workload.run" {
			t.Fatalf("ran the workload adapter although workload.execute was already completed; calls: %v", calls)
		}
		if c == "collector.collect" {
			collected = true
		}
	}
	if !collected {
		t.Errorf("Resume never reached collection; calls: %v", calls)
	}
}
