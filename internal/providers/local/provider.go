// Package local implements a no-op DriverProvider for scenarios where the
// load generator runs as a subprocess on the same machine as benchctl.
package local

import (
	"context"

	"github.com/dbarena/benchctl/internal/engine"
)

// Provider is a no-op DriverProvider. Provision and Teardown do nothing
// because the driver is simply the local process running benchctl.
type Provider struct{}

func New() *Provider { return &Provider{} }

func (*Provider) Provision(_ context.Context, _ string, _ map[string]any) (engine.Outputs, error) {
	return engine.Outputs{}, nil
}

// Setup is a no-op for the local driver: tools are assumed to be already
// installed on the machine running benchctl.
func (*Provider) Setup(_ context.Context, _ map[string]any, _ engine.Outputs) error {
	return nil
}

// Collect passes workload metrics through unchanged. For the local driver the
// workload adapter already returned results in-process; there is nothing to
// gather from a remote machine.
func (*Provider) Collect(_ context.Context, _ map[string]any, _ engine.Outputs, metrics engine.Metrics) (engine.Metrics, error) {
	return metrics, nil
}

func (*Provider) Teardown(_ context.Context, _ engine.Outputs) error {
	return nil
}
