package supabase

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/dbarena/benchctl/internal/diagnostics"
)

// outputProfile carries the CLI profile provisioning used, so collection
// talks to the same environment.
const outputProfile = "_supabase_profile"

// inspectDirName holds the CSVs `supabase inspect report` writes.
const inspectDirName = "inspect"

// cmdRunner runs the supabase CLI. Injected so tests need no CLI and no
// project, following the pattern in internal/providers/supabase.
type cmdRunner func(ctx context.Context, env []string, args ...string) ([]byte, error)

func execCLI(ctx context.Context, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "supabase", args...)
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%w: %s", err, msg)
		}
		return nil, err
	}
	return out, nil
}

// collectInspectReport runs `supabase inspect report`, which dumps a
// catalogue snapshot as CSVs in one call.
//
// This is where the SQL-level data comes from. Supabase has no Performance
// Insights equivalent, but the report includes calls.csv and outliers.csv,
// both built on pg_stat_statements, so per-statement call counts and total
// time survive the run without benchctl opening its own connection.
//
// Note that pg_stat_statements is cumulative since the last reset, i.e. on a
// benchmark project they cover provisioning, data loading and the benchmark.
//
// --project-ref rather than --db-url on purpose: the CLI then authenticates
// with the access token already needed for the log endpoint.
func (c *Collector) collectInspectReport(ctx context.Context, req diagnostics.Request, res *diagnostics.Result) {
	dir := filepath.Join(req.Dest, inspectDirName)
	args := []string{"inspect", "report", "--project-ref", c.projectRef, "--output-dir", dir}
	if c.profile != "" {
		args = append(args, "--profile", c.profile)
	}
	command := "supabase " + strings.Join(args, " ")

	if _, err := c.run(ctx, c.env(), args...); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%s: %v", inspectDirName, err))
		return
	}

	// The CLI writes into a dated subdirectory of --output-dir and reports
	// nothing machine-readable about what it produced, so the tree is walked
	// to record it.
	files, err := csvFilesUnder(dir)
	if err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%s: %v", inspectDirName, err))
		return
	}
	if len(files) == 0 {
		res.Warnings = append(res.Warnings, inspectDirName+": the report produced no CSVs")
		return
	}
	for _, f := range files {
		rel, err := filepath.Rel(req.Dest, f)
		if err != nil {
			rel = f
		}
		res.Artifacts = append(res.Artifacts, diagnostics.Artifact{File: rel, Command: command})
	}
}

func csvFilesUnder(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.EqualFold(filepath.Ext(path), ".csv") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// env passes the access token to the CLI the same way the Supabase provider
// does, rather than relying on a `supabase login` having happened.
func (c *Collector) env() []string {
	env := os.Environ()
	if c.token != "" {
		env = append(env, "SUPABASE_ACCESS_TOKEN="+c.token)
	}
	return env
}
