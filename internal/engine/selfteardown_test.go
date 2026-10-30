package engine

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRunSelfTeardown_TimeoutPointsAtBenchctlTeardown(t *testing.T) {
	slow := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	err := runSelfTeardown([]func(context.Context) error{slow}, "run-1", time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "benchctl teardown run-1") {
		t.Errorf("err = %v, want it to say to finish with benchctl teardown run-1", err)
	}
}
