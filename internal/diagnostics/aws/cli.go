// Package aws collects provider-side diagnostics for AWS targets: Performance
// Insights, CloudWatch metrics, exported Postgres logs, and configuration.
//
// It shells out to the `aws` CLI rather than linking the SDK, as the rest of
// benchctl does (docs/concepts.md). Credentials come from the caller's
// environment, which is why collection runs orchestrator-side: the
// load-driver instance has no IAM role.
package aws

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// cmdRunner runs the aws CLI and returns its stdout. Injected so tests can
// replay captured responses without credentials, as in
// internal/providers/supabase.
type cmdRunner func(ctx context.Context, args ...string) ([]byte, error)

// execAWS is the real runner. stderr is folded into the error: the CLI puts
// the actionable part of a failure there, not in the exit status.
func execAWS(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "aws", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// The CLI pages to a terminal by default, hanging an unattended fetch.
	cmd.Env = append(cmd.Environ(), "AWS_PAGER=")
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s", err, msg)
	}
	return stdout.Bytes(), nil
}

// run invokes the CLI with the collector's region appended, returning the
// output and the argv that produced it for the diagnostics index.
func (c *Collector) run(ctx context.Context, args ...string) (out []byte, argv string, err error) {
	// Copy rather than append in place: callers build args over a shared
	// prefix (piArgs), so growing their slice here would alias.
	full := make([]string, 0, len(args)+4)
	full = append(full, args...)
	full = append(full, "--region", c.region, "--output", "json")
	out, err = c.exec(ctx, full...)
	return out, shellArgs(append([]string{"aws"}, full...)), err
}

// shellArgs renders an argv as a copy-pasteable command line. The JSON
// payloads are full of quotes and braces, so an unquoted record of them would
// not be reproducible.
func shellArgs(argv []string) string {
	quoted := make([]string, 0, len(argv))
	for _, a := range argv {
		if a != "" && !strings.ContainsAny(a, " \t\n\"'$`\\{}[]|&;<>()*?!#~") {
			quoted = append(quoted, a)
			continue
		}
		quoted = append(quoted, "'"+strings.ReplaceAll(a, "'", `'\''`)+"'")
	}
	return strings.Join(quoted, " ")
}
