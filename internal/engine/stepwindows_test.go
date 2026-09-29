package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dbarena/benchctl/internal/schema"
)

// windowStubWorkload is a minimal WorkloadAdapter for exercising step
// dispatch without a load generator.
type windowStubWorkload struct{}

func (windowStubWorkload) Run(context.Context, Outputs, schema.SuiteStep) (Metrics, error) {
	return Metrics{}, nil
}

// dispatchEnv returns a suiteStepEnv whose stepOutputs are deliberately
// empty. Every SQL-backed step type then fails in buildConnString, before any
// network I/O, which is what lets this test cover them at all.
func dispatchEnv() *suiteStepEnv {
	var info map[string]string
	var metrics Metrics
	return &suiteStepEnv{
		runID:           "run-1",
		tc:              &TemplateContext{},
		stepOutputs:     Outputs{},
		benchmark:       "bench",
		infoMap:         &info,
		workloadMetrics: &metrics,
		workloadPending: func() bool { return true },
		failWorkload:    func(err error) error { return err },
	}
}

// TestDispatchSuiteStep_MeasuredStepTypes pins which step types earn an entry
// in step_windows.json. Only steps that occupy real time qualify: a workload
// step, and a `type: collect` step, whose sampling instant a caller wants to
// line up against provider-side data. Letting `metadata` and `sql` in would
// bury the handful of intervals that matter under dozens of near-zero ones (a
// single RDS fixture runs six metadata steps).
//
// The measured verdict is deliberately independent of success: a step that
// failed is exactly when its window is worth having.
func TestDispatchSuiteStep_MeasuredStepTypes(t *testing.T) {
	r := &Runner{Workloads: map[string]WorkloadAdapter{"go-tpc": windowStubWorkload{}}}

	tests := []struct {
		name         string
		step         schema.SuiteStep
		args         map[string]string
		wantMeasured bool
		wantErr      bool
	}{
		{
			name:         "metadata is bookkeeping",
			step:         schema.SuiteStep{Name: "pg-version", Type: "metadata", Command: "sql"},
			args:         map[string]string{"name": "pg_version", "query": "SELECT version()"},
			wantMeasured: false,
			wantErr:      true,
		},
		{
			name:         "sql is bookkeeping",
			step:         schema.SuiteStep{Name: "checkpoint", Type: "sql"},
			args:         map[string]string{"query": "CHECKPOINT"},
			wantMeasured: false,
			wantErr:      true,
		},
		{
			name:         "collect samples the target",
			step:         schema.SuiteStep{Name: "pg-stat-io", Type: "collect"},
			args:         map[string]string{"family": "pg_stat_io", "query": "SELECT 'reads', 1.0"},
			wantMeasured: true,
			wantErr:      true,
		},
		{
			name:         "workload occupies real time",
			step:         schema.SuiteStep{Name: "benchmark", Type: "go-tpc", Command: "run"},
			wantMeasured: true,
			wantErr:      false,
		},
		{
			name:         "unknown adapter never ran",
			step:         schema.SuiteStep{Name: "benchmark", Type: "nope"},
			wantMeasured: false,
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			measured, err := r.dispatchSuiteStep(context.Background(), dispatchEnv(), tt.step, tt.args, false)
			if measured != tt.wantMeasured {
				t.Errorf("measured = %v, want %v", measured, tt.wantMeasured)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, want error: %v", err, tt.wantErr)
			}
		})
	}
}

// TestDispatchSuiteStep_SkippedWorkloadIsNotMeasured covers the resume case:
// a workload step whose phase already completed on an earlier attempt does
// not run, so recording a window for it would report the benchmark as having
// taken no time at all.
func TestDispatchSuiteStep_SkippedWorkloadIsNotMeasured(t *testing.T) {
	r := &Runner{Workloads: map[string]WorkloadAdapter{"go-tpc": windowStubWorkload{}}}
	env := dispatchEnv()
	env.workloadPending = func() bool { return false }

	measured, err := r.dispatchSuiteStep(context.Background(), env, schema.SuiteStep{Name: "benchmark", Type: "go-tpc", Command: "run"}, nil, false)
	if err != nil {
		t.Fatalf("dispatchSuiteStep: %v", err)
	}
	if measured {
		t.Error("measured = true for a skipped workload step, want false")
	}
}

