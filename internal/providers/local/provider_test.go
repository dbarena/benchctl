package local_test

import (
	"context"
	"testing"

	"github.com/dbarena/benchctl/internal/providers/local"
)

func TestProvisionReturnsEmptyOutputs(t *testing.T) {
	p := local.New()
	outputs, err := p.Provision(context.Background(), "", nil)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if len(outputs) != 0 {
		t.Errorf("expected empty outputs, got %v", outputs)
	}
}

func TestTeardownIsNoop(t *testing.T) {
	p := local.New()
	if err := p.Teardown(context.Background(), nil); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
}
