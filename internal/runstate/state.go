// Package runstate defines the persistent record for a single benchmark run
// and the Store interface for reading and writing it.
package runstate

import (
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Phase names a lifecycle phase tracked in run state.
type Phase string

const (
	PhaseProvision       Phase = "provision"
	PhaseDriverSetup     Phase = "driver.setup"
	PhaseWorkloadExecute Phase = "workload.execute"
	PhaseDriverCollect   Phase = "driver.collect"
	PhaseTeardown        Phase = "teardown"
)

// AllPhases lists phases in execution order.
var AllPhases = []Phase{
	PhaseProvision,
	PhaseDriverSetup,
	PhaseWorkloadExecute,
	PhaseDriverCollect,
	PhaseTeardown,
}

// Status is the execution status of a lifecycle phase.
type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
)

// State is the persistent record for a single benchmark run.
// benchctl creates it at run start; the driver process updates it as async
// phases complete. Within the driver process, the main run goroutine and the
// heartbeat goroutine (see engine.Runner.startHeartbeat) both write to it
// concurrently; engine.Runner serializes those writes with a mutex.
type State struct {
	RunID          string            `json:"run_id"`
	ScenarioName   string            `json:"scenario_name"`
	ScenarioPath   string            `json:"scenario_path"`
	TargetProvider string            `json:"target_provider"`
	DriverProvider string            `json:"driver_provider"`
	StartedAt      time.Time         `json:"started_at"`
	Inputs         map[string]any    `json:"inputs"`
	Phases         map[Phase]Status  `json:"phases"`
	TargetOutputs  map[string]string `json:"target_outputs,omitempty"`
	DriverOutputs  map[string]string `json:"driver_outputs,omitempty"`
	CompletedAt    *time.Time        `json:"completed_at,omitempty"`
	// TerminatedAt is set by benchctl teardown after infrastructure is
	// successfully destroyed. Its presence signals "completed, infra torn down"
	// as opposed to "completed, infra still up".
	TerminatedAt *time.Time `json:"terminated_at,omitempty"`
	Error        string     `json:"error,omitempty"`
	// CreatedBy identifies the run initiator.
	// Human users: their Supabase auth UUID (from the JWT sub claim).
	// CI: "ci/nightly" or "github:{actor}".
	// LocalStore runs: empty (no auth layer).
	CreatedBy string `json:"created_by,omitempty"`
	// CreatedByEmail is the human-readable email for CreatedBy, captured from
	// the JWT email claim at run creation so `benchctl status` can display it
	// for any user (a UUID has no reverse lookup without admin API access).
	// Display-only; RLS and --mine filtering use CreatedBy, not this field.
	// Empty for CI runs and for runs created before this field existed.
	CreatedByEmail string `json:"created_by_email,omitempty"`
	// LastHeartbeat is written every ~30s by the driver during workload.execute.
	// benchctl status uses it to detect stale/orphaned runs.
	LastHeartbeat *time.Time `json:"last_heartbeat,omitempty"`
	// Metadata is a flat string map populated after collection. It merges
	// scenario.metadata.labels, workload.info results, and collector config
	// keys worth persisting (effective_date, endpoint). Later keys win.
	Metadata map[string]string `json:"metadata,omitempty"`
	// CurrentFixture, CurrentIteration, CurrentStep, and StepStartedAt
	// describe the suite step presently executing (or last executed) during
	// workload.execute. Unset outside that phase. Written by engine.Runner
	// as the fixture/iteration loop advances, so external tooling (e.g. a
	// pprof-capture script polling `benchctl status -o json`) can tell which
	// fixture is active and how long its current step has been running.
	CurrentFixture   map[string]string `json:"current_fixture,omitempty"`
	CurrentIteration int               `json:"current_iteration,omitempty"`
	CurrentStep      string            `json:"current_step,omitempty"`
	StepStartedAt    *time.Time        `json:"step_started_at,omitempty"`
	// TofuState is a base64-encoded tar+gzip archive of the OpenTofu working
	// directory, written after provision when a shared store is configured.
	// Consumed by teardown to restore the work dir on a different machine or
	// CI runner. Empty for local-store runs and non-opentofu providers.
	TofuState string `json:"tofu_state,omitempty"`

	// RefreshedAt and RefreshError describe how current this snapshot is.
	// LocalStore reads a run executing on a remote driver instance back from
	// that instance and caches it, so a record served from cache can lag, and
	// an unreachable instance leaves it stale indefinitely. Both fields are set
	// on read and never stored; json:"-" keeps them out of the persisted record
	// and out of `benchctl status -o json`, whose payload is the run itself.
	// Stores that always read live leave them empty.
	RefreshedAt  time.Time `json:"-"`
	RefreshError string    `json:"-"`
}

