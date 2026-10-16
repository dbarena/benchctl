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
	Short: "Show the configuration files and effective settings",
	Args:  cobra.NoArgs,
	RunE:  runConfigShow,
}

var configSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set a value in the home configuration",
	Long: `set writes a single key to the home configuration (~/.benchctl/config.yaml).
It overrides the shared and default configuration for that key.

Valid keys: store.mode, store.url, store.anon_key, store.refresh_interval,
metrics.endpoint, metrics.username, username, and tags.<name>.`,
	Args: cobra.ExactArgs(2),
	RunE: runConfigSet,
}

var configUnsetOverridesShared bool

var configUnsetCmd = &cobra.Command{
	Use:   "unset <key> | --overrides-shared",
	Short: "Remove a value from the home configuration",
	Long: `unset removes a single key from the home configuration, so it falls back to
the shared configuration (if any) or the default configuration. Valid keys
are the same as for set.

With --overrides-shared, unset removes every key that "benchctl config show"
marks as overriding the shared configuration.`,
	Args: func(cmd *cobra.Command, args []string) error {
		if configUnsetOverridesShared {
			return cobra.NoArgs(cmd, args)
		}
		return cobra.ExactArgs(1)(cmd, args)
	},
	RunE: runConfigUnset,
}

func init() {
	configCmd.AddCommand(configShowCmd)
	configCmd.AddCommand(configSetCmd)
	configCmd.AddCommand(configUnsetCmd)
	configUnsetCmd.Flags().BoolVar(&configUnsetOverridesShared, "overrides-shared", false, "Remove every key that overrides the shared configuration")
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

	fmt.Println("Home configuration:")
	fmt.Printf("  %s\n", path)
	if sp := config.SharedConfigPath(); sp != "" {
		fmt.Println("Shared configuration (read-only):")
		fmt.Printf("  %s\n", sp)
	}

	fmt.Println()
	fmt.Println("Effective settings:")
	var overrides []string
	printSetting := func(label, dottedKey, value string) {
		if value == "" {
			fmt.Printf("  %s: (unset)\n", label)
			return
		}
		if cfg.OverridesShared(dottedKey) {
			overrides = append(overrides, dottedKey)
			fmt.Printf("  %s: %s (%s, overrides shared configuration)\n", label, value, cfg.Source(dottedKey))
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

	if len(overrides) > 0 {
		fmt.Println()
		fmt.Println("Your home configuration overrides the shared configuration for these keys:")
		for _, key := range overrides {
			fmt.Printf("  %s\n", key)
		}
		fmt.Println("\nTo use the shared configuration for all of them, run:")
		fmt.Println("  benchctl config unset --overrides-shared")
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

func runConfigUnset(_ *cobra.Command, args []string) error {
	if configUnsetOverridesShared {
		return unsetOverridesShared()
	}
	key := args[0]
	removed, err := config.Unset(key)
	if err != nil {
		return err
	}
	if removed {
		fmt.Printf("Unset %s.\n", key)
	} else {
		fmt.Printf("%s was not set in the home configuration.\n", key)
	}
	return nil
}

// unsetOverridesShared removes every home configuration key that overrides
// the shared configuration.
func unsetOverridesShared() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	keys := cfg.KeysOverridingShared()
	if len(keys) == 0 {
		fmt.Println("No keys in the home configuration override the shared configuration.")
		return nil
	}
	for _, key := range keys {
		if _, err := config.Unset(key); err != nil {
			return err
		}
		fmt.Printf("Unset %s.\n", key)
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
