package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/dbarena/benchctl/internal/runstate"
)

// fakeLoadStore is a minimal runstate.Store whose Load returns queued errors
// (or a terminal state on the final call) in sequence.
type fakeLoadStore struct {
	loadErrs []error // returned in order; the last entry, if nil, means "return okState"
	okState  *runstate.State
	calls    int
}

func (f *fakeLoadStore) Create(*runstate.State) error               { return nil }
func (f *fakeLoadStore) Update(string, func(*runstate.State)) error { return nil }
func (f *fakeLoadStore) List() ([]*runstate.State, error)           { return nil, nil }

func (f *fakeLoadStore) Load(string) (*runstate.State, error) {
	idx := f.calls
	if idx >= len(f.loadErrs) {
		idx = len(f.loadErrs) - 1
	}
	f.calls++
	if err := f.loadErrs[idx]; err != nil {
		return nil, err
	}
	return f.okState, nil
}

func TestPollRunStateNotFoundFailsImmediately(t *testing.T) {
	notFoundErr := fmt.Errorf("runstate: run %q not found: %w", "r1", runstate.ErrRunNotFound)
	store := &fakeLoadStore{loadErrs: []error{notFoundErr}}

	var out bytes.Buffer
	err := pollRunState(context.Background(), store, "r1", time.Millisecond, &out)

	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, runstate.ErrRunNotFound) {
		t.Fatalf("expected error to wrap ErrRunNotFound, got: %v", err)
	}
	if store.calls != 1 {
		t.Fatalf("expected exactly 1 Load call (no retries for a definitive not-found), got %d", store.calls)
	}
	if out.Len() != 0 {
		t.Fatalf("expected no transient-warning output, got: %q", out.String())
	}
}

func TestPollRunStateTransientErrorsStillRetry(t *testing.T) {
	transientErr := errors.New("dial tcp: connection reset by peer")
	completed := &runstate.State{CompletedAt: timePtr(time.Now().UTC())}

	store := &fakeLoadStore{
		loadErrs: []error{transientErr, transientErr, nil},
		okState:  completed,
	}

	var out bytes.Buffer
	err := pollRunState(context.Background(), store, "r1", time.Millisecond, &out)

	if err != nil {
		t.Fatalf("expected success after recovering from transient errors, got: %v", err)
	}
	if store.calls != 3 {
		t.Fatalf("expected 3 Load calls (2 transient failures + 1 success), got %d", store.calls)
	}
}

func TestPollRunStateGivesUpAfterMaxConsecutiveTransientErrors(t *testing.T) {
	transientErr := errors.New("dial tcp: connection reset by peer")
	store := &fakeLoadStore{loadErrs: []error{transientErr}}

	var out bytes.Buffer
	err := pollRunState(context.Background(), store, "r1", time.Millisecond, &out)

	if err == nil {
		t.Fatal("expected an error after exhausting retries, got nil")
	}
	if errors.Is(err, runstate.ErrRunNotFound) {
		t.Fatalf("transient error should not be reported as ErrRunNotFound: %v", err)
	}
	if store.calls != maxConsecutiveLoadErrors {
		t.Fatalf("expected %d Load calls, got %d", maxConsecutiveLoadErrors, store.calls)
	}
}

func timePtr(t time.Time) *time.Time { return &t }
