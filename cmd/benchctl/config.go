package main

import (
	"fmt"
	"sort"
	"strings"

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
	Short: "Set a value in the user configuration",
	Long: `set writes a single key to the user configuration (~/.benchctl/config.yaml).
It overrides the shared and default configuration for that key.

Valid keys: ` + strings.Join(config.FileKeys(), ", ") + `, and tags.<name>.`,
	Args: cobra.ExactArgs(2),
	RunE: runConfigSet,
}

var configUnsetCmd = &cobra.Command{
	Use:   "unset <key>",
	Short: "Remove a value from the user configuration",
	Long: `unset removes a single key from the user configuration, so it falls back to
the shared configuration (if any) or the default configuration. Valid keys
are the same as for set.`,
	Args: cobra.ExactArgs(1),
	RunE: runConfigUnset,
}

var configUseSharedCmd = &cobra.Command{
	Use:   "use-shared [key...]",
	Short: "Remove user configuration values that override the shared configuration",
	Long: `use-shared removes the user configuration values that override a value in the
shared configuration, so the shared value is used instead. Without arguments it
covers every overriden key, as listed by "benchctl config show". Keys that the shared configuration 
does not set are left alone.

It only edits the user configuration file. Any BENCHCTL_* environment variables are untouched.`,
	RunE: runConfigUseShared,
}

func init() {
	configCmd.AddCommand(configShowCmd)
	configCmd.AddCommand(configSetCmd)
	configCmd.AddCommand(configUnsetCmd)
	configCmd.AddCommand(configUseSharedCmd)
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

	fmt.Println("User configuration:")
	fmt.Printf("  %s\n", path)
	if sp := config.SharedConfigPath(); sp != "" {
		fmt.Println("Shared configuration (read-only):")
		fmt.Printf("  %s\n", sp)
	}

	fmt.Println()
	fmt.Println("Effective settings:")
	printSetting := func(indent, label, dottedKey string) {
		value := cfg.Value(dottedKey)
		if value == "" {
			fmt.Printf("%s%s: (unset)\n", indent, label)
			return
		}
		fmt.Printf("%s%s: %s (%s)\n", indent, label, value, cfg.Source(dottedKey))
	}
	for _, key := range config.FileKeys() {
		printSetting("  ", key, key)
	}

	if len(cfg.Tags) == 0 {
		fmt.Println("  tags: (unset)")
	} else {
		fmt.Println("  tags:")
		names := make([]string, 0, len(cfg.Tags))
		for name := range cfg.Tags {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			printSetting("    ", name, "tags."+name)
		}
	}

	if overrides := cfg.UserOverrideKeys(); len(overrides) > 0 {
		fmt.Println()
		fmt.Println("Your user configuration overrides the shared configuration for these keys:")
		for _, key := range overrides {
			fmt.Printf("  %s\n", key)
		}
		fmt.Println("\nTo use the shared configuration for all of them, run:")
		fmt.Println("  benchctl config use-shared")
	}

	if keys := cfg.EnvOverrideKeys(); len(keys) > 0 {
		fmt.Println()
		fmt.Println("Your environment overrides the configuration files for these keys:")
		for _, key := range keys {
			fmt.Printf("  %s (%s)\n", key, config.EnvVar(key))
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

func runConfigUnset(_ *cobra.Command, args []string) error {
	key := args[0]
	removed, err := config.Unset(key)
	if err != nil {
		return err
	}
	if removed {
		fmt.Printf("Unset %s.\n", key)
	} else {
		fmt.Printf("%s was not set in the user configuration.\n", key)
	}
	return nil
}

// runConfigUseShared removes the user configuration values that override the
// shared configuration: every such key, or just the ones named in args.
func runConfigUseShared(_ *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	keys := cfg.UserOverrideKeys()
	if len(args) > 0 {
		keys = keys[:0:0]
		for _, key := range args {
			if cfg.UserOverridesShared(key) {
				keys = append(keys, key)
				continue
			}
			fmt.Printf("%s does not override the shared configuration; left as is.\n", key)
		}
	}
	if len(keys) == 0 {
		if len(args) == 0 {
			fmt.Println("No keys in the user configuration override the shared configuration.")
		}
		return nil
	}
	for _, key := range keys {
		if _, err := config.Unset(key); err != nil {
			return err
		}
		fmt.Printf("Unset %s.\n", key)
	}
	// Environment variables are not ours to unset
	for _, key := range keys {
		if cfg.Source(key) == config.SourceEnv {
			fmt.Printf("%s is set, so %s keeps its environment value until you unset it.\n", config.EnvVar(key), key)
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
