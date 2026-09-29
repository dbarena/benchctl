package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dbarena/benchctl/internal/diagnostics"
	"github.com/dbarena/benchctl/internal/engine"
	"github.com/dbarena/benchctl/internal/runstate"
)

func TestBuildDiagnosticsCollector_RejectsUnknownVendors(t *testing.T) {
	if _, err := buildDiagnosticsCollector(nil, "azure"); err == nil {
		t.Error("expected an error for a vendor with no collector")
	}
	// Recognised vendors resolve without error even before their collector
	// exists, so the caller can say "not implemented yet" rather than "bad
	// configuration".
	for _, name := range []string{"", diagnostics.VendorAWS, diagnostics.VendorGCP, diagnostics.VendorSupabase} {
		if _, err := buildDiagnosticsCollector(nil, name); err != nil {
			t.Errorf("buildDiagnosticsCollector(%q): %v", name, err)
		}
	}
}

// TestCollectDiagnostics_NoDeclarationIsQuietAndHarmless covers every local
// and self-hosted run: nothing declared, one line of explanation, no directory
// created, and above all no error. `benchctl fetch` has to keep exiting 0 or
// dbarenactl stops the sweep.
func TestCollectDiagnostics_NoDeclarationIsQuietAndHarmless(t *testing.T) {
	dest := t.TempDir()
	state := &runstate.State{
		RunID:          "local-run",
		TargetProvider: "docker-compose",
		TargetOutputs:  map[string]string{"host": "localhost", "port": "5432"},
		StartedAt:      time.Now().UTC().Add(-time.Hour),
	}

	collectDiagnostics(context.Background(), state, dest)

	if _, err := os.Stat(filepath.Join(dest, diagnostics.DirName)); !os.IsNotExist(err) {
		t.Errorf("a diagnostics directory was created for a target that declares nothing (err=%v)", err)
	}
}

// TestCollectDiagnostics_DeclaredVendorWithoutCollector is the state this PR
// ships in: the declaration is read, no implementation exists yet, and the
// fetch survives it untouched.
func TestCollectDiagnostics_DeclaredVendorWithoutCollector(t *testing.T) {
	dest := t.TempDir()
	state := &runstate.State{
		RunID:          "rds-run",
		TargetProvider: "opentofu",
		TargetOutputs: map[string]string{
			"dbi_resource_id":           "db-ABC123",
			diagnostics.VendorOutputKey: diagnostics.VendorAWS,
		},
		StartedAt: time.Now().UTC().Add(-time.Hour),
	}

	collectDiagnostics(context.Background(), state, dest)

	if _, err := os.Stat(filepath.Join(dest, diagnostics.DirName)); !os.IsNotExist(err) {
		t.Errorf("a diagnostics directory was created with no collector to fill it (err=%v)", err)
	}
}

// TestCollectDiagnostics_MissingStepWindowsSkips covers a run whose driver is
// already gone, so `benchctl fetch` copied no step_windows.json. There is no
// run-span fallback on purpose: a window spanning provisioning would
// attribute minutes of idle time to the benchmark.
func TestCollectDiagnostics_MissingStepWindowsSkips(t *testing.T) {
	dest := t.TempDir()
	state := &runstate.State{
		RunID:          "rds-run",
		TargetProvider: "opentofu",
		TargetOutputs:  map[string]string{diagnostics.VendorOutputKey: diagnostics.VendorAWS},
		StartedAt:      time.Now().UTC().Add(-time.Hour),
	}

	collectDiagnostics(context.Background(), state, dest)

	if _, err := os.Stat(filepath.Join(dest, diagnostics.DirName)); !os.IsNotExist(err) {
		t.Errorf("collection proceeded without any benchmark window (err=%v)", err)
	}
}

// TestCollectDiagnostics_UsesFetchedStepWindows checks the join with the
// runner: the step_windows.json that FetchArtifacts copies into the
// destination is where the collection windows come from, and only the
// benchmark interval is used rather than the whole run.
func TestCollectDiagnostics_UsesFetchedStepWindows(t *testing.T) {
	dest := t.TempDir()
	start := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	data, err := json.Marshal([]engine.StepWindow{
		{Step: "benchmark", Benchmark: "rds", Iteration: 1, Fixture: map[string]string{"client_threads": "8"},
			StartedAt: start, EndedAt: start.Add(time.Hour)},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dest, engine.StepWindowsFilename), data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	windows, err := diagnostics.LoadStepWindows(dest)
	if err != nil {
		t.Fatalf("LoadStepWindows: %v", err)
	}
	if len(windows) != 1 || windows[0].Name != "benchmark" {
		t.Fatalf("got %+v, want the fetched benchmark window", windows)
	}
	if d := windows[0].End.Sub(windows[0].Start); d != time.Hour {
		t.Errorf("window is %s, want 1h", d)
	}
}
