// Command benchctl orchestrates benchmark runs from a scenario file.
// It provisions the target and driver infrastructure, runs the workload,
// collects the metrics, and tracks the state of every run.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/dbarena/benchctl/internal/buildinfo"
	"github.com/dbarena/benchctl/internal/config"
)

// appCfg is the parsed user configuration, loaded once by rootCmd's
// PersistentPreRunE before any command runs.
var appCfg *config.Config

var rootCmd = &cobra.Command{
	Use:     "benchctl",
	Version: buildinfo.Version,
	Short:   "A pluggable benchmark orchestrator",
	Long:    `A pluggable benchmark orchestrator: it manages isolated benchmark environments in the cloud`,
	// Print usage for argument and flag mistakes only, never for runtime errors.
	SilenceUsage: true,
	// Cobra only runs the PersistentPreRunE closest to the invoked command,
	// so subcommands must not define their own or this load is skipped.
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		appCfg = cfg
		return nil
	},
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	rootCmd.SetVersionTemplate(fmt.Sprintf("benchctl version {{.Version}}\ncommit: %s\n", buildinfo.Commit))
	rootCmd.AddCommand(runCmd)
	rootCmd.AddCommand(resumeCmd)
	rootCmd.AddCommand(validateCmd)
	rootCmd.AddCommand(infoCmd)
	rootCmd.AddCommand(providersCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(waitCmd)
	rootCmd.AddCommand(teardownCmd)
	rootCmd.AddCommand(authCmd)
	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(purgeCmd)
	rootCmd.AddCommand(connectCmd)
	rootCmd.AddCommand(resultsCmd)
	rootCmd.AddCommand(fetchCmd)
	rootCmd.AddCommand(stateCmd)
}
