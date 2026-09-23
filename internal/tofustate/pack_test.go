package tofustate_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dbarena/benchctl/internal/tofustate"
)

func TestPackUnpack_RoundTrip(t *testing.T) {
	src := t.TempDir()
	must(t, os.WriteFile(filepath.Join(src, "terraform.tfstate"), []byte(`{"version":4}`), 0o644))
	must(t, os.WriteFile(filepath.Join(src, "main.tf"), []byte(`resource "aws_instance" "x" {}`), 0o644))
	must(t, os.WriteFile(filepath.Join(src, "terraform.tfvars.json"), []byte(`{"instance_type":"t3.micro"}`), 0o644))

	// .terraform/ must be excluded from the blob.
	must(t, os.MkdirAll(filepath.Join(src, ".terraform", "providers"), 0o755))
	must(t, os.WriteFile(filepath.Join(src, ".terraform", "providers", "terraform-provider-aws"), []byte("binary"), 0o755))

	blob, err := tofustate.Pack(map[string]string{"target": src})
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if blob == "" {
		t.Fatal("Pack returned empty blob")
	}

	restored, err := tofustate.Unpack(blob)
	if err != nil {
		t.Fatalf("Unpack: %v", err)
	}

	dir, ok := restored["target"]
	if !ok {
		t.Fatal("Unpack: missing 'target' component")
	}

	// .terraform/ must NOT be restored.
	if _, err := os.Stat(filepath.Join(dir, ".terraform")); !os.IsNotExist(err) {
		t.Error(".terraform/ should be excluded from the blob")
	}

	checkFile(t, filepath.Join(dir, "terraform.tfstate"), `{"version":4}`)
	checkFile(t, filepath.Join(dir, "main.tf"), `resource "aws_instance" "x" {}`)
	checkFile(t, filepath.Join(dir, "terraform.tfvars.json"), `{"instance_type":"t3.micro"}`)
}

func TestPackUnpack_MultipleComponents(t *testing.T) {
	targetSrc := t.TempDir()
	driverSrc := t.TempDir()
	must(t, os.WriteFile(filepath.Join(targetSrc, "target.tfstate"), []byte("target-state"), 0o644))
	must(t, os.WriteFile(filepath.Join(driverSrc, "driver.tfstate"), []byte("driver-state"), 0o644))

	blob, err := tofustate.Pack(map[string]string{"target": targetSrc, "driver": driverSrc})
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}

	restored, err := tofustate.Unpack(blob)
	if err != nil {
		t.Fatalf("Unpack: %v", err)
	}

	if len(restored) != 2 {
		t.Fatalf("expected 2 components, got %d", len(restored))
	}
	checkFile(t, filepath.Join(restored["target"], "target.tfstate"), "target-state")
	checkFile(t, filepath.Join(restored["driver"], "driver.tfstate"), "driver-state")
}

func TestPackUnpack_EmptyDir(t *testing.T) {
	src := t.TempDir()
	blob, err := tofustate.Pack(map[string]string{"target": src})
	if err != nil {
		t.Fatalf("Pack empty dir: %v", err)
	}
	restored, err := tofustate.Unpack(blob)
	if err != nil {
		t.Fatalf("Unpack empty: %v", err)
	}
	// Component is absent when no files were packed.
	if _, ok := restored["target"]; ok {
		t.Error("expected no 'target' component for empty dir")
	}
}

func TestUnpack_InvalidBlob(t *testing.T) {
	_, err := tofustate.Unpack("not-valid-base64!!!")
	if err == nil {
		t.Fatal("expected error for invalid blob")
	}
}

// ---- helpers ----

func checkFile(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	if string(data) != want {
		t.Errorf("%s = %q, want %q", filepath.Base(path), data, want)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
