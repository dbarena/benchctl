package runstate

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

// DefaultRefreshInterval is how long a record read from a driver instance stays
// fresh. It has to leave comfortable room above refreshTimeout: a single SSH
// round trip over a slow link can take several seconds, and a shorter
// interval would expire before the previous refresh returned, making every
// read relay.
const DefaultRefreshInterval = 30 * time.Second

const (
	// defaultRefreshTimeout bounds one refresh, and a whole listing.
	defaultRefreshTimeout = 15 * time.Second
	// refreshConcurrency bounds parallel refreshes during a listing, so a
	// long run history does not open dozens of SSH connections at once.
	refreshConcurrency = 8
)

// RemoteAccess describes how to reach the machine executing a run.
type RemoteAccess struct {
	// SSHArgs is a complete argv for a non-interactive SSH session, e.g.
	// {"ssh", "-i", "/path/key", "ubuntu@1.2.3.4"}.
	SSHArgs []string
	// BenchctlPath is where the benchctl binary lives on that machine.
	BenchctlPath string
}

// Dialer reports how to reach the driver instance for a run. It is supplied by
// the command layer, which can resolve a provider from a State (and so reuse
// its SSH user, key and host-key handling) without runstate having to import
// any provider package.
type Dialer func(*State) (RemoteAccess, error)

// commandRunner executes argv and returns its stdout. Injected so tests never
// shell out.
type commandRunner func(ctx context.Context, argv []string) ([]byte, error)

func execCommand(ctx context.Context, argv []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return out, nil
}

// refresh brings rec up to date from the machine executing it, and returns
// the state to hand back to the caller.
//
// It never reports an error. A torn-down driver instance is the normal end
// state of a finished run, and cmd/benchctl's wait loop gives up after a
// handful of consecutive Load errors, so a refresh failure degrades to the last
// known snapshot, marked stale, rather than failing the read.
func (s *LocalStore) refresh(ctx context.Context, rec record) *State {
	if !s.shouldRefresh(rec) {
		rec.state.RefreshedAt = rec.refreshedAt
		rec.state.RefreshError = rec.refreshError
		return rec.state
	}

	ctx, cancel := context.WithTimeout(ctx, s.effectiveRefreshTimeout())
	defer cancel()

	fresh, err := s.fetch(ctx, rec.state)
	if err != nil {
		// Best effort: the point of recording the failure is to show it in
		// `benchctl status`, so failing to record it must not fail the read.
		_ = s.markRefreshFailed(rec.state.RunID, err)
		rec.state.RefreshedAt = rec.refreshedAt
		rec.state.RefreshError = err.Error()
		return rec.state
	}

	now := time.Now().UTC()
	if err := s.storeRefreshed(fresh, now); err != nil {
		fresh.RefreshError = err.Error()
	}
	fresh.RefreshedAt = now
	return fresh
}

// shouldRefresh reports whether rec has to be read back from the machine
// executing the run.
func (s *LocalStore) shouldRefresh(rec record) bool {
	switch {
	case s.dial == nil:
		// No way to reach anything: the driver-side process and tests.
		return false
	case rec.authoritative:
		// This machine executes the run, so the local record is the truth.
		return false
	case rec.state.IsTerminal():
		// Terminal records never change again.
		return false
	case time.Since(rec.refreshedAt) < s.effectiveRefreshInterval():
		return false
	}
	return true
}

func (s *LocalStore) effectiveRefreshInterval() time.Duration {
	if s.refreshInterval <= 0 {
		return DefaultRefreshInterval
	}
	return s.refreshInterval
}

func (s *LocalStore) effectiveRefreshTimeout() time.Duration {
	if s.refreshTimeout <= 0 {
		return defaultRefreshTimeout
	}
	return s.refreshTimeout
}

// refreshAll refreshes every record that needs it, concurrently, under one
// deadline for the whole listing. One unreachable host must not stall a
// listing, so records that do not make it are returned from cache.
func (s *LocalStore) refreshAll(recs []record) []*State {
	ctx, cancel := context.WithTimeout(context.Background(), s.effectiveRefreshTimeout())
	defer cancel()

	states := make([]*State, len(recs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, refreshConcurrency)
	for i, rec := range recs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			states[i] = s.refresh(ctx, rec)
		}()
	}
	wg.Wait()
	return states
}

// fetch reads the run's own record from the machine executing it, by asking
// the benchctl installed there to export it.
func (s *LocalStore) fetch(ctx context.Context, state *State) (*State, error) {
	access, err := s.dial(state)
	if err != nil {
		return nil, err
	}
	if len(access.SSHArgs) == 0 || access.BenchctlPath == "" {
		return nil, fmt.Errorf("runstate: no way to reach the driver instance for run %s", state.RunID)
	}

	// BatchMode forbids any interactive prompt: a refresh happens behind a
	// plain `benchctl status`, so it must never sit waiting for input.
	argv := append([]string{access.SSHArgs[0], "-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}, access.SSHArgs[1:]...)
	argv = append(argv, access.BenchctlPath, "state", "export", state.RunID)

	out, err := s.run(ctx, argv)
	if err != nil {
		return nil, fmt.Errorf("read run state from driver instance: %w", err)
	}
	var fresh State
	if err := json.Unmarshal(out, &fresh); err != nil {
		return nil, fmt.Errorf("parse run state from driver instance: %w", err)
	}
	if fresh.RunID != state.RunID {
		return nil, fmt.Errorf("driver instance returned run %q, want %q", fresh.RunID, state.RunID)
	}
	return &fresh, nil
}

// storeRefreshed writes a record read back from the driver instance. It leaves
// authoritative alone: the driver still owns the run.
func (s *LocalStore) storeRefreshed(state *State, at time.Time) error {
	values, err := rowValues(state)
	if err != nil {
		return err
	}
	args := append(values[1:], at.UTC().Format(time.RFC3339Nano), values[0])
	_, err = s.db.Exec(`UPDATE runs
	                    SET started_at = ?, completed_at = ?, terminated_at = ?, has_target = ?,
	                        created_by = ?, doc = ?, refreshed_at = ?, refresh_error = NULL
	                    WHERE run_id = ?`, args...)
	if err != nil {
		return fmt.Errorf("runstate: cache refreshed state for %s: %w", state.RunID, err)
	}
	return nil
}

func (s *LocalStore) markRefreshFailed(runID string, cause error) error {
	_, err := s.db.Exec(`UPDATE runs SET refresh_error = ? WHERE run_id = ?`, cause.Error(), runID)
	return err
}
