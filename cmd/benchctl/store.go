package main

import (
	"fmt"

	"github.com/dbarena/benchctl/internal/engine"
	"github.com/dbarena/benchctl/internal/runstate"
)

// openStore builds the run-state store for this invocation.
func openStore() (runstate.Store, error) {
	return runstate.NewStore(appCfg, driverDialer)
}

// openLocalStore builds a store that reads only what is on this machine.
//
// Two callers need that. A read-through store invokes the `state` commands over
// SSH, so relaying from there would send a machine straight back to itself. And
// `benchctl resume` runs on the driver instance, where the local record is the
// authoritative one.
func openLocalStore() (runstate.Store, error) {
	return runstate.NewStore(appCfg, nil)
}

// driverDialer implements runstate.Dialer: given a run, it works out how to
// reach the driver instance executing it and where benchctl lives there.
//
// It resolves the provider from the run's own record the same way `benchctl
// connect` does, so the SSH user, key, and host-key handling all come from that
// driver. There is no second SSH configuration to keep in step.
func driverDialer(state *runstate.State) (runstate.RemoteAccess, error) {
	if len(state.DriverOutputs) == 0 {
		return runstate.RemoteAccess{}, fmt.Errorf("run %s: driver was not provisioned", state.RunID)
	}
	provider, err := buildDriverProvider(appCfg, state.DriverProvider)
	if err != nil {
		return runstate.RemoteAccess{}, err
	}
	host, ok := provider.(engine.BenchctlHost)
	if !ok {
		return runstate.RemoteAccess{}, fmt.Errorf("driver provider %q does not run benchctl on a separate machine", state.DriverProvider)
	}
	connector, ok := provider.(engine.Connector)
	if !ok {
		return runstate.RemoteAccess{}, fmt.Errorf("driver provider %q does not support SSH access", state.DriverProvider)
	}
	argv, err := connector.ConnectArgs(engine.Outputs(state.DriverOutputs))
	if err != nil {
		return runstate.RemoteAccess{}, err
	}
	return runstate.RemoteAccess{SSHArgs: argv, BenchctlPath: host.RemoteBenchctlPath()}, nil
}
