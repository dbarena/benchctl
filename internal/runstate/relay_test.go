package runstate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRunner stands in for SSH. It records every argv it is handed and
// replies with whatever the test set up, so no test needs a network.
// Refreshes run concurrently during a listing, hence the mutex.
type fakeRunner struct {
	calls atomic.Int32
	out   []byte
	err   error
	block chan struct{} // when non-nil, the call waits for ctx or this

	mu       sync.Mutex
	lastArgv []string
}

func (f *fakeRunner) run(ctx context.Context, argv []string) ([]byte, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.lastArgv = argv
	f.mu.Unlock()
	if f.block != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-f.block:
		}
	}
	return f.out, f.err
}

func (f *fakeRunner) argv() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.lastArgv, " ")
}

// relayStore returns a store that reads through to a fake driver instance.
func relayStore(t *testing.T, runner *fakeRunner) *LocalStore {
	t.Helper()
	store := newTestStore(t)
	store.dial = func(*State) (RemoteAccess, error) {
		return RemoteAccess{SSHArgs: []string{"ssh", "ubuntu@1.2.3.4"}, BenchctlPath: "~/benchctl/benchctl"}, nil
	}
	store.run = runner.run
	store.refreshTimeout = 500 * time.Millisecond
	return store
}

// delegatedRun creates a run and hands it off, the state RunAsync leaves
// behind after bootstrapping a driver instance.
func delegatedRun(t *testing.T, store *LocalStore, runID string) *State {
	t.Helper()
	st := NewState(runID, "scenario", "scenarios/x.yaml", nil)
	st.DriverProvider = "ec2"
	st.DriverOutputs = map[string]string{"public_ip": "1.2.3.4"}
	st.TargetOutputs = map[string]string{"host": "10.0.0.1"}
	if err := store.Create(st); err != nil {
		t.Fatalf("Create %s: %v", runID, err)
	}
	if err := store.Delegate(runID); err != nil {
		t.Fatalf("Delegate %s: %v", runID, err)
	}
	return st
}

