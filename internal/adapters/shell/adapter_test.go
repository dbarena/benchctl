package shell_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/dbarena/benchctl/internal/adapters/shell"
	"github.com/dbarena/benchctl/internal/engine"
	"github.com/dbarena/benchctl/internal/schema"
)

func TestShellAdapter_Run_Echo(t *testing.T) {
	var buf bytes.Buffer
	a := shell.NewWithWriter(&buf)
	step := schema.SuiteStep{
		Name: "echo-step",
		Type: "shell",
		Args: map[string]string{"command": "echo hello"},
	}
	metrics, err := a.Run(context.Background(), engine.Outputs{}, step)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(metrics) != 0 {
		t.Errorf("len(metrics) = %d, want 0", len(metrics))
	}
	if !strings.Contains(buf.String(), "hello") {
		t.Errorf("output %q should contain hello", buf.String())
	}
}

func TestShellAdapter_Run_MissingCommand(t *testing.T) {
	a := shell.New()
	step := schema.SuiteStep{Name: "no-cmd", Type: "shell", Args: map[string]string{}}
	_, err := a.Run(context.Background(), nil, step)
	if err == nil {
		t.Fatal("expected error for missing args.command, got nil")
	}
}

func TestShellAdapter_Run_ExitFailure(t *testing.T) {
	a := shell.New()
	step := schema.SuiteStep{
		Name: "fail",
		Type: "shell",
		Args: map[string]string{"command": "exit 1"},
	}
	_, err := a.Run(context.Background(), nil, step)
	if err == nil {
		t.Fatal("expected error for exit 1, got nil")
	}
}
