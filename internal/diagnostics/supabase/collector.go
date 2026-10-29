package supabase

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/dbarena/benchctl/internal/config"
	"github.com/dbarena/benchctl/internal/diagnostics"
	supabaseprovider "github.com/dbarena/benchctl/internal/providers/supabase"
)

// maxParallelProjects caps how many projects of a multi-project run are
// collected at once. Each one is a handful of Management API calls plus a
// `supabase inspect report`, which opens its own database connection.
const maxParallelProjects = 5

// maxSampledProjects caps how many projects of a Management API run with
// several projects are collected: the first ones to become ready stand for
// the rest, which can number tens of thousands. A CLI run lists every project
// in its outputs, and all of them are collected.
const maxSampledProjects = 5

// Collector implements diagnostics.Collector for Supabase targets.
type Collector struct {
	// checkCLI overrides the PATH lookup in Preflight; nil uses the real one.
	checkCLI func() error

	doer httpDoer
	run  cmdRunner
	cfg  *config.Config

	// hostOverride replaces the derived Management API base in tests.
	hostOverride string
	// logRetryInterval overrides the wait between log query retries; tests
	// set a small value to avoid a real wait. Zero means the default.
	logRetryInterval time.Duration

	// Resolved once in Preflight.
	projects []supabaseprovider.TargetProject
	token    string
	profile  string
	apiURL   string
}

var (
	_ diagnostics.Collector = (*Collector)(nil)
	_ diagnostics.Preflight = (*Collector)(nil)
)

func New(cfg *config.Config) *Collector {
	return &Collector{doer: http.DefaultClient, run: execCLI, cfg: cfg}
}

func (c *Collector) Name() string { return diagnostics.VendorSupabase }

// Preflight resolves the projects and the access token once.
func (c *Collector) Preflight(req diagnostics.Request) error {
	if err := c.requireCLI(); err != nil {
		return err
	}

	projects, err := supabaseprovider.TargetProjects(req.Outputs)
	if err != nil {
		return fmt.Errorf("read the run's projects: %w", err)
	}
	if len(projects) == 0 {
		return fmt.Errorf("target declares vendor %q but exports no project_ref, project_refs or ready projects in projects_manifest", diagnostics.VendorSupabase)
	}
	if req.Outputs[outputProjectsManifest] != "" {
		projects = projects[:min(len(projects), maxSampledProjects)]
	}
	c.projects = projects
	c.profile = req.Outputs[outputProfile]
	c.apiURL = req.Outputs[outputManagementAPIURL]

	if c.cfg != nil {
		c.token = c.cfg.Supabase.AccessToken
	}
	if c.token == "" {
		// Not `supabase login`: that stores the token in the OS keyring, where
		// the Management API calls here can't read it.
		return errors.New("no Supabase access token; set BENCHCTL_SUPABASE_ACCESS_TOKEN")
	}
	return nil
}

// Collect gathers everything reachable for every sampled project and reports
// what it could not.
//
// A single project's artifacts go straight into the diagnostics directory, as
// they always have. With several, each project gets a subdirectory named after
// its ref, holding the same set of files.
//
// Nothing here is sliced per window. The log is the only time-bounded source
// Supabase offers, and it reads better as one continuous record; every other
// artifact is a snapshot with no history behind it.
func (c *Collector) Collect(ctx context.Context, req diagnostics.Request) (diagnostics.Result, error) {
	if len(c.projects) == 1 {
		res := c.collectProject(ctx, req, c.projects[0], req.Dest, req.Reportf)
		return res, collectErr(res)
	}

	// Each project fills its own Result, merged afterwards in creation order,
	// so the index doesn't depend on which project finished first.
	results := make([]diagnostics.Result, len(c.projects))
	supabaseprovider.ForEachParallel(len(c.projects), maxParallelProjects, func(i int) {
		p := c.projects[i]
		reportf := func(format string, args ...any) {
			req.Reportf("project %d/%d %s: "+format, append([]any{i + 1, len(c.projects), p.Ref}, args...)...)
		}
		dir := filepath.Join(req.Dest, p.Ref)
		results[i] = c.collectProject(ctx, req, p, dir, reportf)
	})

	var res diagnostics.Result
	for _, r := range results {
		res.Artifacts = append(res.Artifacts, r.Artifacts...)
		res.Warnings = append(res.Warnings, r.Warnings...)
	}
	return res, collectErr(res)
}

// collectProject gathers one project's configuration, log and catalogue
// snapshot into dir.
func (c *Collector) collectProject(ctx context.Context, req diagnostics.Request, p supabaseprovider.TargetProject,
	dir string, reportf func(format string, args ...any),
) diagnostics.Result {
	var res diagnostics.Result
	if err := os.MkdirAll(dir, 0o755); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%s: %v", p.Ref, err))
		return res
	}
	start, end := req.Span()

	reportf("project, disk and Postgres configuration")
	c.collectConfig(ctx, req, p, dir, &res)

	reportf("Postgres log from the analytics endpoint")
	c.collectLog(ctx, req, p, dir, start, end, &res)

	reportf("catalogue snapshot via `supabase inspect report`")
	c.collectInspectReport(ctx, req, p, dir, &res)

	return res
}

// artifactName names a file in dir the way the index does, relative to the
// diagnostics directory, so with several projects a warning says which one
// it's about.
func artifactName(req diagnostics.Request, dir, name string) string {
	if rel, err := filepath.Rel(req.Dest, filepath.Join(dir, name)); err == nil {
		return rel
	}
	return name
}

// collectErr fails the collection only when no source produced anything.
func collectErr(res diagnostics.Result) error {
	if len(res.Artifacts) == 0 {
		return errors.New("every Supabase source failed; see the warnings above")
	}
	return nil
}

// checkCLI is injected so tests do not depend on whether the host has the
// CLI installed. Nil means use the real PATH lookup.
func (c *Collector) requireCLI() error {
	if c.checkCLI != nil {
		return c.checkCLI()
	}
	return diagnostics.RequireCLI("supabase")
}
