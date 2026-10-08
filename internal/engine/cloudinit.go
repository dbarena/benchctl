package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// cloudInitExitDegraded is the exit code `cloud-init status` uses for a boot
// that finished with recoverable warnings: a deprecated config key, a
// non-fatal module warning. The instance is up and usable.
//
// Ubuntu 24.04 ships cloud-init 24.x or later, which reports this routinely,
// so treating every non-zero exit as fatal aborts provisioning on perfectly
// healthy instances. Exit code 1 is the real failure.
const cloudInitExitDegraded = 2

// cloudInitDiagnosticsTimeout bounds the follow-up SSH call that collects the
// reason for a failure. Short on purpose: the run is already failing and the
// only job left is to explain why.
const cloudInitDiagnosticsTimeout = 30 * time.Second

// cloudInitOutputLogTail is how much of cloud-init's console log to pull back
// on failure. The user-data script runs with `set -x`, so the last few dozen
// lines are the commands leading up to the failure.
const cloudInitOutputLogTail = 40

// sshExitConnectionFailed is the exit code ssh reserves for its own failures,
// as opposed to the remote command's exit code. During early boot, sshd can
// accept a TCP connection and then reset it while cloud-init regenerates host
// keys, so the first `status --wait` call can fail this way on a healthy
// instance.
const sshExitConnectionFailed = 255

// cloudInitSSHAttempts bounds how often the wait reconnects after ssh itself
// fails. Bounded so that a persistent problem, such as a wrong key, surfaces
// as an ssh failure within a minute rather than as a timeout much later.
const cloudInitSSHAttempts = 12

// cloudInitSSHRetryInterval is the pause between those attempts. A variable so
// that tests can shorten it.
var cloudInitSSHRetryInterval = 5 * time.Second

// WaitForCloudInit blocks until cloud-init finishes on host and reports
// whether the instance came up usable.
//
// On failure it SSHes back in for `cloud-init status --long` and the tail of
// cloud-init's console log, streams both to out, and folds cloud-init's own
// error list into the returned error. Without that, a failed bootstrap
// persists in run state as a bare "exit status 1", and the only way to learn
// anything is to SSH into an instance teardown is about to destroy.
func WaitForCloudInit(ctx context.Context, out io.Writer, keyPath, user, host string, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	err := runCloudInitWait(waitCtx, out, keyPath, user, host)
	timedOut := errors.Is(waitCtx.Err(), context.DeadlineExceeded)
	cancel()

	switch {
	case err == nil:
		return nil
	case timedOut:
		// Whatever ssh exited with here describes its own cancellation, not
		// cloud-init, so reporting it would mislead.
		return fmt.Errorf("cloud-init on %s did not finish within %s", host, timeout)
	case sshExitCode(err) == cloudInitExitDegraded:
		if out != nil {
			fmt.Fprintf(out, "  cloud-init: finished with recoverable warnings on %s; continuing\n", host)
		}
		return nil
	}

	if detail := cloudInitFailureDetail(ctx, out, keyPath, user, host); detail != "" {
		return fmt.Errorf("cloud-init failed on %s: %s: %w", host, detail, err)
	}
	return fmt.Errorf("cloud-init failed on %s: %w", host, err)
}

// runCloudInitWait runs `cloud-init status --wait` on host and reconnects when
// ssh itself fails, up to cloudInitSSHAttempts times. It returns the error of
// the last attempt.
func runCloudInitWait(ctx context.Context, out io.Writer, keyPath, user, host string) error {
	var err error
	for attempt := 1; attempt <= cloudInitSSHAttempts; attempt++ {
		err = RunSSH(ctx, out, keyPath, user, host, "cloud-init status --wait")
		if sshExitCode(err) != sshExitConnectionFailed || attempt == cloudInitSSHAttempts {
			return err
		}
		if out != nil {
			fmt.Fprintf(out, "  cloud-init: ssh connection to %s failed; retrying (attempt %d of %d)\n", host, attempt+1, cloudInitSSHAttempts)
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(cloudInitSSHRetryInterval):
		}
	}
	return err
}

// cloudInitFailureDetail prints why cloud-init failed and returns a one-line
// summary for the error message.
//
// Best effort by design: it returns "" rather than an error, because the
// caller already has a better error to report and a second SSH failure should
// not replace it.
func cloudInitFailureDetail(ctx context.Context, out io.Writer, keyPath, user, host string) string {
	if ctx.Err() != nil {
		return ""
	}
	diagCtx, cancel := context.WithTimeout(ctx, cloudInitDiagnosticsTimeout)
	defer cancel()

	// Every part tolerates its own failure: a truncated report is still worth
	// having, and the log needs sudo on some images but not others.
	cmd := fmt.Sprintf(
		"cloud-init status --long 2>&1 || true; "+
			"echo '--- last %d lines of /var/log/cloud-init-output.log ---'; "+
			"sudo tail -n %d /var/log/cloud-init-output.log 2>&1 || true",
		cloudInitOutputLogTail, cloudInitOutputLogTail)

	raw, err := RunSSHCapture(diagCtx, keyPath, user, host, cmd)
	if err != nil {
		return ""
	}
	if out != nil && raw != "" {
		fmt.Fprintf(out, "  cloud-init: failure detail from %s:\n%s\n", host, raw)
	}
	return summarizeCloudInitErrors(raw)
}

// summarizeCloudInitErrors pulls cloud-init's own error list out of
// `cloud-init status --long` output and joins it into one line.
//
// The block it reads looks like:
//
//	errors:
//		- ('scripts_user', RuntimeError('Runparts: 1 failures (part-001) in 1 attempted commands'))
//	recoverable_errors:
//
// Only `errors:` is taken. Entries under `recoverable_errors:` are warnings,
// and on a failed boot they are usually restatements of the same failure.
// Returns "" when there is no error list to report, leaving the caller to fall
// back to the raw exit status.
func summarizeCloudInitErrors(statusOutput string) string {
	var items []string
	inErrors := false
	for _, line := range strings.Split(statusOutput, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "errors:":
			inErrors = true
		case !inErrors:
			continue
		case strings.HasPrefix(trimmed, "-"):
			if item := strings.TrimSpace(strings.TrimPrefix(trimmed, "-")); item != "" {
				items = append(items, item)
			}
		default:
			// Any other unindented line closes the block, e.g.
			// "recoverable_errors:".
			inErrors = false
		}
	}
	return strings.Join(items, "; ")
}

// sshExitCode returns the exit status a finished remote command produced, or
// -1 when err came from something other than a completed process. ssh
// propagates the remote command's exit code, reserving 255 for its own
// failures.
func sshExitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
