// Package pgbench implements a WorkloadAdapter for PostgreSQL's pgbench.
// See https://www.postgresql.org/docs/current/pgbench.html and
// docs/pgbench.md.
//
// A custom script comes inline as args.script (schema.PgbenchArgScript); the
// adapter writes it to a scratch file and passes that with --file, so a
// scenario needs no file of its own on the driver instance.
package pgbench

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"github.com/dbarena/benchctl/internal/engine"
	"github.com/dbarena/benchctl/internal/schema"
)

// reservedArgs are pgbench options the adapter must own. The connection comes
// from the target's outputs, and the command picks between initializing and
// running.
var reservedArgs = []string{"host", "port", "username", "dbname", "initialize"}

// cmdRunner executes a command with env added to the environment, tees its
// combined output to out (if non-nil), and returns a bounded tail of the
// output: enough for pgbench's closing summary and for error reporting.
type cmdRunner func(ctx context.Context, out io.Writer, env []string, name string, args ...string) ([]byte, error)

// toolEnsurer resolves the path to the pgbench binary, failing if it's missing.
type toolEnsurer func() (string, error)

// Adapter implements engine.WorkloadAdapter for pgbench.
type Adapter struct {
	run     cmdRunner
	out     io.Writer
	ensurer toolEnsurer
}

// New returns an Adapter that streams pgbench output to stdout.
func New() *Adapter {
	return &Adapter{run: execRun, out: os.Stdout, ensurer: ensurePgbench}
}

// newWithRunner returns an Adapter with an injected runner and no output
// streaming, for testing. The ensurer is stubbed to return "pgbench".
func newWithRunner(r cmdRunner) *Adapter {
	return &Adapter{
		run:     r,
		out:     io.Discard,
		ensurer: func() (string, error) { return "pgbench", nil },
	}
}

// Run executes the step. "init" returns empty Metrics; "run" returns the
// summary pgbench prints at the end, structured under engine.StructuredKey,
// and its per-interval --progress reports as metrics["raw_samples_csv"].
func (a *Adapter) Run(ctx context.Context, outputs engine.Outputs, step schema.SuiteStep) (engine.Metrics, error) {
	switch step.Command {
	case schema.PgbenchCommandInit:
		return a.initialize(ctx, outputs, step)
	case schema.PgbenchCommandRun:
		return a.measure(ctx, outputs, step)
	default:
		return nil, fmt.Errorf("pgbench step %q: unknown command %q (want %q or %q)",
			step.Name, step.Command, schema.PgbenchCommandInit, schema.PgbenchCommandRun)
	}
}

func (a *Adapter) initialize(ctx context.Context, outputs engine.Outputs, step schema.SuiteStep) (engine.Metrics, error) {
	if _, ok := step.Args[schema.PgbenchArgScript]; ok {
		return nil, fmt.Errorf("pgbench init: step %q: args.%s only applies to command %q",
			step.Name, schema.PgbenchArgScript, schema.PgbenchCommandRun)
	}
	bin, env, args, err := a.prepare(outputs, step)
	if err != nil {
		return nil, fmt.Errorf("pgbench init: %w", err)
	}
	args = append([]string{"--initialize"}, args...)
	if out, err := a.run(ctx, a.out, env, bin, args...); err != nil {
		return nil, fmt.Errorf("pgbench init: %w\noutput: %s", err, out)
	}
	return engine.Metrics{}, nil
}

func (a *Adapter) measure(ctx context.Context, outputs engine.Outputs, step schema.SuiteStep) (engine.Metrics, error) {
	bin, env, args, err := a.prepare(outputs, step)
	if err != nil {
		return nil, fmt.Errorf("pgbench run: %w", err)
	}

	if script := step.Args[schema.PgbenchArgScript]; script != "" {
		path, err := writeScript(script)
		if err != nil {
			return nil, fmt.Errorf("pgbench run: %w", err)
		}
		defer removeScratchFile(a.out, path)
		args = append(args, "--file="+path)
	}
	// Per-interval reports are what raw_samples_csv is made of; a scenario
	// may still pick its own interval.
	if _, ok := step.Args["progress"]; !ok {
		args = append(args, "--progress=1")
	}

	progress := &progressWriter{}
	out, err := a.run(ctx, io.MultiWriter(a.out, progress), env, bin, args...)
	if err != nil {
		return nil, fmt.Errorf("pgbench run: %w\noutput: %s", err, out)
	}

	sum, err := parseSummary(out)
	if err != nil {
		return nil, fmt.Errorf("pgbench run: %w\noutput: %s", err, out)
	}
	metrics := engine.Metrics{engine.StructuredKey: sum.structured()}
	if csv := progress.csv(); csv != "" {
		metrics["raw_samples_csv"] = csv
	}
	return metrics, nil
}

