package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dbarena/benchctl/internal/engine"
	"github.com/dbarena/benchctl/internal/runstate"
	"github.com/dbarena/benchctl/internal/tofustate"
)

func TestPatchOutputPaths(t *testing.T) {
	oldDir := "/ci/tofu-state/run-abc/loaddriver"
	newDir := "/tmp/benchctl-tofu-XYZ/driver"

	outputs := map[string]string{
		"_tofu_work_dir":       oldDir,
		"ssh_private_key_path": oldDir + "/id_rsa",
		"public_ip":            "1.2.3.4",
		"other_path":           "/some/unrelated/path",
	}

	patchOutputPaths(outputs, oldDir, newDir)

	if got := outputs["_tofu_work_dir"]; got != newDir {
		t.Errorf("_tofu_work_dir: got %q, want %q", got, newDir)
	}
	if got := outputs["ssh_private_key_path"]; got != newDir+"/id_rsa" {
		t.Errorf("ssh_private_key_path: got %q, want %q", got, newDir+"/id_rsa")
	}
	if got := outputs["public_ip"]; got != "1.2.3.4" {
		t.Errorf("public_ip should be unchanged: got %q", got)
	}
	if got := outputs["other_path"]; got != "/some/unrelated/path" {
		t.Errorf("other_path should be unchanged: got %q", got)
	}
}

func TestPatchOutputPaths_NoMatchLeavesUntouched(t *testing.T) {
	oldDir := "/ci/tofu-state/run-abc/target"
	newDir := "/tmp/benchctl-tofu-XYZ/target"

	outputs := map[string]string{
		"public_ip": "10.0.0.1",
	}

	patchOutputPaths(outputs, oldDir, newDir)

	if got := outputs["public_ip"]; got != "10.0.0.1" {
		t.Errorf("public_ip should be unchanged: got %q", got)
	}
}

func TestRestoreTofuWorkDirs_RestoresTheRunFilesDir(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "manifest.jsonl"), []byte("m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	blob, err := tofustate.Pack(map[string]string{engine.TargetFilesComponent: src})
	if err != nil {
		t.Fatal(err)
	}
	// The machine that ran this never had the directory.
	gone := filepath.Join(t.TempDir(), "elsewhere")
	state := &runstate.State{
		TofuState: blob,
		TargetOutputs: map[string]string{
			engine.OutputKeyRunFilesDir: gone,
			"projects_manifest":         filepath.Join(gone, "manifest.jsonl"),
		},
	}
	if err := restoreTofuWorkDirs(state, false); err != nil {
		t.Fatalf("restoreTofuWorkDirs: %v", err)
	}
	got, err := os.ReadFile(state.TargetOutputs["projects_manifest"])
	if err != nil || string(got) != "m\n" {
		t.Errorf("projects_manifest = %q -> %q, %v; want the restored copy", state.TargetOutputs["projects_manifest"], got, err)
	}
	if state.TargetOutputs[engine.OutputKeyRunFilesDir] == gone {
		t.Error("the run files directory still points at the missing one")
	}
}

func TestRestoreTofuWorkDirs_NoBlobLeavesTheRunFilesDirAlone(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "elsewhere")
	state := &runstate.State{TargetOutputs: map[string]string{engine.OutputKeyRunFilesDir: gone}}
	if err := restoreTofuWorkDirs(state, false); err != nil {
		t.Fatalf("restoreTofuWorkDirs: %v", err)
	}
	if state.TargetOutputs[engine.OutputKeyRunFilesDir] != gone {
		t.Error("the path changed with nothing to restore from")
	}
}
