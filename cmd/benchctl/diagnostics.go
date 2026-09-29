package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dbarena/benchctl/internal/config"
	"github.com/dbarena/benchctl/internal/diagnostics"
	awsdiag "github.com/dbarena/benchctl/internal/diagnostics/aws"
	gcpdiag "github.com/dbarena/benchctl/internal/diagnostics/gcp"
	"github.com/dbarena/benchctl/internal/engine"
	"github.com/dbarena/benchctl/internal/runstate"
)

// buildDiagnosticsCollector resolves a vendor to a collector, mirroring
// buildTargetProvider. A nil collector and nil error means the vendor is
// recognised but has no collector yet.
func buildDiagnosticsCollector(cfg *config.Config, vendor string) (diagnostics.Collector, error) {
	switch vendor {
	case "":
		return nil, nil
	case diagnostics.VendorAWS:
		return awsdiag.New(cfg), nil
	case diagnostics.VendorGCP:
		return gcpdiag.New(cfg), nil
	case diagnostics.VendorSupabase:
		// Implementations land one vendor at a time.
		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported diagnostics collector %q", vendor)
	}
}

// collectDiagnostics gathers provider-side data into <dest>/diagnostics.
//
// It reports every problem and returns nil for all of them: `benchctl fetch`
// must keep exiting 0, because dbarenactl stops a whole sweep after three
// consecutive fetch failures.
func collectDiagnostics(ctx context.Context, state *runstate.State, dest string) {
	vendor := diagnostics.Vendor(state.TargetOutputs)
	if vendor == "" {
		fmt.Fprintf(os.Stderr, "diagnostics: target %q declares no cloud vendor, so there is nothing to collect; skipping\n", state.TargetProvider)
		return
	}

	collector, err := buildDiagnosticsCollector(appCfg, vendor)
	if err != nil {
		fmt.Fprintf(os.Stderr, "diagnostics: %v\n", err)
		return
	}
	if collector == nil {
		fmt.Fprintf(os.Stderr, "diagnostics: no collector implemented for %s yet; skipping\n", vendor)
		return
	}

	windows, err := diagnostics.LoadStepWindows(dest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "diagnostics: %v\n", err)
		return
	}
	if len(windows) == 0 {
		fmt.Fprintf(os.Stderr, "diagnostics: no %s in %s, so there is no benchmark window to collect over; skipping\n",
			engine.StepWindowsFilename, dest)
		return
	}

	diagDir := filepath.Join(dest, diagnostics.DirName)
	fmt.Fprintf(os.Stderr, "Collecting %s diagnostics over %d window(s); this queries several APIs and takes a while\n", vendor, len(windows))
	idx, err := diagnostics.Run(ctx, os.Stderr, collector, diagnostics.Request{
		RunID:   state.RunID,
		Outputs: state.TargetOutputs,
		Windows: windows,
		Dest:    diagDir,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "diagnostics: %v\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "Diagnostics (%s): %d artifacts over %d window(s) in %s\n",
		idx.Collector, len(idx.Artifacts), len(idx.Windows), diagDir)
}
