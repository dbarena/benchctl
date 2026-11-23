package supabase

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"net/url"
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

// outputDiagnosticsMaxProjects and outputDiagnosticsParallel carry the
// scenario's collection settings, when it set them.
const (
	outputDiagnosticsMaxProjects = "_supabase_diagnostics_max_projects"
	outputDiagnosticsParallel    = "_supabase_diagnostics_parallel"
)

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
	command := "supabase " + strings.Join(args, " ")
	profile, custom, cleanup, err := c.inspectProfile(p)
	if err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%s: %v", name, err))
		return
	}
	defer cleanup()
	if profile != "" {
		args = append(args, "--profile", profile)
		if custom {
			// The generated file is gone once the report ends, so the
			// recorded command names the API it stands for instead.
			command += " --profile <profile for " + c.apiURL + ">"
		} else {
			command += " --profile " + profile
		}
	}

	_, err = c.run(ctx, c.env(), args...)
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

// inspectProfile returns the --profile value for the CLI to use for project p.
//
// The CLI picks its Management API from the profile. A run that overrode the
// API with management_api_url is not reachable through any named profile, so
// the CLI would ask production instead. For such a run this writes a profile
// file that points at the run's API, and returns its path with custom set. The
// caller must call cleanup once the CLI has run.
func (c *Collector) inspectProfile(p supabaseprovider.TargetProject) (profile string, custom bool, cleanup func(), err error) {
	noop := func() {}
	if c.apiURL == "" {
		return c.profile, false, noop, nil
	}
	// Compare with the API the CLI itself will use for the profile. It does
	// not guess from the project's host the way ManagementAPIBase does.
	if base, ok := supabaseprovider.ProfileAPIBase(c.profile); ok && base == c.apiURL {
		return c.profile, false, noop, nil
	}
	u, err := url.Parse(c.apiURL)
	if err != nil || u.Hostname() == "" {
		return "", false, noop, fmt.Errorf("management API URL %q is not usable as a CLI profile", c.apiURL)
	}
	// project_host is the domain project hosts live under: db.<ref>.<domain>.
	host := strings.TrimPrefix(p.DBHost, "db."+p.Ref+".")
	if host == "" || host == p.DBHost {
		host = u.Hostname()
	}
	f, err := os.CreateTemp("", "benchctl-supabase-profile-*.yaml")
	if err != nil {
		return "", false, noop, fmt.Errorf("write CLI profile: %w", err)
	}
	cleanup = func() { os.Remove(f.Name()) }
	_, err = fmt.Fprintf(f, "name: benchctl-custom\napi_url: %q\ndashboard_url: %q\nproject_host: %q\n", c.apiURL, c.apiURL, host)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		cleanup()
		return "", false, noop, fmt.Errorf("write CLI profile: %w", err)
	}
	return f.Name(), true, cleanup, nil
}
