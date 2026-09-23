package main

import (
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"github.com/dbarena/benchctl/internal/config"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage benchctl configuration",
}

var configShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show the config file location and effective settings",
	Args:  cobra.NoArgs,
	RunE:  runConfigShow,
}

var configSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set a config value",
	Long: `set writes a single key to the config file.

Valid keys: store.mode, store.url, store.anon_key, store.refresh_interval,
metrics.endpoint, metrics.username, username, and tags.<name>.`,
	Args: cobra.ExactArgs(2),
	RunE: runConfigSet,
}

func init() {
	configCmd.AddCommand(configShowCmd)
	configCmd.AddCommand(configSetCmd)
}

// runConfigShow skips cfg.Validate: a diagnostic command has to keep working on
// an incomplete configuration.
func runConfigShow(_ *cobra.Command, _ []string) error {
	path, err := config.Path()
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	fmt.Println("Config file:")
	fmt.Printf("  %s\n", path)

	fmt.Println()
	fmt.Println("Effective settings:")
	printSetting := func(label, dottedKey, value string) {
		if value == "" {
			fmt.Printf("  %s: (unset)\n", label)
			return
		}
		fmt.Printf("  %s: %s (%s)\n", label, value, cfg.Source(dottedKey))
	}
	printSetting("store.mode", "store.mode", string(cfg.Store.Mode))
	printSetting("store.url", "store.url", cfg.Store.URL)
	printSetting("store.anon_key", "store.anon_key", cfg.Store.AnonKey)
	printSetting("store.refresh_interval", "store.refresh_interval", cfg.Store.RefreshInterval)
	printSetting("metrics.endpoint", "metrics.endpoint", cfg.Metrics.Endpoint)
	printSetting("metrics.username", "metrics.username", cfg.Metrics.Username)
	printSetting("username", "username", cfg.Username)

	if len(cfg.Tags) == 0 {
		fmt.Println("  tags: (none)")
	} else {
		fmt.Println("  tags:")
		names := make([]string, 0, len(cfg.Tags))
		for name := range cfg.Tags {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Printf("    %s: %s\n", name, cfg.Tags[name])
		}
	}

	fmt.Println()
	fmt.Println("Environment:")
	for _, es := range config.EnvStates() {
		switch {
		case !es.Set:
			fmt.Printf("  %s: (unset)\n", es.Name)
		case es.Secret:
			fmt.Printf("  %s: [redacted]\n", es.Name)
		case es.Value == "":
			fmt.Printf("  %s: (empty)\n", es.Name)
		default:
			fmt.Printf("  %s: %s\n", es.Name, es.Value)
		}
	}

	return nil
}

func runConfigSet(_ *cobra.Command, args []string) error {
	key, value := args[0], args[1]
	if err := config.Set(key, value); err != nil {
		return err
	}
	fmt.Printf("Set %s.\n", key)
	return nil
}
