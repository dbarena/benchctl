package sshdriver

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/dbarena/benchctl/internal/config"
	"github.com/dbarena/benchctl/internal/engine"
	"github.com/dbarena/benchctl/internal/tofustate"
)

// installFakeTofu puts a fake `tofu` executable at the front of PATH that
// exits with the given code for each subcommand (defaulting to 0), so tests
// can exercise Provision/Teardown's control flow without a real OpenTofu
// binary or any cloud credentials.
func installFakeTofu(t *testing.T, exitCodes map[string]int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake tofu script requires a POSIX shell")
	}

	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in\n"
	for cmd, code := range exitCodes {
		script += fmt.Sprintf("  %s) exit %d ;;\n", cmd, code)
	}
	script += "  output) echo '{}' ;;\n  *) exit 0 ;;\nesac\n"

	path := filepath.Join(dir, "tofu")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tofu: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestProvision_ApplyFailsDestroySucceeds_RemovesWorkDir(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	installFakeTofu(t, map[string]int{"apply": 1})

	moduleDir := filepath.Join(tmp, "module")
	if err := os.MkdirAll(moduleDir, 0o755); err != nil {
		t.Fatalf("mkdir module dir: %v", err)
	}
	dummyBinary := filepath.Join(tmp, "benchctl-linux-amd64")
	if err := os.WriteFile(dummyBinary, []byte("dummy"), 0o755); err != nil {
		t.Fatalf("write dummy benchctl binary: %v", err)
	}

	const runID = "test-run-20260101-000000-abcdef"
	p := New(Config{ProviderName: "test", Benchctl: &config.Config{}})
	p.out = &bytes.Buffer{}

	cfg := map[string]any{
		"module":          moduleDir,
		"benchctl_binary": dummyBinary,
	}
	outputs, err := p.Provision(context.Background(), runID, cfg)
	if err == nil {
		t.Fatal("Provision: want error from failed apply, got nil")
	}
	if outputs != nil {
		t.Fatalf("Provision: want nil outputs (destroy succeeded, no infra left), got %v", outputs)
	}

	workDir := filepath.Join(tofustate.RunDir(runID), filepath.Base(moduleDir))
	if _, statErr := os.Stat(workDir); !os.IsNotExist(statErr) {
		t.Errorf("workdir %s: want removed, stat err = %v", workDir, statErr)
	}
	runDir := tofustate.RunDir(runID)
	if _, statErr := os.Stat(runDir); !os.IsNotExist(statErr) {
		t.Errorf("run dir %s: want removed, stat err = %v", runDir, statErr)
	}
}

// TestProvision_ReadTofuOutputsFailsAfterApply_ReturnsWorkDirForTeardown is
// the regression test for the leak class this fix addresses: apply already
// succeeded (real infra exists) by the time `tofu output` fails, so
// Provision must return enough outputs for `benchctl teardown` to destroy it
// later, not a bare error that leaves it with no local or remote record.
func TestProvision_ReadTofuOutputsFailsAfterApply_ReturnsWorkDirForTeardown(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	installFakeTofu(t, map[string]int{"output": 1})

	moduleDir := filepath.Join(tmp, "module")
	if err := os.MkdirAll(moduleDir, 0o755); err != nil {
		t.Fatalf("mkdir module dir: %v", err)
	}
	dummyBinary := filepath.Join(tmp, "benchctl-linux-amd64")
	if err := os.WriteFile(dummyBinary, []byte("dummy"), 0o755); err != nil {
		t.Fatalf("write dummy benchctl binary: %v", err)
	}

	const runID = "test-run-20260101-000000-abcdef"
	p := New(Config{ProviderName: "test", Benchctl: &config.Config{}})
	p.out = &bytes.Buffer{}

	cfg := map[string]any{
		"module":          moduleDir,
		"benchctl_binary": dummyBinary,
	}
	outputs, err := p.Provision(context.Background(), runID, cfg)
	if err == nil {
		t.Fatal("Provision: want error from failed tofu output, got nil")
	}

	workDir := filepath.Join(tofustate.RunDir(runID), filepath.Base(moduleDir))
	absWorkDir, err2 := filepath.Abs(workDir)
	if err2 != nil {
		t.Fatalf("filepath.Abs: %v", err2)
	}
	if outputs == nil || outputs[engine.OutputKeyTofuWorkDir] != absWorkDir {
		t.Fatalf("Provision: want outputs[%s]=%s (apply succeeded, real infra exists), got %v",
			engine.OutputKeyTofuWorkDir, absWorkDir, outputs)
	}

	// Unlike the apply-failure path, nothing destroyed the infra here, so its
	// tfstate must remain on disk for a later `benchctl teardown` to use.
	if _, statErr := os.Stat(workDir); statErr != nil {
		t.Errorf("workdir %s: want present for later teardown, stat err = %v", workDir, statErr)
	}
}

func TestSyncTFFiles_CopiesSymlinkedShellScript(t *testing.T) {
	srcDir := t.TempDir()
	sharedDir := t.TempDir()
	dstDir := t.TempDir()

	sharedScript := filepath.Join(sharedDir, "reduce-variability.sh")
	if err := os.WriteFile(sharedScript, []byte("#!/bin/bash\necho hi\n"), 0o644); err != nil {
		t.Fatalf("write shared script: %v", err)
	}
	if err := os.Symlink(sharedScript, filepath.Join(srcDir, "reduce-variability.sh")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "main.tf"), []byte("# noop\n"), 0o644); err != nil {
		t.Fatalf("write main.tf: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "ignored.txt"), []byte("ignored\n"), 0o644); err != nil {
		t.Fatalf("write ignored.txt: %v", err)
	}

	if err := syncTFFiles(srcDir, dstDir); err != nil {
		t.Fatalf("syncTFFiles: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dstDir, "reduce-variability.sh"))
	if err != nil {
		t.Fatalf("read synced script: %v", err)
	}
	if string(got) != "#!/bin/bash\necho hi\n" {
		t.Errorf("synced script content = %q, want shared script content", got)
	}
	if fi, err := os.Lstat(filepath.Join(dstDir, "reduce-variability.sh")); err != nil {
		t.Fatalf("lstat synced script: %v", err)
	} else if fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("synced script is still a symlink; want a real file")
	}
	if _, err := os.Stat(filepath.Join(dstDir, "main.tf")); err != nil {
		t.Errorf("main.tf not copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "ignored.txt")); !os.IsNotExist(err) {
		t.Errorf("ignored.txt should not have been copied, err = %v", err)
	}
}