func marshalState(t *testing.T, st *State) []byte {
	t.Helper()
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

func TestRelay_TerminalRecordIsNotRefreshed(t *testing.T) {
	runner := &fakeRunner{}
	store := relayStore(t, runner)
	delegatedRun(t, store, "run-a")

	now := time.Now().UTC()
	if err := store.Update("run-a", func(st *State) { st.CompletedAt = &now }); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if _, err := store.Load("run-a"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := runner.calls.Load(); got != 0 {
		t.Errorf("runner called %d times; a terminal record never changes again", got)
	}
}

func TestRelay_AuthoritativeRecordIsNotRefreshed(t *testing.T) {
	runner := &fakeRunner{}
	store := relayStore(t, runner)
	// Created but never delegated: this machine is still executing the run.
	st := NewState("run-a", "scenario", "scenarios/x.yaml", nil)
	st.DriverOutputs = map[string]string{"public_ip": "1.2.3.4"}
	if err := store.Create(st); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := store.Load("run-a"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := runner.calls.Load(); got != 0 {
		t.Errorf("runner called %d times; this machine owns the record", got)
	}
}

func TestRelay_NoDialerNeverReachesOut(t *testing.T) {
	runner := &fakeRunner{}
	store := relayStore(t, runner)
	store.dial = nil
	delegatedRun(t, store, "run-a")

	if _, err := store.Load("run-a"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := runner.calls.Load(); got != 0 {
		t.Errorf("runner called %d times without a dialer", got)
	}
}

func TestRelay_RefreshMergesRemoteStateAndCachesIt(t *testing.T) {
	runner := &fakeRunner{}
	store := relayStore(t, runner)
	store.refreshInterval = time.Hour
	local := delegatedRun(t, store, "run-a")

	// What the driver instance reports: one phase further along.
	remote := *local
	remote.Phases = map[Phase]Status{PhaseDriverSetup: StatusCompleted}
	remote.CurrentStep = "benchmark"
	runner.out = marshalState(t, &remote)

	loaded, err := store.Load("run-a")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.CurrentStep != "benchmark" {
		t.Errorf("CurrentStep = %q, want the driver instance's value", loaded.CurrentStep)
	}
	if loaded.RefreshedAt.IsZero() {
		t.Error("RefreshedAt should record when the snapshot was taken")
	}
	if loaded.IsStale() {
		t.Errorf("a successful refresh is not stale: %q", loaded.RefreshError)
	}

	// The refresh asks the driver's own benchctl for the record.
	argv := runner.argv()
	for _, want := range []string{"ssh", "BatchMode=yes", "ubuntu@1.2.3.4", "~/benchctl/benchctl state export run-a"} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv %q should contain %q", argv, want)
		}
	}

	// Within the interval the cached copy is served, without another SSH.
	again, err := store.Load("run-a")
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if again.CurrentStep != "benchmark" {
		t.Error("the refreshed record should have been cached")
	}
	if got := runner.calls.Load(); got != 1 {
		t.Errorf("runner called %d times, want 1: the second read is within the refresh interval", got)
	}
}

func TestRelay_RefreshExpiresAfterInterval(t *testing.T) {
	runner := &fakeRunner{}
	store := relayStore(t, runner)
	store.refreshInterval = time.Nanosecond
	local := delegatedRun(t, store, "run-a")
	runner.out = marshalState(t, local)

	for i := range 2 {
		if _, err := store.Load("run-a"); err != nil {
			t.Fatalf("Load %d: %v", i, err)
		}
	}
	if got := runner.calls.Load(); got != 2 {
		t.Errorf("runner called %d times, want 2: the cached copy expired between reads", got)
	}
}

func TestRelay_FailureFallsBackToCachedSnapshot(t *testing.T) {
	runner := &fakeRunner{err: errors.New("connection refused")}
	store := relayStore(t, runner)
	delegatedRun(t, store, "run-a")

	// A refresh failure must not fail the read: a torn-down driver instance is
	// the normal end state of a finished run, and cmd/benchctl's wait loop
	// gives up after a handful of consecutive Load errors.
	loaded, err := store.Load("run-a")
	if err != nil {
		t.Fatalf("Load should fall back to cache, got: %v", err)
	}
	if loaded.RunID != "run-a" {
		t.Fatalf("expected the cached record, got %+v", loaded)
	}
	if !loaded.IsStale() {
		t.Error("a record that could not be refreshed is stale")
	}
	if !strings.Contains(loaded.RefreshError, "connection refused") {
		t.Errorf("RefreshError = %q, want the underlying cause", loaded.RefreshError)
	}

	// The failure is recorded, so a later read served from cache still
	// reports the run as stale.
	store.refreshInterval = time.Hour
	again, err := store.Load("run-a")
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if !again.IsStale() {
		t.Error("the recorded refresh failure should survive into later reads")
	}
}

func TestRelay_MismatchedRunIDIsRejected(t *testing.T) {
	runner := &fakeRunner{}
	store := relayStore(t, runner)
	delegatedRun(t, store, "run-a")
	other := NewState("run-b", "scenario", "scenarios/x.yaml", nil)
	runner.out = marshalState(t, other)

	loaded, err := store.Load("run-a")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.RunID != "run-a" {
		t.Errorf("run ID = %q; a mismatched reply must not replace the record", loaded.RunID)
	}
	if !loaded.IsStale() {
		t.Error("a mismatched reply leaves the record stale")
	}
}

// TestRelay_ListActive_OneUnreachableHostDoesNotStallTheRest is the property
// that matters for `benchctl status`: run histories accumulate hosts that no
// longer answer, and one of them must not hold up the listing.
func TestRelay_ListActive_OneUnreachableHostDoesNotStallTheRest(t *testing.T) {
	runner := &fakeRunner{block: make(chan struct{})}
	store := relayStore(t, runner)
	store.refreshTimeout = 200 * time.Millisecond
	for _, id := range []string{"run-a", "run-b", "run-c"} {
		delegatedRun(t, store, id)
	}

	start := time.Now()
	states, err := store.ListActive()
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	elapsed := time.Since(start)

	if len(states) != 3 {
		t.Fatalf("ListActive returned %d states, want 3", len(states))
	}
	for _, st := range states {
		if !st.IsStale() {
			t.Errorf("run %s should be marked stale after the deadline", st.RunID)
		}
	}
	// One shared deadline for the whole listing, not one per run.
	if elapsed > time.Second {
		t.Errorf("listing took %s; refreshes should share one deadline and run concurrently", elapsed)
	}
}

func TestRelay_ClaimMakesRecordAuthoritativeAgain(t *testing.T) {
	runner := &fakeRunner{}
	store := relayStore(t, runner)
	delegatedRun(t, store, "run-a")

	if err := store.Claim("run-a"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, err := store.Load("run-a"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := runner.calls.Load(); got != 0 {
		t.Errorf("runner called %d times after Claim; this machine owns the record", got)
	}
}

// TestRelay_MigrationAddsOwnershipToExistingDatabase builds a database at the
// previous schema version, exactly as the last release left it, and checks
// the upgrade: records written before ownership existed default to
// authoritative, so they keep being read locally rather than suddenly trying
// to SSH somewhere.
func TestRelay_MigrationAddsOwnershipToExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := old.Exec(migrations[0]); err != nil {
		t.Fatalf("apply v1 schema: %v", err)
	}
	if _, err := old.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatalf("set version: %v", err)
	}
	values, err := rowValues(NewState("run-a", "scenario", "scenarios/x.yaml", nil))
	if err != nil {
		t.Fatalf("rowValues: %v", err)
	}
	if _, err := old.Exec(`INSERT INTO runs (`+upsertColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?)`, values...); err != nil {
		t.Fatalf("insert v1 row: %v", err)
	}
	old.Close()

	upgraded, err := NewLocalStoreAt(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer upgraded.Close()

	var version int
	if err := upgraded.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("user_version = %d, want %d", version, schemaVersion)
	}
	rec, err := upgraded.record("run-a")
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if !rec.authoritative {
		t.Error("a record written before ownership existed should default to authoritative")
	}
}

// TestRelay_RefreshThroughRealCommandRunner exercises the parts the fake
// runner stubs out: argv assembly, process execution and stdout parsing. The
// stand-in "ssh" on PATH ignores its options and prints a prepared record,
// which is what the real `benchctl state export` on a driver instance does.
func TestRelay_RefreshThroughRealCommandRunner(t *testing.T) {
	dir := t.TempDir()
	recordPath := filepath.Join(dir, "remote.json")

	fakeSSH := filepath.Join(dir, "ssh")
	script := "#!/bin/sh\nexec cat " + recordPath + "\n"
	if err := os.WriteFile(fakeSSH, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ssh: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	store := newTestStore(t)
	store.dial = func(*State) (RemoteAccess, error) {
		return RemoteAccess{SSHArgs: []string{"ssh", "ubuntu@1.2.3.4"}, BenchctlPath: "~/benchctl/benchctl"}, nil
	}
	local := delegatedRun(t, store, "run-a")

	remote := *local
	remote.CurrentStep = "benchmark"
	remote.Phases = map[Phase]Status{PhaseDriverSetup: StatusCompleted}
	if err := os.WriteFile(recordPath, marshalState(t, &remote), 0o644); err != nil {
		t.Fatalf("write record: %v", err)
	}

	loaded, err := store.Load("run-a")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.CurrentStep != "benchmark" {
		t.Errorf("CurrentStep = %q, want the driver instance's value", loaded.CurrentStep)
	}
	if loaded.IsStale() {
		t.Errorf("refresh should have succeeded: %s", loaded.RefreshError)
	}
}

// TestRelay_RefreshWhenTheCommandFails covers the same path when the driver
// host is gone: a non-zero exit must degrade to the cached snapshot.
func TestRelay_RefreshWhenTheCommandFails(t *testing.T) {
	dir := t.TempDir()
	fakeSSH := filepath.Join(dir, "ssh")
	script := "#!/bin/sh\necho 'ssh: connect to host 1.2.3.4 port 22: No route to host' >&2\nexit 255\n"
	if err := os.WriteFile(fakeSSH, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ssh: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	store := newTestStore(t)
	store.dial = func(*State) (RemoteAccess, error) {
		return RemoteAccess{SSHArgs: []string{"ssh", "ubuntu@1.2.3.4"}, BenchctlPath: "~/benchctl/benchctl"}, nil
	}
	delegatedRun(t, store, "run-a")

	loaded, err := store.Load("run-a")
	if err != nil {
		t.Fatalf("Load should fall back to cache, got: %v", err)
	}
	if !loaded.IsStale() {
		t.Error("an unreachable driver instance leaves the record stale")
	}
}
