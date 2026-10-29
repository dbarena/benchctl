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
	supabaseprovider "github.com/dbarena/benchctl/internal/providers/supabase"
)

// outputProfile carries the CLI profile provisioning used, so collection
// talks to the same environment.
const outputProfile = "_supabase_profile"

// outputManagementAPIURL carries the Management API provisioning used, when it
// went through the Management API rather than the CLI.
const outputManagementAPIURL = "_supabase_management_api_url"

// outputProjectsManifest names the manifest of a Management API run with
// several projects, which lists none of them in its outputs.
const outputProjectsManifest = "projects_manifest"

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
		// The CLI prints progress on stderr but its error on stdout, as JSON.
		msg := strings.TrimPrefix(strings.TrimSpace(stderr.String())+supabaseprovider.CLIErrorDetail(out), ": ")
		if msg != "" {
			return nil, fmt.Errorf("%w: %s", err, msg)
		}
		return nil, err
	}
	return out, nil
}

// collectInspectReport runs `supabase inspect report` for project p, which
// dumps a catalogue snapshot as CSVs into projectDir/inspect in one call.
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
func (c *Collector) collectInspectReport(ctx context.Context, req diagnostics.Request, p supabaseprovider.TargetProject, projectDir string, res *diagnostics.Result) {
	dir := filepath.Join(projectDir, inspectDirName)
	name := artifactName(req, projectDir, inspectDirName)
	args := []string{"inspect", "report", "--project-ref", p.Ref, "--output-dir", dir}
	if c.profile != "" {
		args = append(args, "--profile", c.profile)
	}
	command := "supabase " + strings.Join(args, " ")

	_, err := c.run(ctx, c.env(), args...)
	if err != nil && ctx.Err() == nil {
		_, err = c.run(ctx, c.env(), args...)
	}
	if err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%s: %v", name, err))
		return
	}

	// The CLI writes into a dated subdirectory of --output-dir and reports
	// nothing machine-readable about what it produced, so the tree is walked
	// to record it.
	files, err := csvFilesUnder(dir)
	if err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%s: %v", name, err))
		return
	}
	if len(files) == 0 {
		res.Warnings = append(res.Warnings, name+": the report produced no CSVs")
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
