package supabase

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"

	"github.com/dbarena/benchctl/internal/config"
	"github.com/dbarena/benchctl/internal/diagnostics"
)

// Output keys the collector addresses the project by, set by the Supabase
// target provider.
const (
	outputProjectRef = "project_ref"
	outputHost       = "host"
)

// Collector implements diagnostics.Collector for Supabase targets.
type Collector struct {
	doer httpDoer
	run  cmdRunner
	cfg  *config.Config

	// hostOverride replaces the derived Management API base in tests.
	hostOverride string

	// Resolved once in Preflight.
	projectRef string
	dbHost     string
	token      string
	profile    string
}

var (
	_ diagnostics.Collector = (*Collector)(nil)
	_ diagnostics.Preflight = (*Collector)(nil)
)

func New(cfg *config.Config) *Collector {
	return &Collector{doer: http.DefaultClient, run: execCLI, cfg: cfg}
}

func (c *Collector) Name() string { return diagnostics.VendorSupabase }

// Preflight resolves the project identifiers and the access token once.
func (c *Collector) Preflight(req diagnostics.Request) error {
	if _, err := exec.LookPath("supabase"); err != nil {
		return errors.New("the supabase CLI is not on PATH")
	}

	c.projectRef = req.Outputs[outputProjectRef]
	c.dbHost = req.Outputs[outputHost]
	if c.projectRef == "" {
		return fmt.Errorf("target declares vendor %q but exports no %s", diagnostics.VendorSupabase, outputProjectRef)
	}
	c.profile = req.Outputs[outputProfile]

	if c.cfg != nil {
		c.token = c.cfg.Supabase.AccessToken
	}
	if c.token == "" {
		return errors.New("no Supabase access token; set BENCHCTL_SUPABASE_ACCESS_TOKEN or run `supabase login`")
	}
	return nil
}

// Collect gathers everything reachable and reports what it could not.
//
// Nothing here is sliced per window. The log is the only time-bounded source
// Supabase offers, and it reads better as one continuous record; every other
// artifact is a snapshot with no history behind it.
func (c *Collector) Collect(ctx context.Context, req diagnostics.Request) (diagnostics.Result, error) {
	var res diagnostics.Result
	start, end := req.Span()

	req.Reportf("project, disk and Postgres configuration")
	c.collectConfig(ctx, req, &res)

	req.Reportf("Postgres log from the analytics endpoint")
	c.collectLog(ctx, req, start, end, &res)

	req.Reportf("catalogue snapshot via `supabase inspect report`")
	c.collectInspectReport(ctx, req, &res)

	if len(res.Artifacts) == 0 {
		return res, errors.New("every Supabase source failed; see the warnings above")
	}
	return res, nil
}
