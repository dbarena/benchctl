package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/dbarena/benchctl/internal/engine"
	"github.com/dbarena/benchctl/internal/runstate"
	"github.com/dbarena/benchctl/internal/schema"
)

func loadStepWindows(t *testing.T) []engine.StepWindow {
	t.Helper()
	data, err := os.ReadFile(engine.StepWindowsFilename)
	if err != nil {
		t.Fatalf("read %s: %v", engine.StepWindowsFilename, err)
	}
	var got []engine.StepWindow
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal %s: %v\ncontent: %s", engine.StepWindowsFilename, err, data)
	}
	return got
}

// TestRun_WritesStepWindowsPerBenchmarkAndIteration verifies a full run labels
// every window with the benchmark entry and 1-based iteration it belongs to,
// in execution order. The labels are the whole point: a diagnostics fetch has
// to ask "which window was fixture X, iteration 2 of benchmark bench-b", and
// run state cannot answer that (it keeps one CurrentStep, overwritten).
//
// Iteration is 1-based to match the label the collector writes into
// results_*.json, so a window and a result file for the same step agree.
func TestRun_WritesStepWindowsPerBenchmarkAndIteration(t *testing.T) {
	t.Chdir(t.TempDir())
	var calls []string
	r := engine.Runner{
		Target:    &stubTarget{calls: &calls},
		Driver:    &stubDriver{calls: &calls},
		Workloads: map[string]engine.WorkloadAdapter{"go-tpc": &stubWorkload{calls: &calls}},
		Collector: &stubCollector{calls: &calls},
		Store:     newMemStore(),
	}

	// 2 iterations x 2 benchmark entries, one workload step each.
	if err := r.Run(context.Background(), multiIterationScenario(), schema.ResolvedInputs{"warehouses": int64(10)}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := loadStepWindows(t)
	type key struct {
		benchmark string
		iteration int
	}
	want := []key{{"bench-a", 1}, {"bench-b", 1}, {"bench-a", 2}, {"bench-b", 2}}
	if len(got) != len(want) {
		t.Fatalf("%d windows, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Benchmark != w.benchmark || got[i].Iteration != w.iteration {
			t.Errorf("window %d = %s/iter %d, want %s/iter %d", i, got[i].Benchmark, got[i].Iteration, w.benchmark, w.iteration)
		}
		if got[i].Step != "run" || got[i].Type != "go-tpc" {
			t.Errorf("window %d = step %q type %q, want run/go-tpc", i, got[i].Step, got[i].Type)
		}
		if got[i].EndedAt.Before(got[i].StartedAt) {
			t.Errorf("window %d ends before it starts: [%s, %s]", i, got[i].StartedAt, got[i].EndedAt)
		}
		if got[i].Failed {
			t.Errorf("window %d marked failed on a successful run", i)
		}
	}
}

// TestRun_StepWindowsCarryFixtureValues verifies each window records the
// fixture combination it ran under. Without it, a sweep's windows are
// indistinguishable and a diagnostics fetch cannot attribute a Performance
// Insights slice to a client count.
func TestRun_StepWindowsCarryFixtureValues(t *testing.T) {
	t.Chdir(t.TempDir())
	var calls []string
	r := engine.Runner{
		Target:    &stubTarget{calls: &calls},
		Driver:    &stubDriver{calls: &calls},
		Workloads: map[string]engine.WorkloadAdapter{"go-tpc": &stubWorkload{calls: &calls}},
		Collector: &stubCollector{calls: &calls},
		Store:     newMemStore(),
	}

	// 2 iterations x a two-value "clients" fixture x (prepare + benchmark).
	if err := r.Run(context.Background(), fixtureIterationScenario(), schema.ResolvedInputs{"warehouses": int64(10)}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := loadStepWindows(t)
	if len(got) == 0 {
		t.Fatal("no windows recorded")
	}
	seen := map[string]bool{}
	for _, w := range got {
		if w.Fixture["clients"] == "" {
			t.Fatalf("window %s/iter %d has no clients fixture value: %+v", w.Step, w.Iteration, w.Fixture)
		}
		seen[w.Fixture["clients"]] = true
	}
	for _, want := range []string{"1", "2"} {
		if !seen[want] {
			t.Errorf("no window recorded for clients=%s; got %v", want, seen)
		}
	}
}

// TestRun_FailedWorkloadStepStillGetsAWindow is the case the feature exists
// for: when a benchmark blows up, its window is what someone needs to go read
// the provider's wait events and logs over.
func TestRun_FailedWorkloadStepStillGetsAWindow(t *testing.T) {
	t.Chdir(t.TempDir())
	var calls []string
	r := engine.Runner{
		Target:    &stubTarget{calls: &calls},
		Driver:    &stubDriver{calls: &calls},
		Workloads: map[string]engine.WorkloadAdapter{"go-tpc": &stubWorkload{calls: &calls, runErr: errors.New("boom")}},
		Collector: &stubCollector{calls: &calls},
		Store:     newMemStore(),
	}

	// minimalScenario runs a prepare step, which succeeds, then a benchmark
	// step, which stubWorkload fails.
	if err := r.Run(context.Background(), minimalScenario(), schema.ResolvedInputs{"warehouses": int64(10)}); err == nil {
		t.Fatal("Run succeeded, want the workload error")
	}

	got := loadStepWindows(t)
	if len(got) != 2 {
		t.Fatalf("%d windows, want 2 (prepare then benchmark): %+v", len(got), got)
	}
	if got[0].Step != "prepare" || got[0].Failed {
		t.Errorf("window 0 = %q failed=%v, want prepare failed=false", got[0].Step, got[0].Failed)
	}
	if got[1].Step != "benchmark" || !got[1].Failed {
		t.Errorf("window 1 = %q failed=%v, want benchmark failed=true", got[1].Step, got[1].Failed)
	}
}

// TestResume_WritesStepWindows verifies the driver-side entry point records
// windows too. This is the path that matters in practice: every cloud scenario
// runs with --async, so the suite executes under Resume on the load-driver
// instance, and step_windows.json has to be there for `benchctl fetch` to copy
// down alongside results_*.json.
func TestResume_WritesStepWindows(t *testing.T) {
	t.Chdir(t.TempDir())
	var calls []string
	store := newMemStore()
	runID := "resume-windows"

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

	loaded, err := store.Load(runID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := r.Resume(context.Background(), multiIterationScenario(), loaded); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	if got := loadStepWindows(t); len(got) != 4 {
		t.Fatalf("%d windows, want 4 (2 iterations x 2 benchmarks): %+v", len(got), got)
	}
}
