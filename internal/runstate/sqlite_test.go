package runstate

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *LocalStore {
	t.Helper()
	store, err := NewLocalStoreAt(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewLocalStoreAt: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestLocalStore_Create_RejectsDuplicateRunID(t *testing.T) {
	store := newTestStore(t)

	first := NewState("run-a", "scenario", "scenarios/x.yaml", nil)
	if err := store.Create(first); err != nil {
		t.Fatalf("first Create: %v", err)
	}

	second := NewState("run-a", "other-scenario", "scenarios/x.yaml", nil)
	if err := store.Create(second); err == nil {
		t.Fatal("expected error creating a run with a colliding id, got nil")
	}

	// The original state must be untouched by the rejected second Create.
	loaded, err := store.Load("run-a")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.ScenarioName != "scenario" {
		t.Errorf("scenario name = %q, want unchanged original", loaded.ScenarioName)
	}
}

func TestLocalStore_Create_DistinctIDsSucceed(t *testing.T) {
	store := newTestStore(t)

	if err := store.Create(NewState("run-a", "scenario", "scenarios/x.yaml", nil)); err != nil {
		t.Fatalf("Create run-a: %v", err)
	}
	if err := store.Create(NewState("run-b", "scenario", "scenarios/x.yaml", nil)); err != nil {
		t.Fatalf("Create run-b: %v", err)
	}

	states, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(states) != 2 {
		t.Errorf("List returned %d states, want 2", len(states))
	}
}

func TestLocalStore_Load_MissingRunIsErrRunNotFound(t *testing.T) {
	store := newTestStore(t)

	_, err := store.Load("nope")
	if !errors.Is(err, ErrRunNotFound) {
		t.Errorf("Load error = %v, want ErrRunNotFound", err)
	}
}

// TestLocalStore_CurrentStepFields_SurviveRoundTrip verifies that
// CurrentFixture/CurrentIteration/CurrentStep/StepStartedAt persist through an
// Update/Load cycle. That is the exact path `benchctl status <run-id> -o json`
// reads from, and what an external pprof-capture script would poll.
func TestLocalStore_CurrentStepFields_SurviveRoundTrip(t *testing.T) {
	store := newTestStore(t)

	if err := store.Create(NewState("run-a", "scenario", "scenarios/x.yaml", nil)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	started := time.Now().UTC().Truncate(time.Second)
	err := store.Update("run-a", func(st *State) {
		st.CurrentFixture = map[string]string{"clients": "8"}
		st.CurrentIteration = 2
		st.CurrentStep = "benchmark"
		st.StepStartedAt = &started
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	loaded, err := store.Load("run-a")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.CurrentFixture["clients"] != "8" {
		t.Errorf("CurrentFixture[clients] = %q, want 8", loaded.CurrentFixture["clients"])
	}
	if loaded.CurrentIteration != 2 {
		t.Errorf("CurrentIteration = %d, want 2", loaded.CurrentIteration)
	}
	if loaded.CurrentStep != "benchmark" {
		t.Errorf("CurrentStep = %q, want benchmark", loaded.CurrentStep)
	}
	if loaded.StepStartedAt == nil || !loaded.StepStartedAt.Equal(started) {
		t.Errorf("StepStartedAt = %v, want %v", loaded.StepStartedAt, started)
	}
}

func TestLocalStore_Update_MissingRunIsErrRunNotFound(t *testing.T) {
	store := newTestStore(t)

	err := store.Update("nope", func(*State) {})
	if !errors.Is(err, ErrRunNotFound) {
		t.Errorf("Update error = %v, want ErrRunNotFound", err)
	}
}

func TestLocalStore_List_OrdersMostRecentFirst(t *testing.T) {
	store := newTestStore(t)

	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	for i, id := range []string{"oldest", "newest", "middle"} {
		st := NewState(id, "scenario", "scenarios/x.yaml", nil)
		switch i {
		case 0:
			st.StartedAt = base
		case 1:
			st.StartedAt = base.Add(2 * time.Hour)
		case 2:
			st.StartedAt = base.Add(time.Hour)
		}
		if err := store.Create(st); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}

	states, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := make([]string, len(states))
	for i, st := range states {
		got[i] = st.RunID
	}
	want := []string{"newest", "middle", "oldest"}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("List order = %v, want %v", got, want)
		}
	}
}

func TestLocalStore_Delete(t *testing.T) {
	store := newTestStore(t)

	if err := store.Create(NewState("run-a", "scenario", "scenarios/x.yaml", nil)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Delete("run-a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Load("run-a"); !errors.Is(err, ErrRunNotFound) {
		t.Errorf("Load after Delete = %v, want ErrRunNotFound", err)
	}
	// Deleting an absent run is not an error: `purge` may race with another
	// process that already removed the record.
	if err := store.Delete("run-a"); err != nil {
		t.Errorf("Delete of absent run = %v, want nil", err)
	}
}

// TestLocalStore_Update_ConcurrentWritersDoNotLoseUpdates exercises the case
// engine.Runner hits for real: the main run goroutine and the heartbeat
// goroutine both mutating one record. Each writer appends a distinct metadata
// key, so a lost read-modify-write shows up as a missing key.
func TestLocalStore_Update_ConcurrentWritersDoNotLoseUpdates(t *testing.T) {
	store := newTestStore(t)

	if err := store.Create(NewState("run-a", "scenario", "scenarios/x.yaml", nil)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	const writers = 8
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = store.Update("run-a", func(st *State) {
				if st.Metadata == nil {
					st.Metadata = map[string]string{}
				}
				st.Metadata[string(rune('a'+i))] = "set"
			})
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	loaded, err := store.Load("run-a")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Metadata) != writers {
		t.Errorf("Metadata has %d keys, want %d (a lost update drops one): %v", len(loaded.Metadata), writers, loaded.Metadata)
	}
}

// TestLocalStore_ListActive_MatchesShowsByDefault pins the local store's SQL
// filter to the same predicate cmd/benchctl/status.go's showsByDefault and
// SupabaseStore.ListActive's PostgREST filter implement.
func TestLocalStore_ListActive_MatchesShowsByDefault(t *testing.T) {
	// showsByDefault, restated: active while not terminal, or while the
	// environment is still up after completing.
	showsByDefault := func(st *State) bool {
		if !st.IsTerminal() {
			return true
		}
		return st.TerminatedAt == nil && len(st.TargetOutputs) > 0
	}

	now := time.Now().UTC()
	cases := []struct {
		name      string
		mutate    func(*State)
		wantShown bool
	}{
		{"running, no infra yet", func(*State) {}, true},
		{"running, infra up", func(st *State) { st.TargetOutputs = map[string]string{"host": "h"} }, true},
		{"completed, infra up", func(st *State) {
			st.TargetOutputs = map[string]string{"host": "h"}
			st.CompletedAt = &now
		}, true},
		{"completed, infra torn down", func(st *State) {
			st.TargetOutputs = map[string]string{"host": "h"}
			st.CompletedAt = &now
			st.TerminatedAt = &now
		}, false},
		{"completed, never had infra", func(st *State) { st.CompletedAt = &now }, false},
	}

	store := newTestStore(t)
	want := map[string]bool{}
	for _, tc := range cases {
		st := NewState(tc.name, "scenario", "scenarios/x.yaml", nil)
		tc.mutate(st)
		if err := store.Create(st); err != nil {
			t.Fatalf("Create %s: %v", tc.name, err)
		}
		// Guard the restated predicate itself against the table.
		if got := showsByDefault(st); got != tc.wantShown {
			t.Fatalf("%s: showsByDefault = %v, want %v", tc.name, got, tc.wantShown)
		}
		want[tc.name] = tc.wantShown
	}

	active, err := store.ListActive()
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	got := map[string]bool{}
	for _, st := range active {
		got[st.RunID] = true
	}
	for name, shown := range want {
		if got[name] != shown {
			t.Errorf("ListActive included %q = %v, want %v", name, got[name], shown)
		}
	}
}

func TestLocalStore_RejectsNewerSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := NewLocalStoreAt(path)
	if err != nil {
		t.Fatalf("NewLocalStoreAt: %v", err)
	}
	if _, err := store.db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatalf("bump user_version: %v", err)
	}
	store.Close()

	if _, err := NewLocalStoreAt(path); err == nil {
		t.Fatal("expected an error opening a database written by a newer benchctl")
	}
}
