package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/dbarena/benchctl/internal/engine"
	"github.com/dbarena/benchctl/internal/providers/local"
	"github.com/dbarena/benchctl/internal/schema"
)

var resumeScenarioPath string

var resumeCmd = &cobra.Command{
	Use:   "resume <run-id>",
	Short: "Resume a run from its first pending phase (runs on the driver instance)",
	Long: `resume loads run state from the store, then runs every pending phase in
order: driver.setup → workload.execute → driver.collect.

The orchestrator always runs teardown, never resume.

Use --scenario to override the path stored in run state. A remote driver needs
it, because the orchestrator's local path does not exist there.`,
	Args: cobra.ExactArgs(1),
	RunE: runResume,
}

func init() {
	resumeCmd.Flags().StringVar(&resumeScenarioPath, "scenario", "",
		"Path to the scenario YAML file (overrides the path stored in run state)")
}

func runResume(_ *cobra.Command, args []string) error {
	runID := args[0]

	store, err := openLocalStore()
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}

	state, err := store.Load(runID)
	if err != nil {
		return err
	}

	if state.IsTerminal() {
		return fmt.Errorf("run %s is already in a terminal state (completed or failed)", runID)
	}

	scenarioFile := resumeScenarioPath
	if scenarioFile == "" {
		scenarioFile = state.ScenarioPath
	}
	if scenarioFile == "" {
		return fmt.Errorf("no scenario path available; pass --scenario <path>")
	}

	s, err := schema.Load(scenarioFile)
	if err != nil {
		return fmt.Errorf("load scenario: %w", err)
	}
	if err := s.Validate(); err != nil {
		return fmt.Errorf("invalid scenario: %w", err)
	}

	runner, err := buildResumeRunner(s, state.Inputs)
	if err != nil {
		return err
	}
	runner.Store = store
	runner.ScenarioPath = scenarioFile

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	return runner.Resume(ctx, s, state)
}

// buildResumeRunner builds a Runner for the resume path. The driver is always
// "local", whatever the scenario declares: resume executes on the driver
// instance itself, so the workload is a local process.
func buildResumeRunner(s *schema.Scenario, inputs schema.ResolvedInputs) (engine.Runner, error) {
	targetProvider, err := resolveProvider(s.Target.Provider, inputs)
	if err != nil {
		return engine.Runner{}, err
	}
	collectorProvider, err := resolveProvider(s.Collector.Provider, inputs)
	if err != nil {
		return engine.Runner{}, err
	}

	target, err := buildTargetProvider(appCfg, targetProvider)
	if err != nil {
		return engine.Runner{}, err
	}

	workloads, err := buildWorkloadAdapters(s.Suite.Benchmarks)
	if err != nil {
		return engine.Runner{}, err
	}

	collector, err := buildCollector(appCfg, collectorProvider)
	if err != nil {
		return engine.Runner{}, err
	}

	return engine.Runner{
		Target:    target,
		Driver:    local.New(), // always local: this process runs on the driver instance
		Workloads: workloads,
		Collector: collector,
		Out:       os.Stderr,
	}, nil
}
