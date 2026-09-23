package engine

import (
	"context"
	"fmt"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"time"
)

// RunSSH runs cmd on user@host via the system ssh binary, streaming stdout and
// stderr to out.
func RunSSH(ctx context.Context, out io.Writer, keyPath, user, host, cmd string) error {
	return runSSH(ctx, out, keyPath, user, host, cmd, false)
}

// RunSSHIgnoreDisconnect is like RunSSH but treats SSH exit-code 255 (remote
// closed the connection) as success. Use for commands that intentionally
// terminate the session, such as reboot.
func RunSSHIgnoreDisconnect(ctx context.Context, out io.Writer, keyPath, user, host, cmd string) error {
	return runSSH(ctx, out, keyPath, user, host, cmd, true)
}

// RunSSHCapture runs cmd on user@host via SSH and returns the combined
// stdout+stderr as a string. Output is not written to any log writer; use
// this for machine-readable remote reads (e.g. /proc/diskstats).
func RunSSHCapture(ctx context.Context, keyPath, user, host, cmd string) (string, error) {
	args := append(hostKeyArgs(keyPath), "-o", "BatchMode=yes")
	if keyPath != "" {
		args = append(args, "-i", keyPath)
	}
	args = append(args, user+"@"+host, cmd)
	c := exec.CommandContext(ctx, "ssh", args...)
	out, err := c.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ssh %s@%s %q: %w", user, host, cmd, err)
	}
	return string(out), nil
}

func runSSH(ctx context.Context, out io.Writer, keyPath, user, host, cmd string, ignoreDisconnect bool) error {
	args := append(hostKeyArgs(keyPath), "-o", "BatchMode=yes")
	if keyPath != "" {
		args = append(args, "-i", keyPath)
	}
	args = append(args, user+"@"+host, cmd)
	c := exec.CommandContext(ctx, "ssh", args...)
	c.Stdout = out
	c.Stderr = out
	err := c.Run()
	if err == nil {
		return nil
	}
	if ignoreDisconnect {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 255 {
			return nil
		}
	}
	return err
}

// ScpDir copies localDir to userAtHost:remoteDst via scp -r.
func ScpDir(ctx context.Context, out io.Writer, keyPath, localDir, userAtHost, remoteDst string) error {
	args := append([]string{"-r"}, hostKeyArgs(keyPath)...)
	args = append(args, "-o", "BatchMode=yes")
	if keyPath != "" {
		args = append(args, "-i", keyPath)
	}
	args = append(args, localDir, userAtHost+":"+remoteDst)
	c := exec.CommandContext(ctx, "scp", args...)
	c.Stdout = out
	c.Stderr = out
	return c.Run()
}

// hostKeyArgs returns the ssh/scp host-key-verification flags to use for a
// connection whose private key lives at keyPath. When keyPath points into a
// per-run working directory (the normal case), host keys are checked against
// a known_hosts file colocated in that same directory rather than the user's
// ~/.ssh/known_hosts. This avoids spurious "REMOTE HOST IDENTIFICATION HAS
// CHANGED" failures when a cloud provider reassigns an IP that was previously
// used by a different instance (and whose old host key is still in the
// user's global known_hosts). The file starts empty each run, so the first
// connection to a host is trusted automatically (StrictHostKeyChecking=
// accept-new), while a key change mid-run still fails closed.
func hostKeyArgs(keyPath string) []string {
	if keyPath == "" {
		return []string{"-o", "StrictHostKeyChecking=no"}
	}
	knownHosts := filepath.Join(filepath.Dir(keyPath), "known_hosts")
	return []string{
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + knownHosts,
	}
}

// WaitForSSHDown polls until TCP port 22 on host stops accepting connections,
// or the context / timeout expires. Used after a reboot command to confirm the
// machine has actually started shutting down before WaitForSSH begins the
// up-again poll. Returns nil on both success and timeout, so a slow shutdown
// does not abort the run; WaitForSSH handles what follows either way.
func WaitForSSHDown(ctx context.Context, out io.Writer, host string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if out != nil {
		fmt.Fprintf(out, "  ssh: waiting for %s to stop accepting connections (timeout: %s)\n", host, timeout)
	}
	addr := net.JoinHostPort(host, "22")
	for {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			return nil
		}
		conn.Close()
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

// WaitForSSH polls TCP port 22 on host until a connection is accepted or the
// context / timeout expires. It logs progress to out.
func WaitForSSH(ctx context.Context, out io.Writer, host string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if out != nil {
		fmt.Fprintf(out, "  ssh: waiting for %s to accept connections (timeout: %s)\n", host, timeout)
	}
	addr := net.JoinHostPort(host, "22")
	for {
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("SSH not ready on %s after %s: %w", host, timeout, err)
		case <-time.After(5 * time.Second):
		}
	}
}
