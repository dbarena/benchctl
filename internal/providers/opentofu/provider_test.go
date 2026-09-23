package opentofu

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

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

	const runID = "test-run-20260101-000000-abcdef"
	p := New(nil, &config.Config{})
	p.out = &bytes.Buffer{}

	outputs, err := p.Provision(context.Background(), runID, map[string]any{"module": moduleDir})
	if err == nil {
		t.Fatal("Provision: want error from failed apply, got nil")
	}
	if outputs != nil {
		t.Fatalf("Provision: want nil outputs (destroy succeeded, no infra left), got %v", outputs)
	}

	workDir := deriveWorkDir(runID, moduleDir)
	if _, statErr := os.Stat(workDir); !os.IsNotExist(statErr) {
		t.Errorf("workdir %s: want removed, stat err = %v", workDir, statErr)
	}
	runDir := tofustate.RunDir(runID)
	if _, statErr := os.Stat(runDir); !os.IsNotExist(statErr) {
		t.Errorf("run dir %s: want removed, stat err = %v", runDir, statErr)
	}
}

// TestProvision_ReadOutputsFailsAfterApply_ReturnsWorkDirForTeardown is the
// regression test for the leak class this fix addresses: apply already
// succeeded (real infra exists) by the time `tofu output` fails, so
// Provision must return enough outputs for `benchctl teardown` to destroy it
// later, not a bare error that leaves it with no local or remote record.
func TestProvision_ReadOutputsFailsAfterApply_ReturnsWorkDirForTeardown(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	installFakeTofu(t, map[string]int{"output": 1})

	moduleDir := filepath.Join(tmp, "module")
	if err := os.MkdirAll(moduleDir, 0o755); err != nil {
		t.Fatalf("mkdir module dir: %v", err)
	}

	const runID = "test-run-20260101-000000-abcdef"
	p := New(nil, &config.Config{})
	p.out = &bytes.Buffer{}

	outputs, err := p.Provision(context.Background(), runID, map[string]any{"module": moduleDir})
	if err == nil {
		t.Fatal("Provision: want error from failed tofu output, got nil")
	}

	workDir := deriveWorkDir(runID, moduleDir)
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

func TestPrunePluginCache_RemovesStaleLeavesKeepsFresh(t *testing.T) {
	cacheDir := t.TempDir()

	// Stale provider version: registry.opentofu.org/hashicorp/time/0.14.1/darwin_arm64
	staleLeaf := filepath.Join(cacheDir, "registry.opentofu.org", "hashicorp", "time", "0.14.1", "darwin_arm64")
	// Fresh provider version, same registry host: .../hashicorp/aws/5.99.0/darwin_arm64
	freshLeaf := filepath.Join(cacheDir, "registry.opentofu.org", "hashicorp", "aws", "5.99.0", "darwin_arm64")

	for _, dir := range []string{staleLeaf, freshLeaf} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "terraform-provider"), []byte("bin"), 0o755); err != nil {
			t.Fatalf("write provider binary in %s: %v", dir, err)
		}
	}

	old := time.Now().Add(-pluginCacheMaxAge - 24*time.Hour)
	if err := os.Chtimes(staleLeaf, old, old); err != nil {
		t.Fatalf("chtimes %s: %v", staleLeaf, err)
	}

	var out bytes.Buffer
	prunePluginCache(&out, cacheDir)

	if _, err := os.Stat(staleLeaf); !os.IsNotExist(err) {
		t.Errorf("stale leaf %s: want removed, stat err = %v", staleLeaf, err)
	}
	if _, err := os.Stat(filepath.Dir(staleLeaf)); !os.IsNotExist(err) {
		t.Errorf("stale leaf's now-empty parent %s: want removed, stat err = %v", filepath.Dir(staleLeaf), err)
	}
	if _, err := os.Stat(freshLeaf); err != nil {
		t.Errorf("fresh leaf %s: want kept, stat err = %v", freshLeaf, err)
	}
	// Registry host dir must survive: it still has the fresh "aws" subtree.
	if _, err := os.Stat(filepath.Join(cacheDir, "registry.opentofu.org")); err != nil {
		t.Errorf("registry host dir: want kept (fresh entries remain), stat err = %v", err)
	}
}

func TestSyncTFFiles_CopiesLockFileWhenPresent(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	files := map[string]string{
		"main.tf":             "# main",
		".terraform.lock.hcl": "# lock",
		"README.md":           "# not copied",
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(srcDir, name), []byte(contents), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	if err := syncTFFiles(srcDir, dstDir); err != nil {
		t.Fatalf("syncTFFiles: %v", err)
	}

	for _, name := range []string{"main.tf", ".terraform.lock.hcl"} {
		got, err := os.ReadFile(filepath.Join(dstDir, name))
		if err != nil {
			t.Errorf("%s: want copied, read err = %v", name, err)
			continue
		}
		if string(got) != files[name] {
			t.Errorf("%s: got %q, want %q", name, got, files[name])
		}
	}
	if _, err := os.Stat(filepath.Join(dstDir, "README.md")); !os.IsNotExist(err) {
		t.Errorf("README.md: want not copied, stat err = %v", err)
	}
}

func TestSyncTFFiles_NoLockFile_NoOpNoError(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(srcDir, "main.tf"), []byte("# main"), 0o644); err != nil {
		t.Fatalf("write main.tf: %v", err)
	}

	if err := syncTFFiles(srcDir, dstDir); err != nil {
		t.Fatalf("syncTFFiles: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dstDir, ".terraform.lock.hcl")); !os.IsNotExist(err) {
		t.Errorf(".terraform.lock.hcl: want absent, stat err = %v", err)
	}
}

func TestPrunePluginCache_EmptyCacheDir_NoOp(t *testing.T) {
	cacheDir := t.TempDir()
	var out bytes.Buffer
	prunePluginCache(&out, cacheDir) // must not panic or error on an empty/fresh cache
	if out.Len() != 0 {
		t.Errorf("prunePluginCache on empty dir: want no warnings, got %q", out.String())
	}
}
