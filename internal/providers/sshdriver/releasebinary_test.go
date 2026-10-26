package sshdriver

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestFetchReleaseBinaryCmd_VerifiesAndInstalls runs the remote install script
// locally against a fake curl that serves release assets from a directory. It
// guards against the downloaded file name drifting from the name listed in
// checksums.txt, which makes sha256sum -c fail.
func TestFetchReleaseBinaryCmd_VerifiesAndInstalls(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install script requires a POSIX shell")
	}
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("sha256sum not available")
	}

	assets := t.TempDir()
	binary := []byte("fake benchctl binary\n")
	if err := os.WriteFile(filepath.Join(assets, "benchctl-linux-arm64"), binary, 0o644); err != nil {
		t.Fatal(err)
	}
	checksums := fmt.Sprintf("%x  benchctl-linux-amd64\n%x  benchctl-linux-arm64\n", sha256.Sum256([]byte("other")), sha256.Sum256(binary))
	if err := os.WriteFile(filepath.Join(assets, "checksums.txt"), []byte(checksums), 0o644); err != nil {
		t.Fatal(err)
	}

	// The fake curl copies the asset named by the URL's last path segment to
	// the file given by -o.
	binDir := t.TempDir()
	fakeCurl := fmt.Sprintf(`#!/bin/sh
out=""
while [ $# -gt 1 ]; do
  if [ "$1" = "-o" ]; then out="$2"; shift; fi
  shift
done
cp %q/"$(basename "$1")" "$out"
`, assets)
	if err := os.WriteFile(filepath.Join(binDir, "curl"), []byte(fakeCurl), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	dest := filepath.Join(t.TempDir(), "benchctl")
	out, err := exec.Command("bash", "-c", fetchReleaseBinaryCmd("arm64", dest)).CombinedOutput()
	if err != nil {
		t.Fatalf("install script failed: %v\n%s", err, out)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	if string(got) != string(binary) {
		t.Errorf("installed binary = %q, want %q", got, binary)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o100 == 0 {
		t.Errorf("installed binary is not executable: %v", info.Mode())
	}
}