// prepare resolves the binary and builds the connection environment and the
// pass-through options shared by both commands.
func (a *Adapter) prepare(outputs engine.Outputs, step schema.SuiteStep) (bin string, env, args []string, err error) {
	env, err = connEnv(outputs)
	if err != nil {
		return "", nil, nil, err
	}
	args, err = buildArgs(step)
	if err != nil {
		return "", nil, nil, err
	}
	bin, err = a.ensurer()
	if err != nil {
		return "", nil, nil, err
	}
	return bin, env, args, nil
}

// connEnv returns the libpq environment variables that point pgbench at the
// target. Passing them through the environment keeps the password off the
// command line, where any user of the driver instance could read it.
func connEnv(outputs engine.Outputs) ([]string, error) {
	required := []string{"host", "port", "user", "password", "db"}
	var missing []string
	for _, k := range required {
		if outputs[k] == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("target connection params missing: %v (declare them in the scenario's service vars)", missing)
	}
	sslmode := outputs["sslmode"]
	if sslmode == "" {
		sslmode = "disable"
	}
	env := []string{
		"PGHOST=" + outputs["host"],
		"PGPORT=" + outputs["port"],
		"PGUSER=" + outputs["user"],
		"PGPASSWORD=" + outputs["password"],
		"PGDATABASE=" + outputs["db"],
		"PGSSLMODE=" + sslmode,
		"PGAPPNAME=benchctl-pgbench",
	}
	if v := outputs["sslnegotiation"]; v != "" {
		env = append(env, "PGSSLNEGOTIATION="+v)
	}
	return env, nil
}

// buildArgs turns step.Args, minus the adapter's own args.script, into pgbench
// long options in sorted order: an empty value emits a bare flag
// (no-vacuum: "" → --no-vacuum), anything else --key=value.
func buildArgs(step schema.SuiteStep) ([]string, error) {
	for _, reserved := range reservedArgs {
		if _, ok := step.Args[reserved]; ok {
			return nil, fmt.Errorf("step %q: %q is managed by the pgbench adapter and must not be set in scenario args",
				step.Name, reserved)
		}
	}
	keys := make([]string, 0, len(step.Args))
	for k := range step.Args {
		if k != schema.PgbenchArgScript {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	args := make([]string, 0, len(keys))
	for _, k := range keys {
		if v := step.Args[k]; v == "" {
			args = append(args, "--"+k)
		} else {
			args = append(args, "--"+k+"="+v)
		}
	}
	return args, nil
}

// writeScript writes a custom script to a scratch file for --file.
func writeScript(script string) (string, error) {
	f, err := os.CreateTemp("", "benchctl-pgbench-*.sql")
	if err != nil {
		return "", fmt.Errorf("create script file: %w", err)
	}
	if _, err := f.WriteString(script); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("write script file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("write script file: %w", err)
	}
	return f.Name(), nil
}

func removeScratchFile(out io.Writer, path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(out, "warning: could not remove scratch file %s: %v\n", path, err)
	}
}

var versionRe = regexp.MustCompile(`\(PostgreSQL\)\s+(\S+)`)

// VersionInfo runs "pgbench --version". Best-effort: a failure logs a warning
// to a.out and returns nil rather than failing the run.
func (a *Adapter) VersionInfo(ctx context.Context) map[string]string {
	bin, err := a.ensurer()
	if err != nil {
		fmt.Fprintf(a.out, "warning: pgbench version: %v\n", err)
		return nil
	}
	out, err := a.run(ctx, io.Discard, nil, bin, "--version")
	if err != nil {
		fmt.Fprintf(a.out, "warning: pgbench version: %v\n", err)
		return nil
	}
	m := versionRe.FindSubmatch(out)
	if m == nil {
		fmt.Fprintf(a.out, "warning: pgbench version: unexpected output %q\n", strings.TrimSpace(string(out)))
		return nil
	}
	return map[string]string{"pgbench_version": string(m[1])}
}

func execRun(ctx context.Context, out io.Writer, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	tail := &tailBuffer{}
	w := io.Writer(tail)
	if out != nil && out != io.Discard {
		fmt.Fprintf(out, "$ %s %s\n", name, strings.Join(args, " "))
		w = io.MultiWriter(out, tail)
	}
	cmd.Stdout = w
	cmd.Stderr = w
	err := cmd.Run()
	return tail.Bytes(), err
}

// maxTailBytes bounds how much subprocess output execRun keeps in memory.
// pgbench prints its summary last, so the tail always holds it.
const maxTailBytes = 64 * 1024

// tailBuffer is an io.Writer that retains only the last maxTailBytes written
// to it.
type tailBuffer struct {
	buf       []byte
	truncated bool
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > maxTailBytes {
		trimmed := make([]byte, maxTailBytes)
		copy(trimmed, t.buf[len(t.buf)-maxTailBytes:])
		t.buf = trimmed
		t.truncated = true
	}
	return len(p), nil
}

func (t *tailBuffer) Bytes() []byte {
	if t.truncated {
		return append([]byte("...[truncated]...\n"), t.buf...)
	}
	return t.buf
}