// IsStale reports whether this snapshot could not be refreshed from the
// machine executing the run.
func (s *State) IsStale() bool { return s.RefreshError != "" }

// Store persists and retrieves run state.
type Store interface {
	Create(state *State) error
	Update(runID string, fn func(*State)) error
	Load(runID string) (*State, error)
	List() ([]*State, error)
}

// ErrRunNotFound is returned by Store.Load (wrapped with the run ID for
// context) when no record exists for the given run ID. Callers that need to
// distinguish "the store authoritatively said this run doesn't exist" from a
// transient/connectivity error should check for it with errors.Is.
var ErrRunNotFound = errors.New("runstate: run not found")

// SharedStore is implemented by stores whose records other users and machines
// can read (e.g. Supabase). The runner uses it to enforce that providers whose
// state must be recoverable from another machine have a store that can be
// reached from one.
type SharedStore interface {
	IsShared() bool
}

// Purger is implemented by stores that support hard-deleting run records.
// Both LocalStore and SupabaseStore implement it. Deleting a record removes
// metadata only; infrastructure is torn down by `benchctl teardown`.
//
// With a service role key SupabaseStore's delete bypasses RLS, so any run can
// be removed, not just the caller's own.
type Purger interface {
	Delete(runID string) error
}

// Owner is implemented by stores that track which machine executes a run.
// A store whose records are shared does not need it: every machine reads the
// same row, so there is nothing to hand over.
type Owner interface {
	// Delegate marks the run as executed by another machine, so later reads
	// refresh from that machine instead of trusting the local copy.
	Delegate(runID string) error
	// Claim marks the run as executed by this machine.
	Claim(runID string) error
}

// ActiveLister is implemented by stores that can filter to active runs in the
// store itself rather than in the client. Used by `status` to avoid fetching
// full historical run data (which can include large blob fields like
// tofu_state/error) for runs that client-side filtering would discard anyway.
// Both LocalStore and SupabaseStore implement it; the two filters and
// showsByDefault in cmd/benchctl/status.go must agree.
type ActiveLister interface {
	ListActive() ([]*State, error)
}

// NewRunID generates a unique run identifier from the scenario name and
// current UTC time with a short random suffix to prevent collisions.
func NewRunID(scenarioName string) string {
	var b [3]byte
	rand.Read(b[:])
	return fmt.Sprintf("%s-%s-%x", scenarioName, time.Now().UTC().Format("20060102-150405"), b)
}

// runIDPattern restricts caller-supplied run IDs to characters safe for use as
// a LocalStore directory name and a PostgREST filter value: letters, digits,
// dash, underscore, and dot, with no path separators.
var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// ValidateRunID reports whether id is safe to use as a run ID. Callers use it
// when they supply their own ID (e.g. `benchctl run --async --run-id`) instead
// of letting NewRunID generate one.
func ValidateRunID(id string) error {
	if id == "" {
		return fmt.Errorf("run ID must not be empty")
	}
	if id == "." || id == ".." {
		return fmt.Errorf("run ID %q is not a valid identifier", id)
	}
	if !runIDPattern.MatchString(id) {
		return fmt.Errorf("run ID %q may contain only letters, digits, dash, underscore, and dot", id)
	}
	return nil
}

// NewState returns an initialized State with all phases set to pending.
func NewState(runID, scenarioName, scenarioPath string, inputs map[string]any) *State {
	phases := make(map[Phase]Status, len(AllPhases))
	for _, p := range AllPhases {
		phases[p] = StatusPending
	}
	return &State{
		RunID:        runID,
		ScenarioName: scenarioName,
		ScenarioPath: scenarioPath,
		StartedAt:    time.Now().UTC(),
		Inputs:       inputs,
		Phases:       phases,
	}
}

// IsTerminal reports whether the run has reached a final state.
func (s *State) IsTerminal() bool {
	return s.CompletedAt != nil
}
