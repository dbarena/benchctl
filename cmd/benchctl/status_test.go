package main

import (
	"testing"
	"time"

	"github.com/dbarena/benchctl/internal/engine"
	"github.com/dbarena/benchctl/internal/runstate"
)

func TestOverallStatus(t *testing.T) {
	now := time.Now().UTC()

	tests := []struct {
		name string
		st   *runstate.State
		want string
	}{
		{
			name: "pending - no phases running",
			st: &runstate.State{
				Phases: map[runstate.Phase]runstate.Status{},
			},
			want: "pending",
		},
		{
			name: "running provision",
			st: &runstate.State{
				Phases: map[runstate.Phase]runstate.Status{
					runstate.PhaseProvision: runstate.StatusRunning,
				},
			},
			want: "running (provision)",
		},
		{
			name: "completed",
			st: &runstate.State{
				CompletedAt: &now,
				Phases:      map[runstate.Phase]runstate.Status{},
			},
			want: "completed",
		},
		{
			name: "completed and terminated - status is still completed",
			st: &runstate.State{
				CompletedAt:  &now,
				TerminatedAt: &now,
				Phases:       map[runstate.Phase]runstate.Status{},
			},
			want: "completed",
		},
		{
			name: "failed with error string",
			st: &runstate.State{
				Error:  "something exploded",
				Phases: map[runstate.Phase]runstate.Status{},
			},
			want: "failed",
		},
		{
			// LastHeartbeat is only written while a workload step runs, so a
			// stale heartbeat must not flag an unrelated running phase.
			name: "running driver.collect with old heartbeat is not flagged stale",
			st: &runstate.State{
				Phases: map[runstate.Phase]runstate.Status{
					runstate.PhaseDriverCollect: runstate.StatusRunning,
				},
				LastHeartbeat: func() *time.Time { t := now.Add(-30 * time.Minute); return &t }(),
			},
			want: "running (driver.collect)",
		},
		{
			name: "running workload.execute with old heartbeat is flagged stale",
			st: &runstate.State{
				Phases: map[runstate.Phase]runstate.Status{
					runstate.PhaseWorkloadExecute: runstate.StatusRunning,
				},
				LastHeartbeat: func() *time.Time { t := now.Add(-30 * time.Minute); return &t }(),
			},
			want: "running (workload.execute) [stale]",
		},
		{
			name: "running workload.execute with fresh heartbeat is not flagged stale",
			st: &runstate.State{
				Phases: map[runstate.Phase]runstate.Status{
					runstate.PhaseWorkloadExecute: runstate.StatusRunning,
				},
				LastHeartbeat: &now,
			},
			want: "running (workload.execute)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := overallStatus(tc.st)
			if got != tc.want {
				t.Errorf("overallStatus() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEnvironmentStatus(t *testing.T) {
	now := time.Now().UTC()

	tests := []struct {
		name string
		st   *runstate.State
		want string
	}{
		{
			name: "no provision yet",
			st:   &runstate.State{},
			want: "-",
		},
		{
			name: "provision completed, infra running",
			st: &runstate.State{
				TargetOutputs: map[string]string{"host": "localhost", "port": "5432"},
			},
			want: "running",
		},
		{
			name: "terminated",
			st: &runstate.State{
				TargetOutputs: map[string]string{"host": "localhost"},
				TerminatedAt:  &now,
			},
			want: "terminated",
		},
		{
			name: "terminated takes precedence over running target outputs",
			st: &runstate.State{
				TargetOutputs: map[string]string{"host": "localhost"},
				TerminatedAt:  &now,
			},
			want: "terminated",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := environmentStatus(tc.st)
			if got != tc.want {
				t.Errorf("environmentStatus() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTofuStateShort(t *testing.T) {
	tests := []struct {
		name string
		st   *runstate.State
		want string
	}{
		{
			name: "no tofu providers",
			st:   &runstate.State{},
			want: "-",
		},
		{
			name: "blob stored remotely",
			st:   &runstate.State{TofuState: "blob"},
			want: "remote",
		},
		{
			name: "local work dir in target outputs",
			st: &runstate.State{
				TargetOutputs: map[string]string{engine.OutputKeyTofuWorkDir: "/tmp/benchctl-tofu-abc"},
			},
			want: "local",
		},
		{
			name: "local work dir in driver outputs",
			st: &runstate.State{
				DriverOutputs: map[string]string{engine.OutputKeyTofuWorkDir: "/tmp/benchctl-tofu-xyz"},
			},
			want: "local",
		},
		{
			name: "remote takes precedence over any local outputs",
			st: &runstate.State{
				TofuState:     "blob",
				TargetOutputs: map[string]string{engine.OutputKeyTofuWorkDir: "/tmp/benchctl-tofu-abc"},
			},
			want: "remote",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tofuStateShort(tc.st)
			if got != tc.want {
				t.Errorf("tofuStateShort() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestShowsByDefault(t *testing.T) {
	now := time.Now().UTC()

	tests := []struct {
		name string
		st   *runstate.State
		want bool
	}{
		{
			name: "not terminal - still running",
			st:   &runstate.State{},
			want: true,
		},
		{
			name: "terminal but environment still running",
			st: &runstate.State{
				CompletedAt:   &now,
				TargetOutputs: map[string]string{"host": "localhost"},
			},
			want: true,
		},
		{
			name: "terminal and environment terminated",
			st: &runstate.State{
				CompletedAt:   &now,
				TargetOutputs: map[string]string{"host": "localhost"},
				TerminatedAt:  &now,
			},
			want: false,
		},
		{
			name: "terminal and never provisioned",
			st: &runstate.State{
				CompletedAt: &now,
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := showsByDefault(tc.st)
			if got != tc.want {
				t.Errorf("showsByDefault() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBuildCreatedByResolver(t *testing.T) {
	tests := []struct {
		name string
		st   *runstate.State
		want string
	}{
		{
			name: "persisted email is used verbatim",
			st:   &runstate.State{CreatedBy: "9cb959e9-5e7d-4970-bb17-6c1ef789cbcd", CreatedByEmail: "dev@example.com"},
			want: "dev@example.com",
		},
		{
			name: "no created_by at all",
			st:   &runstate.State{},
			want: "-",
		},
		{
			name: "no persisted email - unresolvable UUID passes through",
			st:   &runstate.State{CreatedBy: "9cb959e9-5e7d-4970-bb17-6c1ef789cbcd"},
			want: "9cb959e9-5e7d-4970-bb17-6c1ef789cbcd",
		},
		{
			name: "no persisted email - CI identity passes through",
			st:   &runstate.State{CreatedBy: "github:danielmitterdorfer"},
			want: "github:danielmitterdorfer",
		},
	}

	resolve := buildCreatedByResolver()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolve(tc.st)
			if got != tc.want {
				t.Errorf("resolve() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTofuStateDesc(t *testing.T) {
	tests := []struct {
		name string
		st   *runstate.State
		want string
	}{
		{
			name: "no tofu providers",
			st:   &runstate.State{},
			want: "",
		},
		{
			name: "blob stored in the run record",
			st:   &runstate.State{TofuState: "blob"},
			want: "stored (in the run record)",
		},
		{
			name: "target work dir only",
			st: &runstate.State{
				TargetOutputs: map[string]string{engine.OutputKeyTofuWorkDir: "/tmp/tofu-target"},
			},
			want: "local (target: /tmp/tofu-target)",
		},
		{
			name: "driver work dir only",
			st: &runstate.State{
				DriverOutputs: map[string]string{engine.OutputKeyTofuWorkDir: "/tmp/tofu-driver"},
			},
			want: "local (driver: /tmp/tofu-driver)",
		},
		{
			name: "both target and driver work dirs",
			st: &runstate.State{
				TargetOutputs: map[string]string{engine.OutputKeyTofuWorkDir: "/tmp/tofu-target"},
				DriverOutputs: map[string]string{engine.OutputKeyTofuWorkDir: "/tmp/tofu-driver"},
			},
			want: "local (target: /tmp/tofu-target, driver: /tmp/tofu-driver)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tofuStateDesc(tc.st)
			if got != tc.want {
				t.Errorf("tofuStateDesc() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStaleSuffix(t *testing.T) {
	refreshed := time.Now().Add(-90 * time.Second)
	tests := []struct {
		name string
		st   *runstate.State
		want string
	}{
		{
			name: "current snapshot",
			st:   &runstate.State{RefreshedAt: refreshed},
			want: "",
		},
		{
			name: "refresh failed, earlier snapshot available",
			st:   &runstate.State{RefreshedAt: refreshed, RefreshError: "connection refused"},
			want: " [stale, 1m30s old]",
		},
		{
			name: "never reached the driver instance",
			st:   &runstate.State{RefreshError: "connection refused"},
			want: " [unreachable]",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := staleSuffix(tc.st); got != tc.want {
				t.Errorf("staleSuffix() = %q, want %q", got, tc.want)
			}
		})
	}
}
