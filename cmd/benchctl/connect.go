package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/dbarena/benchctl/internal/engine"
)

var connectPrint bool

var connectCmd = &cobra.Command{
	Use:   "connect <run-id> driver|target",
	Short: "Open an interactive shell on a run's driver or target instance",
	Args:  cobra.ExactArgs(2),
	RunE:  runConnect,
}

func init() {
	connectCmd.Flags().BoolVar(&connectPrint, "print", false, "Print the connection command instead of executing it")
}

func runConnect(_ *cobra.Command, args []string) error {
	runID := args[0]
	which := args[1]
	if which != "driver" && which != "target" {
		return fmt.Errorf("second argument must be \"driver\" or \"target\"")
	}

	store, err := openStore()
	if err != nil {
		return err
	}
	state, err := store.Load(runID)
	if err != nil {
		return err
	}

	// Restore OpenTofu work dirs when the run was provisioned on a different
	// machine (e.g. a CI runner). Updates _tofu_work_dir and ssh_private_key_path
	// in state outputs before ConnectArgs reads them.
	if state.TofuState != "" {
		if err := restoreTofuWorkDirs(state, false); err != nil {
			return fmt.Errorf("restore tofu state: %w", err)
		}
	}

	var providerName string
	var outputs engine.Outputs
	switch which {
	case "driver":
		providerName = state.DriverProvider
		outputs = engine.Outputs(state.DriverOutputs)
	case "target":
		providerName = state.TargetProvider
		outputs = engine.Outputs(state.TargetOutputs)
	}

	if len(outputs) == 0 {
		return fmt.Errorf("run %s: %s was not provisioned", runID, which)
	}

	connector, err := resolveConnector(which, providerName)
	if err != nil {
		return err
	}

	argv, err := connector.ConnectArgs(outputs)
	if err != nil {
		return err
	}

	if connectPrint {
		fmt.Println(strings.Join(argv, " "))
		return nil
	}

	sshPath, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	return syscall.Exec(sshPath, argv, os.Environ())
}

func resolveConnector(which, providerName string) (engine.Connector, error) {
	var connector engine.Connector
	switch which {
	case "driver":
		p, err := buildDriverProvider(appCfg, providerName)
		if err != nil {
			return nil, err
		}
		c, ok := p.(engine.Connector)
		if !ok {
			return nil, fmt.Errorf("driver provider %q does not support connect", providerName)
		}
		connector = c
	case "target":
		p, err := buildTargetProvider(appCfg, providerName)
		if err != nil {
			return nil, err
		}
		c, ok := p.(engine.Connector)
		if !ok {
			return nil, fmt.Errorf("target provider %q does not support connect", providerName)
		}
		connector = c
	}
	return connector, nil
}
