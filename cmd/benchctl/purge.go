package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dbarena/benchctl/internal/runstate"
)

var (
	purgeAll    bool
	purgeStatus string
)

var purgeCmd = &cobra.Command{
	Use:   "purge [run-id]",
	Short: "Hard-delete run records from the store",
	Long: `purge hard-deletes run records from the store. It removes metadata only;
tear infrastructure down with 'benchctl teardown'.

A service role key (BENCHCTL_STORE_SERVICE_ROLE_KEY) bypasses RLS on a Supabase
store, so it can delete any run. A user JWT can delete only the caller's own
runs (created_by = auth.uid()).`,
	Args: cobra.RangeArgs(0, 1),
	RunE: runPurge,
}

func init() {
	purgeCmd.Flags().BoolVar(&purgeAll, "all", false, "Delete all matching runs (requires confirmation)")
	purgeCmd.Flags().StringVar(&purgeStatus, "status", "", "Filter by overall status when using --all: pending, running, completed, failed")
}

func runPurge(_ *cobra.Command, args []string) error {
	if len(args) == 0 && !purgeAll {
		return fmt.Errorf("specify a run-id or pass --all")
	}
	if len(args) == 1 && purgeAll {
		return fmt.Errorf("cannot pass both a run-id and --all")
	}

	store, err := openStore()
	if err != nil {
		return err
	}
	purger, ok := store.(runstate.Purger)
	if !ok {
		return fmt.Errorf("the configured store cannot delete records")
	}

	if len(args) == 1 {
		if err := purger.Delete(args[0]); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "deleted %s\n", args[0])
		return nil
	}

	// --all: list, optionally filter by status, confirm, then delete.
	states, err := store.List()
	if err != nil {
		return err
	}
	if purgeStatus != "" {
		var filtered []*runstate.State
		for _, st := range states {
			if overallStatus(st) == purgeStatus {
				filtered = append(filtered, st)
			}
		}
		states = filtered
	}

	if len(states) == 0 {
		fmt.Fprintln(os.Stderr, "no matching runs to delete")
		return nil
	}

	noun := "runs"
	if purgeStatus != "" {
		noun = purgeStatus + " runs"
	}
	fmt.Fprintf(os.Stderr, "This permanently deletes %d %s. Type \"yes\" to confirm: ", len(states), noun)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	if strings.TrimSpace(scanner.Text()) != "yes" {
		fmt.Fprintln(os.Stderr, "aborted")
		return nil
	}

	var failures []string
	for _, st := range states {
		if err := purger.Delete(st.RunID); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", st.RunID, err))
		} else {
			fmt.Fprintf(os.Stderr, "deleted %s\n", st.RunID)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("some deletes failed:\n  %s", strings.Join(failures, "\n  "))
	}
	return nil
}
