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

var fetchDest string

var fetchCmd = &cobra.Command{
	Use:   "fetch <run-id>",
	Short: "Copy result files and logs from the driver instance to the local machine",
	Args:  cobra.ExactArgs(1),
	RunE:  runFetch,
}

func init() {
	fetchCmd.Flags().StringVar(&fetchDest, "dest", ".", "Local directory to copy artifacts into (created if needed)")
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

	if len(state.DriverOutputs) == 0 {
		return fmt.Errorf("run %s: driver was not provisioned, so there is nothing to fetch", runID)
	}

	driver, err := buildDriverProvider(appCfg, state.DriverProvider)
	if err != nil {
		return err
	}
	fetcher, ok := driver.(engine.ArtifactFetcher)
	if !ok {
		return fmt.Errorf("driver provider %q does not support fetch", state.DriverProvider)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := fetcher.FetchArtifacts(ctx, engine.Outputs(state.DriverOutputs), fetchDest); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Artifacts fetched to %s\n", fetchDest)
	return nil
}
