package diagnostics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dbarena/benchctl/internal/engine"
)

var (
	t0 = time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	t1 = t0.Add(30 * time.Minute)
	t2 = t0.Add(90 * time.Minute)
)

func writeStepWindows(t *testing.T, dir string, steps []engine.StepWindow) {
	t.Helper()
	data, err := json.Marshal(steps)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, engine.StepWindowsFilename), data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// TestLoadStepWindows_CarriesAttribution checks the join between the runner's
// step windows and a collection window. The benchmark, iteration and fixture
// have to survive, because attributing a Performance Insights slice to a
// client count is the whole reason for collecting per window.
func TestLoadStepWindows_CarriesAttribution(t *testing.T) {
	dir := t.TempDir()
	writeStepWindows(t, dir, []engine.StepWindow{
		{Step: "warmup", Benchmark: "rds", Iteration: 1, Fixture: map[string]string{"client_threads": "8"}, StartedAt: t0, EndedAt: t1},
		{Step: "benchmark", Benchmark: "rds", Iteration: 2, Fixture: map[string]string{"client_threads": "8"}, StartedAt: t1, EndedAt: t2},
	})

	got, err := LoadStepWindows(dir)
	if err != nil {
		t.Fatalf("LoadStepWindows: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("%d windows, want 2: %+v", len(got), got)
	}
	w := got[1]
	if w.Name != "benchmark" || w.Benchmark != "rds" || w.Iteration != 2 {
		t.Errorf("attribution lost: %+v", w)
	}
	if w.Fixture["client_threads"] != "8" {
		t.Errorf("fixture lost: %v", w.Fixture)
	}
	if !w.Start.Equal(t1) || !w.End.Equal(t2) {
		t.Errorf("window = [%s, %s], want [%s, %s]", w.Start, w.End, t1, t2)
	}
}

// TestLoadStepWindows_SkipsZeroLengthSteps guards against querying a provider
// over an empty interval, which every monitoring API either rejects or answers
// with nothing.
func TestLoadStepWindows_SkipsZeroLengthSteps(t *testing.T) {
	dir := t.TempDir()
	writeStepWindows(t, dir, []engine.StepWindow{
		{Step: "skipped", StartedAt: t0, EndedAt: t0},
		{Step: "real", StartedAt: t0, EndedAt: t1},
	})

	got, err := LoadStepWindows(dir)
	if err != nil {
		t.Fatalf("LoadStepWindows: %v", err)
	}
	if len(got) != 1 || got[0].Name != "real" {
		t.Fatalf("got %+v, want only the real window", got)
	}
}

// TestLoadStepWindows_KeepsFailedSteps is the counterpart: a step that errored
// still occupied real time, and a failed benchmark is exactly when the
// provider's view is worth reading.
func TestLoadStepWindows_KeepsFailedSteps(t *testing.T) {
	dir := t.TempDir()
	writeStepWindows(t, dir, []engine.StepWindow{{Step: "benchmark", StartedAt: t0, EndedAt: t1, Failed: true}})

	got, err := LoadStepWindows(dir)
	if err != nil {
		t.Fatalf("LoadStepWindows: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("failed step dropped: %+v", got)
	}
}

// TestLoadStepWindows_MissingFileIsNotAnError covers a run that predates step
// windows or used the local driver. The caller reports it and skips; there is
// no run-span fallback, because a window that includes provisioning would
// attribute idle time to the benchmark.
func TestLoadStepWindows_MissingFileIsNotAnError(t *testing.T) {
	got, err := LoadStepWindows(t.TempDir())
	if err != nil {
		t.Fatalf("LoadStepWindows on an empty dir: %v", err)
	}
	if got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

func TestLoadStepWindows_CorruptFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, engine.StepWindowsFilename), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadStepWindows(dir); err == nil {
		t.Error("expected a parse error, got nil")
	}
}

func TestWindowSlug(t *testing.T) {
	tests := []struct {
		name string
		w    Window
		want string
	}{
		{"plain", Window{Name: "run"}, "run"},
		{"with iteration", Window{Name: "benchmark", Iteration: 2}, "benchmark_iter2"},
		{
			name: "fixture values sorted for stability",
			w:    Window{Name: "benchmark", Iteration: 1, Fixture: map[string]string{"scale": "10", "client_threads": "8"}},
			want: "benchmark_iter1_client_threads-8_scale-10",
		},
		{
			name: "unsafe characters replaced",
			w:    Window{Name: "run/../etc", Iteration: 1, Fixture: map[string]string{"proto": "simple extended"}},
			want: "run____etc_iter1_proto-simple_extended",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.w.Slug(); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
