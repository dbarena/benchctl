package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/dbarena/benchctl/internal/runstate"
)

// The state commands are the machine-to-machine interface to one benchctl
// installation's own run records. The orchestrator uses `state import` to seed a
// record onto a driver instance before handing a run off to it, and `state
// export` to read that instance's view of the run back. Separate commands keep
// the wire format free of `status -o json`'s presentation choices;
// `benchctl status <run-id> -o json` is the human-facing equivalent.
var stateCmd = &cobra.Command{
	Use:    "state",
	Short:  "Read and write run records in this machine's store",
	Hidden: true,
}

var stateExportCmd = &cobra.Command{
	Use:   "export <run-id>",
	Short: "Write a run record to stdout as JSON",
	Args:  cobra.ExactArgs(1),
	RunE:  runStateExport,
}

var stateImportCmd = &cobra.Command{
	Use:   "import <file>",
	Short: "Read a run record as JSON and write it to the store",
	Long: `import reads a run record as JSON and writes it to this machine's store,
replacing any existing record with the same run ID. Pass - to read stdin.`,
	Args: cobra.ExactArgs(1),
	RunE: runStateImport,
}

func init() {
	stateCmd.AddCommand(stateExportCmd)
	stateCmd.AddCommand(stateImportCmd)
}

func runStateExport(_ *cobra.Command, args []string) error {
	store, err := openLocalStore()
	if err != nil {
		return err
	}
	state, err := store.Load(args[0])
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(state)
}

func runStateImport(_ *cobra.Command, args []string) error {
	data, err := readFileOrStdin(args[0])
	if err != nil {
		return err
	}
	var state runstate.State
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("parse run record: %w", err)
	}
	if err := runstate.ValidateRunID(state.RunID); err != nil {
		return fmt.Errorf("invalid run record: %w", err)
	}

	store, err := openLocalStore()
	if err != nil {
		return err
	}
	// Replace rather than create: a retried bootstrap re-seeds the same
	// record, and the incoming seed is the more recent view at that point.
	_, err = store.Load(state.RunID)
	switch {
	case errors.Is(err, runstate.ErrRunNotFound):
		if err := store.Create(&state); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		if err := store.Update(state.RunID, func(st *runstate.State) { *st = state }); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "imported %s\n", state.RunID)
	return nil
}

func readFileOrStdin(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}
