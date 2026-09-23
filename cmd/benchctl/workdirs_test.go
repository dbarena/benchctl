package main

import (
	"testing"
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
