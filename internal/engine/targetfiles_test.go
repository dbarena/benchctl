package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dbarena/benchctl/internal/engine"
	"github.com/dbarena/benchctl/internal/runstate"
	"github.com/dbarena/benchctl/internal/schema"
)

// filesTarget is a stubTarget that wants a file shipped to the driver.
type filesTarget struct {
	stubTarget
	gotOutputs engine.Outputs
}

func (f *filesTarget) TargetFiles(outputs engine.Outputs) map[string]string {
	f.gotOutputs = outputs
	return map[string]string{"/local/targets.json": engine.TargetsFile}
}

func TestRunAsync_ShipsTargetFiles(t *testing.T) {
	var calls []string
	target := &filesTarget{stubTarget: stubTarget{calls: &calls}}
	driver := &stubAsyncDriver{stubDriver: stubDriver{calls: &calls}}
	r := newAsyncRunner(&calls, newMemStore())
	r.Target = target
	r.Driver = driver

	if _, err := r.RunAsync(context.Background(), asyncScenario(), schema.ResolvedInputs{"warehouses": int64(10)}); err != nil {
		t.Fatalf("RunAsync: %v", err)
	}
	if got := driver.request.Files["/local/targets.json"]; got != engine.TargetsFile {
		t.Errorf("Files = %v, want /local/targets.json -> %s", driver.request.Files, engine.TargetsFile)
	}
	if target.gotOutputs["host"] != "localhost" {
		t.Errorf("TargetFiles got outputs %v, want the target's own", target.gotOutputs)
	}
}

// outputsWorkload records the outputs each step was handed.
type outputsWorkload struct{ got engine.Outputs }

func (o *outputsWorkload) Run(_ context.Context, outputs engine.Outputs, _ schema.SuiteStep) (engine.Metrics, error) {
	o.got = outputs
	return engine.Metrics{}, nil
}

// TestResume_ReportsShippedTargetsFile: on the driver the targets file sits
// next to the scenario, and workload adapters get its absolute path.
func TestResume_ReportsShippedTargetsFile(t *testing.T) {
	dir := t.TempDir()
	targets := filepath.Join(dir, engine.TargetsFile)
	if err := os.WriteFile(targets, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls []string
	store := newMemStore()
	st := runstate.NewState("resume-targets", "test", "", map[string]any{"warehouses": int64(10)})
	st.TargetOutputs = map[string]string{"host": "localhost"}
	if err := store.Create(st); err != nil {
		t.Fatalf("Create: %v", err)
	}
	w := &outputsWorkload{}
	r := makeRunner(&calls)
	r.Workloads = map[string]engine.WorkloadAdapter{"go-tpc": w}
	r.Store = store
	r.ScenarioPath = filepath.Join(dir, "scenario.yaml")
	loaded, err := store.Load("resume-targets")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// A service entry (`using`) only sees that service's outputs, so use none.
	s := minimalScenario()
	s.Suite.Benchmarks[0].Using = ""
	if err := r.Resume(context.Background(), s, loaded); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got := w.got[engine.OutputKeyTargetsFile]; got != targets {
		t.Errorf("%s = %q, want %q", engine.OutputKeyTargetsFile, got, targets)
	}
}

// TestRun_ReportsLocalTargetsFile: a synchronous run executes workloads where
// the target wrote its targets file.
func TestRun_ReportsLocalTargetsFile(t *testing.T) {
	var calls []string
	w := &outputsWorkload{}
	r := makeRunner(&calls)
	r.Target = &filesTarget{stubTarget: stubTarget{calls: &calls}}
	r.Workloads = map[string]engine.WorkloadAdapter{"go-tpc": w}

	s := minimalScenario()
	s.Suite.Benchmarks[0].Using = ""
	if err := r.Run(context.Background(), s, schema.ResolvedInputs{"warehouses": int64(10)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := w.got[engine.OutputKeyTargetsFile]; got != "/local/targets.json" {
		t.Errorf("%s = %q, want /local/targets.json", engine.OutputKeyTargetsFile, got)
	}
}
