package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/dbarena/benchctl/internal/engine"
	"github.com/dbarena/benchctl/internal/runstate"
	"github.com/dbarena/benchctl/internal/tofustate"
)

var (
	teardownForce bool
	teardownPurge bool
)

var teardownCmd = &cobra.Command{
	Use:   "teardown <run-id>",
	Short: "Tear down infrastructure for a completed or failed run",
	Args:  cobra.ExactArgs(1),
	RunE:  runTeardown,
}

func init() {
	teardownCmd.Flags().BoolVar(&teardownForce, "force", false, "Re-run teardown even when the run is already marked terminated; restores from the state blob rather than local state")
	_ = teardownCmd.Flags().MarkHidden("force")
	teardownCmd.Flags().BoolVar(&teardownPurge, "purge", false, "Delete the run record from the store after a successful teardown")
}

func runTeardown(_ *cobra.Command, args []string) error {
	runID := args[0]

	store, err := openStore()
	if err != nil {
		return err
	}

	state, err := store.Load(runID)
	if err != nil {
		return err
	}

	provisioned := state.TargetOutputs != nil
	if !provisioned {
		fmt.Fprintf(os.Stderr, "run %s: provisioning recorded no resources for this run, so there is nothing to tear down from local state\n", runID)
	}
	if state.Phases[runstate.PhaseTeardown] == runstate.StatusCompleted && !teardownForce {
		fmt.Fprintf(os.Stderr, "run %s: teardown already completed\n", runID)
		return nil
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if provisioned {
		// Restore OpenTofu work dirs from the remote state blob if they no longer
		// exist locally (e.g. teardown running on a different CI runner than provision).
		// With --force, always restore from the blob to bypass stale local state.
		if state.TofuState != "" {
			if err := restoreTofuWorkDirs(state, teardownForce); err != nil {
				return fmt.Errorf("restore tofu state: %w", err)
			}
		}

		// Tear down in reverse provisioning order: driver first, then target.
		// Treat failures as fatal. Co-located scenarios (e.g. deployments/rds/postgres
		// handing its subnet_id to deployments/ec2/loaddriver_gotpc) run the driver
		// inside the target's own VPC, so tearing down the target's network while the
		// driver instance and its public IP are still attached fails with an EC2
		// DependencyViolation on the Internet Gateway or subnet. That leaves the VPC
		// half-destroyed and harder to clean up than stopping here.
		if len(state.DriverOutputs) > 0 {
			driver, err := buildDriverProvider(appCfg, state.DriverProvider)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "==> Tearing down driver [%s]\n", state.DriverProvider)
			if err := driver.Teardown(ctx, engine.Outputs(state.DriverOutputs)); err != nil {
				return fmt.Errorf("driver teardown: %w", err)
			}
		}

		target, err := buildTargetProvider(appCfg, state.TargetProvider)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "==> Tearing down target [%s]\n", state.TargetProvider)
		if err := target.Teardown(ctx, engine.Outputs(state.TargetOutputs)); err != nil {
			return fmt.Errorf("target teardown: %w", err)
		}
	}

	// Best-effort sweep of any local tofu state left behind. Providers normally
	// remove their own workdir on a successful Teardown, or on provision's own
	// rollback; this catches what they missed, and for a run whose provision
	// never produced outputs it is the only cleanup path.
	runDir := tofustate.RunDir(runID)
	if err := os.RemoveAll(runDir); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "warning: remove local tofu state %s: %v\n", runDir, err)
	}

	now := time.Now().UTC()
	if err := store.Update(runID, func(st *runstate.State) {
		st.Phases[runstate.PhaseTeardown] = runstate.StatusCompleted
		st.TerminatedAt = &now
		// Set CompletedAt only if not already recorded (e.g. RunAsync that failed
		// mid-provision before the driver had a chance to write it).
		if st.CompletedAt == nil {
			st.CompletedAt = &now
		}
		// Drop the blob to stop the run row from growing forever. It serves
		// only to restore a work dir before tearing down infrastructure that
		// still exists, and teardown has already succeeded by this point.
		st.TofuState = ""
	}); err != nil {
		return err
	}

	if teardownPurge {
		purger, ok := store.(runstate.Purger)
		if !ok {
			fmt.Fprintf(os.Stderr, "warning: --purge ignored: the store cannot delete records\n")
			return nil
		}
		if err := purger.Delete(runID); err != nil {
			return fmt.Errorf("purge: %w", err)
		}
		fmt.Fprintf(os.Stderr, "deleted %s\n", runID)
	}
	return nil
}
