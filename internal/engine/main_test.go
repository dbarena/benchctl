package engine_test

import (
	"fmt"
	"os"
	"testing"
)

// TestMain runs this package's tests from a scratch directory.
//
// Runner.Run and Runner.Resume write engine.StepWindowsFilename into the
// process working directory, the same place the stdout collector writes
// results_*.json and the same place `benchctl fetch` looks for both on the
// driver instance. Around forty tests here drive a run, so without this every
// one of them would drop that file into the source tree.
//
// Tests that assert on the file's contents still take their own t.Chdir into a
// t.TempDir, so they cannot see each other's windows.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "benchctl-engine-test")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create scratch dir: %v\n", err)
		os.Exit(1)
	}
	if err := os.Chdir(dir); err != nil {
		fmt.Fprintf(os.Stderr, "chdir to scratch dir: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	// os.Exit skips deferred cleanup, so tidy up first. Step out of the
	// directory before removing it.
	_ = os.Chdir("/")
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
