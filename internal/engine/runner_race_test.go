package engine

import (
	"sync"
	"testing"
	"time"

	"github.com/dbarena/benchctl/internal/runstate"
)

// slowStore mimics the read-modify-write shape of LocalStore.Update and
// SupabaseStore.Update: Update reads a snapshot, sleeps (simulating file or
// network round-trip latency), then overwrites the stored state with the
// mutated snapshot. Two concurrent, unsynchronized Update calls lose
// whichever change lands in the snapshot that commits last. This is the
// exact race that used to exist between the main run goroutine and the
// heartbeat goroutine in Run/Resume.
type slowStore struct {
	mu    sync.Mutex
	state *runstate.State
}

func (s *slowStore) Create(st *runstate.State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *st
	s.state = &cp
	return nil
}

func (s *slowStore) Update(_ string, fn func(*runstate.State)) error {
	s.mu.Lock()
	cp := *s.state
	phases := make(map[runstate.Phase]runstate.Status, len(s.state.Phases))
	for k, v := range s.state.Phases {
		phases[k] = v
	}
	cp.Phases = phases
	s.mu.Unlock()

	fn(&cp)
	time.Sleep(20 * time.Millisecond) // simulate GET/PATCH round-trip latency

	s.mu.Lock()
	s.state = &cp
	s.mu.Unlock()
	return nil
}

func (s *slowStore) Load(_ string) (*runstate.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *s.state
	return &cp, nil
}

func (s *slowStore) List() ([]*runstate.State, error) { return nil, nil }

// TestSetState_SerializesConcurrentWriters proves that stateMu closes the
// race between the main run goroutine and the heartbeat goroutine: both
// write to run state via setState, and without serialization, whichever
// write's read-modify-write cycle finishes last silently drops the other's
// change.
func TestSetState_SerializesConcurrentWriters(t *testing.T) {
	store := &slowStore{}
	runID := "race-test"
	if err := store.Create(runstate.NewState(runID, "test", "", nil)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	r := &Runner{Store: store}
	r.initStateMu()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		r.setState(runID, func(st *runstate.State) {
			st.Phases[runstate.PhaseWorkloadExecute] = runstate.StatusCompleted
		})
	}()
	go func() {
		defer wg.Done()
		now := time.Now().UTC()
		r.setState(runID, func(st *runstate.State) {
			st.LastHeartbeat = &now
		})
	}()
	wg.Wait()

	final, err := store.Load(runID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if final.Phases[runstate.PhaseWorkloadExecute] != runstate.StatusCompleted {
		t.Errorf("phase write was lost to the race: Phases[workload.execute] = %v, want completed", final.Phases[runstate.PhaseWorkloadExecute])
	}
	if final.LastHeartbeat == nil {
		t.Error("heartbeat write was lost to the race: LastHeartbeat is nil")
	}
}
