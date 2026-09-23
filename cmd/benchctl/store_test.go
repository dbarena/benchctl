package main

import (
	"strings"
	"testing"

	"github.com/dbarena/benchctl/internal/config"
	"github.com/dbarena/benchctl/internal/runstate"
)

// TestDriverDialer_EC2 checks the seam between the store and the driver
// providers: a run recorded with an ec2 driver has to yield a usable SSH argv
// and the path benchctl was installed at, both taken from the run's own
// outputs.
func TestDriverDialer_EC2(t *testing.T) {
	appCfg = &config.Config{Store: config.StoreConfig{Mode: config.ModeLocal}}

	state := &runstate.State{
		RunID:          "run-a",
		DriverProvider: "ec2",
		DriverOutputs: map[string]string{
			"public_ip":            "1.2.3.4",
			"ssh_private_key_path": "/tmp/key.pem",
			"_tofu_work_dir":       "/tmp/wd",
		},
	}

	access, err := driverDialer(state)
	if err != nil {
		t.Fatalf("driverDialer: %v", err)
	}
	argv := strings.Join(access.SSHArgs, " ")
	if !strings.HasPrefix(argv, "ssh ") {
		t.Errorf("argv = %q, want an ssh invocation", argv)
	}
	if !strings.Contains(argv, "1.2.3.4") {
		t.Errorf("argv = %q, should target the driver's public ip", argv)
	}
	if !strings.Contains(argv, "/tmp/key.pem") {
		t.Errorf("argv = %q, should use the run's ssh key", argv)
	}
	if access.BenchctlPath == "" {
		t.Error("BenchctlPath should say where Bootstrap installed benchctl")
	}
}

// TestDriverDialer_LocalDriverHasNowhereToRead: a run whose workload executes
// in this process has no separate machine to read state back from.
func TestDriverDialer_LocalDriverHasNowhereToRead(t *testing.T) {
	appCfg = &config.Config{Store: config.StoreConfig{Mode: config.ModeLocal}}

	state := &runstate.State{
		RunID:          "run-a",
		DriverProvider: "local",
		DriverOutputs:  map[string]string{"host": "localhost"},
	}

	if _, err := driverDialer(state); err == nil {
		t.Error("expected an error for a driver that does not run benchctl elsewhere")
	}
}

func TestDriverDialer_NoDriverOutputs(t *testing.T) {
	appCfg = &config.Config{Store: config.StoreConfig{Mode: config.ModeLocal}}

	if _, err := driverDialer(&runstate.State{RunID: "run-a", DriverProvider: "ec2"}); err == nil {
		t.Error("expected an error when the driver was never provisioned")
	}
}
