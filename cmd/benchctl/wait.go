package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/dbarena/benchctl/internal/runstate"
)

var waitCmd = &cobra.Command{
	Use:   "wait <run-id>",
	Short: "Block until a benchmark run reaches a terminal state",
	Args:  cobra.ExactArgs(1),
	RunE:  runWait,
}

// maxConsecutiveLoadErrors is the number of consecutive store.Load failures wait
// tolerates before giving up. A connection reset or a temporary DNS failure must
// not abort a poll that has already run for hours.
const maxConsecutiveLoadErrors = 5

// waitPollInterval is how often runWait re-polls the store between checks.
const waitPollInterval = 2 * time.Second

func runWait(_ *cobra.Command, args []string) error {
	runID := args[0]

	store, err := openStore()
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	return pollRunState(ctx, store, runID, waitPollInterval, os.Stderr)
}

// pollRunState blocks until the run reaches a terminal state, the context is
// cancelled, or the store reports a definitive (non-transient) failure.
//
// It treats a Load error wrapping runstate.ErrRunNotFound as definitive and
// returns it at once: Create() completes synchronously before a caller ever
// holds a run ID, so the record exists by the time this loop starts.
//
// It treats any other Load error as transient and retries up to
// maxConsecutiveLoadErrors times before giving up.
func pollRunState(ctx context.Context, store runstate.Store, runID string, pollInterval time.Duration, out io.Writer) error {
	var consecutiveErrors int
	for {
		state, err := store.Load(runID)
		if err != nil {
			if errors.Is(err, runstate.ErrRunNotFound) {
				return fmt.Errorf("run %s: no state record in the store (it may have been deleted): %w", runID, err)
			}
			consecutiveErrors++
			if consecutiveErrors >= maxConsecutiveLoadErrors {
				return err
			}
			fmt.Fprintf(out, "warning: transient error loading run state (%d/%d): %v\n",
				consecutiveErrors, maxConsecutiveLoadErrors, err)
		} else {
			consecutiveErrors = 0
			if state.IsTerminal() {
				if state.Error != "" {
					return fmt.Errorf("run %s failed: %s", runID, state.Error)
				}
				fmt.Fprintf(out, "run %s completed successfully\n", runID)
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}
