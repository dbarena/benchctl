package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

var providersCmd = &cobra.Command{
	Use:   "providers",
	Short: "Inspect available providers and adapters",
}

var providersListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the built-in providers and adapters",
	RunE:  listProviders,
}

func init() {
	providersCmd.AddCommand(providersListCmd)
}

func listProviders(_ *cobra.Command, _ []string) error {
	fmt.Println("Target providers:  local (alias: noop), docker-compose, opentofu, supabase")
	fmt.Println("Driver providers:  local, ec2, gce")
	fmt.Println("Workload adapters: go-tpc, k6, shell")
	fmt.Println("Collectors:        stdout, victoriametrics")
	return nil
}
