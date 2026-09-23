package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dbarena/benchctl/internal/schema"
)

var validateCmd = &cobra.Command{
	Use:   "validate <scenario-file>",
	Short: "Validate a scenario file without running it",
	Args:  cobra.ExactArgs(1),
	RunE:  validateScenario,
}

func validateScenario(_ *cobra.Command, args []string) error {
	s, err := schema.Load(args[0])
	if err != nil {
		return err
	}
	if err := s.Validate(); err != nil {
		return fmt.Errorf("invalid: %w", err)
	}
	fmt.Printf("OK: %s\n", s.Metadata.Name)
	return nil
}
