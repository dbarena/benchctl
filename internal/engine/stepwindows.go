package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// StepWindowsFilename is the artifact the runner writes into its working
// directory, next to the collector's results_*.json, recording the wall-clock
// interval each measured suite step occupied.
//
// It exists because nothing else in a run records one. Run state carries a
// single StepStartedAt that every subsequent step overwrites, and no step end
// time at all, while every provider-side diagnostics API (Performance
// Insights, CloudWatch, Cloud Monitoring, the Supabase log endpoint) is
// queried over a [start, end] window. It is also directly useful on its own:
// it answers how long each fixture's benchmark step actually took.
const StepWindowsFilename = "step_windows.json"

// StepWindow is the interval one measured suite step occupied.
//
// Iteration is 1-based, matching the "iteration" label the collector writes
// into results_*.json rather than the 0-based loop counter in run state, so
// that a window and a result file for the same step agree.
type StepWindow struct {
	Step      string            `json:"step"`
	Type      string            `json:"type"`
	Benchmark string            `json:"benchmark"`
	Iteration int               `json:"iteration"`
	Fixture   map[string]string `json:"fixture,omitempty"`
	StartedAt time.Time         `json:"started_at"`
	EndedAt   time.Time         `json:"ended_at"`
	// Failed marks a step that returned an error. Its window is recorded all
	// the same: a run that fails partway is exactly when provider-side
	// diagnostics are most worth having.
	Failed bool `json:"failed,omitempty"`
}

// recordStepWindow appends w and rewrites StepWindowsFilename.
//
// It rewrites on every call rather than once at the end of the suite so the
// file is complete for every step that finished even when the run is killed
// mid-benchmark. A suite produces tens of entries, so the cost is irrelevant
// beside the steps themselves.
//
// A write failure is reported once and otherwise swallowed. Losing the
// diagnostics windows of a benchmark that otherwise succeeded is not worth
// failing the run over, and repeating the same warning for every remaining
// step would bury the run's real output.
func (r *Runner) recordStepWindow(w StepWindow) {
	r.stepWindows = append(r.stepWindows, w)
	if r.stepWindowsUnwritable {
		return
	}
	data, err := json.MarshalIndent(r.stepWindows, "", "  ")
	if err != nil {
		// Unreachable for this struct, but handled rather than discarded so a
		// later field that cannot marshal surfaces instead of failing silently.
		r.warnStepWindows("encode %s: %v", StepWindowsFilename, err)
		return
	}
	if err := os.WriteFile(StepWindowsFilename, append(data, '\n'), 0o644); err != nil {
		r.warnStepWindows("write %s: %v", StepWindowsFilename, err)
	}
}

func (r *Runner) warnStepWindows(format string, args ...any) {
	r.stepWindowsUnwritable = true
	if r.Out != nil {
		fmt.Fprintf(r.Out, "warning: step windows will be missing from this run's artifacts: "+format+"\n", args...)
	}
}