func readStepWindows(t *testing.T) []StepWindow {
	t.Helper()
	data, err := os.ReadFile(StepWindowsFilename)
	if err != nil {
		t.Fatalf("read %s: %v", StepWindowsFilename, err)
	}
	var got []StepWindow
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal %s: %v\ncontent: %s", StepWindowsFilename, err, data)
	}
	return got
}

// TestRecordStepWindow_FileCompleteAfterEveryStep verifies the file is
// rewritten as the suite advances rather than once at the end, so a run killed
// mid-benchmark still yields windows for the steps that finished. That matters
// because a run that died is a run whose provider-side diagnostics someone
// wants to go read.
func TestRecordStepWindow_FileCompleteAfterEveryStep(t *testing.T) {
	t.Chdir(t.TempDir())
	r := &Runner{}
	start := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)

	r.recordStepWindow(StepWindow{
		Step: "warmup", Type: "go-tpc", Benchmark: "rds", Iteration: 1,
		Fixture:   map[string]string{"client_threads": "8"},
		StartedAt: start, EndedAt: start.Add(30 * time.Minute),
	})
	if got := readStepWindows(t); len(got) != 1 {
		t.Fatalf("after one step: %d windows, want 1", len(got))
	}

	r.recordStepWindow(StepWindow{
		Step: "benchmark", Type: "go-tpc", Benchmark: "rds", Iteration: 1,
		Fixture:   map[string]string{"client_threads": "8"},
		StartedAt: start.Add(30 * time.Minute), EndedAt: start.Add(90 * time.Minute),
	})

	got := readStepWindows(t)
	if len(got) != 2 {
		t.Fatalf("after two steps: %d windows, want 2", len(got))
	}
	if got[0].Step != "warmup" || got[1].Step != "benchmark" {
		t.Errorf("windows out of execution order: %q then %q", got[0].Step, got[1].Step)
	}
	if got[1].Fixture["client_threads"] != "8" {
		t.Errorf("fixture = %v, want client_threads=8", got[1].Fixture)
	}
	// Round-tripping through RFC 3339 must preserve the window a diagnostics
	// query will be built from.
	if !got[1].StartedAt.Equal(start.Add(30*time.Minute)) || !got[1].EndedAt.Equal(start.Add(90*time.Minute)) {
		t.Errorf("window = [%s, %s], want [%s, %s]", got[1].StartedAt, got[1].EndedAt, start.Add(30*time.Minute), start.Add(90*time.Minute))
	}
}

// TestRecordStepWindow_UnwritableWarnsOnce verifies a run keeps going when the
// file cannot be written, and that it says so once rather than once per step.
// A 6-fixture sweep would otherwise print dozens of identical warnings and
// bury the run's real output.
func TestRecordStepWindow_UnwritableWarnsOnce(t *testing.T) {
	t.Chdir(t.TempDir())
	// A directory in the file's place makes WriteFile fail without depending
	// on permission bits, which a root-run CI container would ignore.
	if err := os.Mkdir(StepWindowsFilename, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	var out bytes.Buffer
	r := &Runner{Out: &out}
	for i := 0; i < 3; i++ {
		r.recordStepWindow(StepWindow{Step: "benchmark", Type: "go-tpc"})
	}

	if n := strings.Count(out.String(), "warning:"); n != 1 {
		t.Errorf("printed %d warnings, want 1\ngot: %s", n, out.String())
	}
	if len(r.stepWindows) != 3 {
		t.Errorf("kept %d windows in memory, want 3", len(r.stepWindows))
	}
}
