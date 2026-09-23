package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRootCmd_ErrorPrintedToStderr guards against SilenceErrors being set on
// rootCmd. If it were, errors would be returned from Execute() but never
// printed, silently swallowing all command failures.
func TestRootCmd_ErrorPrintedToStderr(t *testing.T) {
	var buf bytes.Buffer
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs([]string{"validate", "nonexistent.yaml"})
	t.Cleanup(func() {
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})

	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error from nonexistent scenario file")
	}
	if !strings.Contains(buf.String(), err.Error()) {
		t.Errorf("stderr output %q does not contain error message %q; SilenceErrors may be set on rootCmd", buf.String(), err.Error())
	}
}
