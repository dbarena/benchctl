package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/dbarena/benchctl/internal/engine"
)

var (
	fetchDest          string
	fetchNoDiagnostics bool
)

var fetchCmd = &cobra.Command{
	Use:   "fetch <run-id>",
	Short: "Copy result files and logs from the driver instance to the local machine",
	Args:  cobra.ExactArgs(1),
	RunE:  runFetch,
}

func init() {
	fetchCmd.Flags().StringVar(&fetchDest, "dest", ".", "Local directory to copy artifacts into (created if needed)")
	fetchCmd.Flags().BoolVar(&fetchNoDiagnostics, "no-diagnostics", false, "Skip provider-side diagnostics collection")
}

func runFetch(_ *cobra.Command, args []string) error {
	runID := args[0]

	store, err := openStore()
	if err != nil {
		return err
	}
	state, err := store.Load(runID)
	if err != nil {
		return err
	}

	// Restore OpenTofu work dirs when the run was provisioned on a different
	// machine (e.g. dbarena resuming on a machine other than the one that
	// launched the run), the same as connect and teardown.
	if state.TofuState != "" {
		if err := restoreTofuWorkDirs(state, false); err != nil {
			return fmt.Errorf("restore tofu state: %w", err)
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// A run with no driver outputs (driver: local) has no remote artifacts to
	// copy, but its target may still have provider-side diagnostics worth
	// collecting, so this is a note rather than a failure.
	if len(state.DriverOutputs) == 0 {
		fmt.Fprintf(os.Stderr, "run %s: driver was not provisioned; no remote artifacts to copy\n", runID)
	} else if err := fetchDriverArtifacts(ctx, state.DriverProvider, state.DriverOutputs); err != nil {
		return err
	} else {
		fmt.Fprintf(os.Stderr, "Artifacts fetched to %s\n", fetchDest)
	}

	if !fetchNoDiagnostics {
		collectDiagnostics(ctx, state, fetchDest)
	}
	return nil
}

func fetchDriverArtifacts(ctx context.Context, providerName string, outputs map[string]string) error {
	driver, err := buildDriverProvider(appCfg, providerName)
	if err != nil {
		return err
	}
	fetcher, ok := driver.(engine.ArtifactFetcher)
	if !ok {
		return fmt.Errorf("driver provider %q does not support fetch", providerName)
	}
	return fetcher.FetchArtifacts(ctx, engine.Outputs(outputs), fetchDest)
}
