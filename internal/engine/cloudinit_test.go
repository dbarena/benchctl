package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// realStatusLong is verbatim `cloud-init status --long` output from the
// instance in run postgres-tpcc-ec2-20260929-090321-472850, whose user-data
// script died because the instance type had no local NVMe. Using the real
// thing keeps the parser honest about tabs, quoting, and the trailing
// recoverable_errors block.
const realStatusLong = `status: error
extended_status: error - done
boot_status_code: enabled-by-generator
last_update: Thu, 01 Jan 1970 00:00:19 +0000
detail: DataSourceEc2Local
errors:
	- ('scripts_user', RuntimeError('Runparts: 1 failures (part-001) in 1 attempted commands'))
recoverable_errors:
WARNING:
	- Failed to run module scripts_user (scripts in /var/lib/cloud/instance/scripts)
	- Running module scripts_user (<module 'cloudinit.config.cc_scripts_user'>) failed
`

func TestSummarizeCloudInitErrors(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "real failure keeps only the errors block",
			in:   realStatusLong,
			want: "('scripts_user', RuntimeError('Runparts: 1 failures (part-001) in 1 attempted commands'))",
		},
		{
			name: "multiple errors are joined",
			in:   "status: error\nerrors:\n\t- first thing\n\t- second thing\nrecoverable_errors:\n",
			want: "first thing; second thing",
		},
		{
			name: "no errors block",
			in:   "status: done\nboot_status_code: enabled-by-generator\n",
			want: "",
		},
		{
			name: "empty errors block falls back to the exit status",
			in:   "status: error\nerrors:\nrecoverable_errors:\n",
			want: "",
		},
		{
			// A degraded boot reports only recoverable_errors. Those are
			// warnings, so there is nothing to put in an error message.
			name: "recoverable errors alone are not reported",
			in:   "status: degraded done\nerrors:\nrecoverable_errors:\nWARNING:\n\t- deprecated key\n",
			want: "",
		},
		{
			name: "garbage in, nothing out",
			in:   "",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := summarizeCloudInitErrors(tt.in); got != tt.want {
				t.Errorf("got %q\nwant %q", got, tt.want)
			}
		})
	}
}

// installFakeSSH puts an executable named "ssh" at the front of $PATH so
// WaitForCloudInit can be tested without a host. The script branches on the
// remote command, which is always ssh's last argument: the `status --wait`
// call exits with $FAKE_SSH_WAIT_EXIT, and the follow-up diagnostics call
// prints $FAKE_SSH_DIAG. Mirrors installFakeTofu in the opentofu provider
// tests.
func installFakeSSH(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake ssh script requires a POSIX shell")
	}
	dir := t.TempDir()
	script := `#!/bin/sh
for a in "$@"; do last="$a"; done
case "$last" in
  *"status --wait"*)
    if [ -n "$FAKE_SSH_WAIT_SLEEP" ]; then sleep "$FAKE_SSH_WAIT_SLEEP"; fi
    echo "status: error"
    exit "${FAKE_SSH_WAIT_EXIT:-0}"
    ;;
  *"--long"*)
    printf '%s' "$FAKE_SSH_DIAG"
    exit 0
    ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ssh: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestWaitForCloudInit_Success(t *testing.T) {
	installFakeSSH(t)
	t.Setenv("FAKE_SSH_WAIT_EXIT", "0")

	var out bytes.Buffer
	if err := WaitForCloudInit(context.Background(), &out, "", "ubuntu", "10.0.0.1", time.Minute); err != nil {
		t.Fatalf("WaitForCloudInit: %v", err)
	}
}

// TestWaitForCloudInit_DegradedIsNotAFailure is the regression guard for the
// case that silently breaks healthy runs: cloud-init 24.x and later exit 2
// when a boot completes with recoverable warnings, and treating that as fatal
// aborts provisioning on an instance that is up and usable.
func TestWaitForCloudInit_DegradedIsNotAFailure(t *testing.T) {
	installFakeSSH(t)
	t.Setenv("FAKE_SSH_WAIT_EXIT", "2")

	var out bytes.Buffer
	if err := WaitForCloudInit(context.Background(), &out, "", "ubuntu", "10.0.0.1", time.Minute); err != nil {
		t.Fatalf("exit code 2 treated as failure: %v", err)
	}
	if !strings.Contains(out.String(), "recoverable warnings") {
		t.Errorf("degraded boot was not reported to the operator; output: %q", out.String())
	}
}

// TestWaitForCloudInit_FailureExplainsItself covers the gap that cost a
// debugging round trip: a failed bootstrap used to persist in run state as
// "exit status 1" with no hint of which module failed.
func TestWaitForCloudInit_FailureExplainsItself(t *testing.T) {
	installFakeSSH(t)
	t.Setenv("FAKE_SSH_WAIT_EXIT", "1")
	t.Setenv("FAKE_SSH_DIAG", realStatusLong+"--- last 40 lines ---\n+ NVME_DEV=\n")

	var out bytes.Buffer
	err := WaitForCloudInit(context.Background(), &out, "", "ubuntu", "10.0.0.1", time.Minute)
	if err == nil {
		t.Fatal("WaitForCloudInit succeeded on exit status 1")
	}
	// The error is what lands in run state and what `benchctl status` shows,
	// so the failing module has to be in there, not just the exit code.
	if !strings.Contains(err.Error(), "scripts_user") {
		t.Errorf("error does not name the failing cloud-init module: %v", err)
	}
	if !strings.Contains(err.Error(), "10.0.0.1") {
		t.Errorf("error does not name the host: %v", err)
	}
	// The console gets the full dump, including the shell trace that shows
	// which command died.
	if !strings.Contains(out.String(), "+ NVME_DEV=") {
		t.Errorf("console output is missing the cloud-init log tail; got: %q", out.String())
	}
}

// TestWaitForCloudInit_FailureWithoutDetailStillErrors covers an instance that
// cannot be reached for diagnostics, or one whose status output has no error
// list. The exit status alone must still fail the run.
func TestWaitForCloudInit_FailureWithoutDetailStillErrors(t *testing.T) {
	installFakeSSH(t)
	t.Setenv("FAKE_SSH_WAIT_EXIT", "1")
	t.Setenv("FAKE_SSH_DIAG", "status: error\n")

	err := WaitForCloudInit(context.Background(), nil, "", "ubuntu", "10.0.0.1", time.Minute)
	if err == nil {
		t.Fatal("WaitForCloudInit succeeded on exit status 1")
	}
	if !strings.Contains(err.Error(), "cloud-init failed") {
		t.Errorf("unexpected error text: %v", err)
	}
}

// TestWaitForCloudInit_TimeoutIsDistinguished guards the message that sent the
// last investigation down the wrong path: a run that failed in 42 seconds
// reported "cloud-init wait (exceeded 15m0s?)". A timeout and a failure are
// different problems and must read differently.
func TestWaitForCloudInit_TimeoutIsDistinguished(t *testing.T) {
	installFakeSSH(t)
	t.Setenv("FAKE_SSH_WAIT_SLEEP", "10")

	err := WaitForCloudInit(context.Background(), nil, "", "ubuntu", "10.0.0.1", 100*time.Millisecond)
	if err == nil {
		t.Fatal("WaitForCloudInit succeeded despite the timeout")
	}
	if !strings.Contains(err.Error(), "did not finish within 100ms") {
		t.Errorf("timeout not reported as a timeout: %v", err)
	}
	if strings.Contains(err.Error(), "exit status") {
		t.Errorf("timeout error reports an exit status, which describes ssh's cancellation rather than cloud-init: %v", err)
	}
}
