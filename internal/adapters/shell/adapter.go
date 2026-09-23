// Package shell implements a WorkloadAdapter that runs a local shell command.
package shell

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/dbarena/benchctl/internal/engine"
	"github.com/dbarena/benchctl/internal/schema"
)

// Adapter implements engine.WorkloadAdapter by running args["command"] via
// "sh -c" on the local machine. It returns empty Metrics.
type Adapter struct {
	out io.Writer
}

// New returns an Adapter that writes command output to os.Stdout.
func New() *Adapter { return &Adapter{out: os.Stdout} }

// NewWithWriter returns an Adapter that writes command output to w, for testing.
func NewWithWriter(w io.Writer) *Adapter { return &Adapter{out: w} }

// Run executes args["command"] via "sh -c" on the local machine, streaming
// output to the adapter's writer. Returns empty Metrics on success.
func (a *Adapter) Run(ctx context.Context, _ engine.Outputs, step schema.SuiteStep) (engine.Metrics, error) {
	cmd := step.Args["command"]
	if cmd == "" {
		return nil, fmt.Errorf("shell step %q: args.command is required", step.Name)
	}
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Stdout = a.out
	c.Stderr = a.out
	if err := c.Run(); err != nil {
		return nil, fmt.Errorf("shell step %q: %w", step.Name, err)
	}
	return engine.Metrics{}, nil
}
