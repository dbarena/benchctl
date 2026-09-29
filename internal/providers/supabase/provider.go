// Package supabase implements a TargetProvider that provisions a Supabase
// cloud database project via the Supabase CLI (beta channel).
package supabase

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dbarena/benchctl/internal/config"
	"github.com/dbarena/benchctl/internal/consolelog"
	"github.com/dbarena/benchctl/internal/engine"
)

const (
	outputKeyProjectRef = "project_ref"

	// outputKeyVendor declares which cloud this target runs on, so post-run tooling
	// knows whose APIs and credentials apply.
	outputKeyVendor = "vendor"
	// _supabase_profile stores the CLI profile used at provision time so
	// teardown can use the same profile on a fresh CI runner.
	outputKeyProfile = "_supabase_profile"
	// _supabase_run_id stores the run ID so teardown can remove the local
	// working directory on success.
	outputKeyRunID = "_supabase_run_id"

	pollInterval = 15 * time.Second
	pollTimeout  = 10 * time.Minute
	// poolerGrace is the wait after ACTIVE_HEALTHY before returning connection
	// details. The Supavisor pooler registers new tenants asynchronously after
	// the project reports ready, so connecting immediately causes ENOTFOUND.
	poolerGrace = 60 * time.Second

	// recoverLookupAttempts/defaultRecoverLookupInterval bound how hard
	// recoverCreatedProject tries to spot a project the CLI created but
	// didn't report: the API can take a moment to list a just-created
	// project, the same lag waitForProject tolerates via errProjectNotListed.
	recoverLookupAttempts        = 3
	defaultRecoverLookupInterval = 5 * time.Second

	// defaultDiskResizePollInterval/defaultDiskResizePollTimeout bound how
	// long resizeDiskIfNeeded waits for GET .../config/disk to confirm a
	// requested resize actually landed, rather than trusting the POST
	// response alone (see resizeDiskIfNeeded's doc comment).
	defaultDiskResizePollInterval = 10 * time.Second
	defaultDiskResizePollTimeout  = 5 * time.Minute

	connModeDirect               = "direct"
	connModeSupavisorSession     = "supavisor-session"
	connModeSupavisorTransaction = "supavisor-transaction"

	diskTypeGP3 = "gp3"
	diskTypeIO2 = "io2"
)

// httpDoer is satisfied by *http.Client; injected so tests can substitute a
// fake without hitting the network, mirroring the cmdRunner injection
// pattern used by the ec2 and gotpc providers/adapters.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// cmdRunner executes the supabase CLI and returns its captured stdout/stderr;
// injected so tests can substitute a fake without invoking the real binary,
// mirroring the cmdRunner pattern in internal/providers/dockercompose.
type cmdRunner func(ctx context.Context, env []string, out io.Writer, args ...string) (stdout, stderr []byte, err error)

// execCLI runs the real `supabase` binary via exec.CommandContext.
func execCLI(ctx context.Context, env []string, out io.Writer, args ...string) (stdout, stderr []byte, err error) {
	cmd := exec.CommandContext(ctx, "supabase", args...)
	cmd.Env = env
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = io.MultiWriter(out, &stderrBuf)
	runErr := cmd.Run()
	return stdoutBuf.Bytes(), stderrBuf.Bytes(), runErr
}

// Provider implements engine.TargetProvider using the Supabase CLI.
type Provider struct {
	out  io.Writer
	doer httpDoer
	run  cmdRunner
	cfg  *config.Config

	// checkCLI is injected so tests can bypass the real exec.LookPath("supabase")
	// check, which verifies nothing about the (already-faked) run field;
	// requiring the real binary just to satisfy it would make tests
	// non-hermetic. Nil means "use the real checkInstalled" (see
	// checkCLIOrDefault).
	checkCLI func() error

	// diskResizePollInterval/diskResizePollTimeout override the default
	// cadence resizeDiskIfNeeded uses to confirm a resize landed. Zero means
	// "use the default consts above"; tests set small values here to
	// exercise the timeout path without a real wait.
	diskResizePollInterval time.Duration
	diskResizePollTimeout  time.Duration

	// pollInterval/pollTimeout override the default cadence waitForProject
	// uses to poll for ACTIVE_HEALTHY. Zero means "use the default consts
	// above"; tests set small values here to exercise the timeout path
	// without a real 10-minute wait.
	pollInterval time.Duration
	pollTimeout  time.Duration

	// recoverLookupInterval overrides the wait between recoverCreatedProject's
	// projects-list attempts. Zero means "use the default const above"; tests
	// set a small value here to avoid a real wait.
	recoverLookupInterval time.Duration
}

func New(cfg *config.Config) *Provider {
	return &Provider{out: os.Stderr, doer: http.DefaultClient, run: execCLI, cfg: cfg}
}

func (p *Provider) diskResizePollIntervalOrDefault() time.Duration {
	if p.diskResizePollInterval > 0 {
		return p.diskResizePollInterval
	}
	return defaultDiskResizePollInterval
}

func (p *Provider) diskResizePollTimeoutOrDefault() time.Duration {
	if p.diskResizePollTimeout > 0 {
		return p.diskResizePollTimeout
	}
	return defaultDiskResizePollTimeout
}

func (p *Provider) pollIntervalOrDefault() time.Duration {
	if p.pollInterval > 0 {
		return p.pollInterval
	}
	return pollInterval
}

func (p *Provider) pollTimeoutOrDefault() time.Duration {
	if p.pollTimeout > 0 {
		return p.pollTimeout
	}
	return pollTimeout
}

func (p *Provider) recoverLookupIntervalOrDefault() time.Duration {
	if p.recoverLookupInterval > 0 {
		return p.recoverLookupInterval
	}
	return defaultRecoverLookupInterval
}

// cliRunner returns p.run, falling back to execCLI for Providers constructed
// as a bare struct literal (e.g. in tests that only set out/doer).
func (p *Provider) cliRunner() cmdRunner {
	if p.run != nil {
		return p.run
	}
	return execCLI
}

// checkCLIOrDefault returns p.checkCLI, falling back to the real
// checkInstalled for Providers constructed via New() (or a bare struct
// literal that doesn't override it).
func (p *Provider) checkCLIOrDefault() error {
	if p.checkCLI != nil {
		return p.checkCLI()
	}
	return checkInstalled()
}

type providerConfig struct {
	Region              string
	OrgID               string
	HighAvailability    bool
	Experimental        bool
	ReleaseChannel      string
	PostgresEngine      string
	Profile             string
	ProjectSize         string
	ConnectionMode      string
	DiskSizeGB          int
	DiskIOPS            int
	DiskThroughputMibps int
	DiskType            string
}

func parseConfig(cfg map[string]any) (providerConfig, error) {
	var c providerConfig
	c.Region, _ = cfg["region"].(string)
	if c.Region == "" {
		return c, fmt.Errorf("config.region is required")
	}
	c.OrgID, _ = cfg["org_id"].(string)
	if c.OrgID == "" {
		return c, fmt.Errorf("config.org_id is required")
	}
	c.ProjectSize, _ = cfg["project_size"].(string)
	if c.ProjectSize == "" {
		return c, fmt.Errorf("config.project_size is required")
	}
	c.ConnectionMode, _ = cfg["connection_mode"].(string)
	switch c.ConnectionMode {
	case connModeDirect, connModeSupavisorSession, connModeSupavisorTransaction:
	case "":
		return c, fmt.Errorf("config.connection_mode is required (direct | supavisor-session | supavisor-transaction)")
	default:
		return c, fmt.Errorf("config.connection_mode %q is invalid: must be direct, supavisor-session, or supavisor-transaction", c.ConnectionMode)
	}
	switch v := cfg["high_availability"].(type) {
	case bool:
		c.HighAvailability = v
	case string:
		c.HighAvailability = strings.EqualFold(v, "true")
	}
	switch v := cfg["experimental"].(type) {
	case bool:
		c.Experimental = v
	case string:
		c.Experimental = strings.EqualFold(v, "true")
	}
	c.ReleaseChannel, _ = cfg["release_channel"].(string)
	c.PostgresEngine, _ = cfg["postgres_engine"].(string)
	c.Profile, _ = cfg["profile"].(string)
	var err error
	if c.DiskSizeGB, err = parseIntConfig(cfg, "disk_size_gb"); err != nil {
		return c, err
	}
	if c.DiskIOPS, err = parseIntConfig(cfg, "disk_iops"); err != nil {
		return c, err
	}
	if c.DiskThroughputMibps, err = parseIntConfig(cfg, "disk_throughput_mibps"); err != nil {
		return c, err
	}
	c.DiskType, _ = cfg["disk_type"].(string)
	switch c.DiskType {
	case diskTypeGP3, diskTypeIO2:
	case "":
		c.DiskType = diskTypeGP3
	default:
		return c, fmt.Errorf("config.disk_type %q is invalid: must be gp3 or io2", c.DiskType)
	}
	return c, nil
}

// parseIntConfig coerces cfg[key] (arriving as a string post-templating, or
// occasionally a raw int/int64) to an int. Returns 0, nil when the key is
// absent or an empty string.
func parseIntConfig(cfg map[string]any, key string) (int, error) {
	switch v := cfg[key].(type) {
	case string:
		if v == "" {
			return 0, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return 0, fmt.Errorf("config.%s: expected int, got %q", key, v)
		}
		return n, nil
	case int:
		return v, nil
	case int64:
		return int(v), nil
	}
	return 0, nil
}

// Provision creates a new Supabase project and waits for it to become ready.
//
// Expected config keys:
//
//	region           string   AWS region (required)
//	org_id           string   Supabase organisation ID (required)
//	project_size     string   instance size, e.g. "pico" (required)
//	connection_mode  string   direct | supavisor-session | supavisor-transaction (required)
//	high_availability bool    enable Multigres HA (optional, default false)
//	experimental     bool     pass --experimental to the CLI (optional, default
//	                          false; auto-enabled when release_channel or
//	                          postgres_engine is set, since the CLI rejects
//	                          those without --experimental)
//	release_channel  string   CLI --release-channel flag (optional)
//	postgres_engine  string   CLI --postgres-engine flag (optional)
//	profile          string   CLI --profile flag, e.g. "supabase-staging" (optional)
//	disk_size_gb     int      grow the disk to at least this size via the
//	                          Management API before returning (optional,
//	                          default 0 = disabled; the CLI has no equivalent
//	                          flag, projects always start at 8 GB)
//	disk_iops        int      requested gp3 IOPS for the resize call (optional,
//	                          default 0 = 3000, gp3's free baseline). Only
//	                          takes effect when disk_size_gb > 0. Pass the
//	                          compute tier's own documented baseline IOPS
//	                          (see https://supabase.com/docs/guides/platform/compute-and-disk)
//	                          rather than leaving every tier hard-pinned to
//	                          3000; Supabase's Management API has no minimum
//	                          above 0, unlike AWS RDS gp3's 3000 floor.
//	disk_throughput_mibps int requested gp3 throughput in MiB/s for the resize
//	                          call (optional, default 0 = omitted from the API
//	                          call entirely, so the Management API picks its
//	                          own default). Only takes effect when
//	                          disk_size_gb > 0. Ignored for io2 disks, which
//	                          don't accept a throughput_mibps attribute.
//	disk_type        string   gp3 | io2 (optional, default "gp3"). gp3 caps
//	                          out at min(500 IOPS/GB, 16,000); tiers whose
//	                          documented IOPS exceed that need "io2" instead.
func (p *Provider) Provision(ctx context.Context, runID string, cfg map[string]any) (engine.Outputs, error) {
	if err := p.checkCLIOrDefault(); err != nil {
		return nil, err
	}

	c, err := parseConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("supabase: %w", err)
	}

	env := p.buildEnv()

	workDir := workDirForRun(runID)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, fmt.Errorf("supabase: mkdir %s: %w", workDir, err)
	}

	password, err := generatePassword()
	if err != nil {
		return nil, fmt.Errorf("supabase: generate password: %w", err)
	}

	// Build the create command. Profile and workdir are global flags placed
	// before the subcommand; the project name is the run ID for traceability.
	args := globalArgs(c.Profile, workDir)
	args = append(args, "projects", "create",
		"--region", c.Region,
		"--yes",
		"--output-format", "json",
		"--org-id", c.OrgID,
		"--db-password", password,
		"--size", c.ProjectSize,
	)
	if c.HighAvailability {
		args = append(args, "--high-availability")
	}
	if c.Experimental || c.ReleaseChannel != "" || c.PostgresEngine != "" {
		args = append(args, "--experimental")
	}
	if c.ReleaseChannel != "" {
		args = append(args, "--release-channel", c.ReleaseChannel)
	}
	if c.PostgresEngine != "" {
		args = append(args, "--postgres-engine", c.PostgresEngine)
	}
	args = append(args, runID) // positional: project name

	consolelog.Println(p.out, fmt.Sprintf("supabase: creating project %s in %s", runID, c.Region))
	out, err := p.runCapture(ctx, env, args...)
	if err != nil {
		detail := cliErrorDetail(out)
		createErr := fmt.Errorf("supabase: projects create: %w%s%s", err, detail, authHint(err.Error()+detail))
		// A non-zero exit does not mean no project was created. The CLI retries
		// POST /v1/projects on any transport error, so a connection dropped
		// mid-create leaves the project behind and the retry fails with
		// "Project with name ... already exists in your organization". Without
		// this lookup that project is never torn down.
		if ref := p.recoverCreatedProject(env, c.Profile, c.OrgID, workDir, runID); ref != "" {
			return p.selfCleanupOrPartialOutputs(env, c.Profile, ref, runID, workDir, createErr)
		}
		return nil, createErr
	}

	ref, err := parseProjectRef(out)
	if err != nil {
		createErr := fmt.Errorf("supabase: parse project ref: %w", err)
		// The create call itself succeeded (exit 0), so a project may well
		// exist even though its ref couldn't be extracted from the response.
		if foundRef := p.recoverCreatedProject(env, c.Profile, c.OrgID, workDir, runID); foundRef != "" {
			return p.selfCleanupOrPartialOutputs(env, c.Profile, foundRef, runID, workDir, createErr)
		}
		return nil, createErr
	}
	consolelog.Println(p.out, fmt.Sprintf("supabase: project created with ref %s, waiting for ACTIVE_HEALTHY", ref))

	dbHost, err := p.waitForProject(ctx, env, c.Profile, workDir, ref, c.ConnectionMode)
	if err != nil {
		return p.selfCleanupOrPartialOutputs(env, c.Profile, ref, runID, workDir, fmt.Errorf("supabase: %w", err))
	}

	if err := p.resizeDiskIfNeeded(ctx, dbHost, ref, c.DiskSizeGB, c.DiskIOPS, c.DiskThroughputMibps, c.DiskType); err != nil {
		return p.selfCleanupOrPartialOutputs(env, c.Profile, ref, runID, workDir, fmt.Errorf("supabase: %w", err))
	}

	host, port, user := connectionDetails(dbHost, ref, c.ConnectionMode, c.Region)

	outputs := engine.Outputs{
		"host":     host,
		"port":     port,
		"user":     user,
		"password": password,
		"db":       "postgres",
		// Supabase projects always require TLS; sslnegotiation=direct skips the
		// plaintext SSLRequest round trip in favor of an immediate TLS handshake.
		"sslmode":           "require",
		"sslnegotiation":    "direct",
		outputKeyProjectRef: ref,
		outputKeyProfile:    c.Profile,
		outputKeyRunID:      runID,
		outputKeyVendor:     "supabase",
	}
	return outputs, nil
}

// recoverCreatedProject returns the ref of the project named runID, or "" if
// there is none. It is the fallback for every way `projects create` can leave a
// live project without reporting its ref: a non-zero exit after the API already
// created it, or an exit 0 without a parseable ref in the response body.
// Project names are always the run ID (see the positional arg in Provision).
//
// The lookup runs on a fresh context because the run context may be the very
// reason create failed, and it retries a few times because the API can take a
// moment to list a just-created project. Recovery is best effort: any error
// yields "" so the caller reports the original create failure unchanged.
func (p *Provider) recoverCreatedProject(env []string, profile, orgID, workDir, runID string) string {
	consolelog.Println(p.out, fmt.Sprintf("supabase: create did not yield a project ref; checking projects list by name %s", runID))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	args := globalArgs(profile, workDir)
	args = append(args, "projects", "list", "--output-format", "json")

	for attempt := 0; attempt < recoverLookupAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ""
			case <-time.After(p.recoverLookupIntervalOrDefault()):
			}
		}
		out, err := p.runCapture(ctx, env, args...)
		if err != nil {
			continue
		}
		ref, err := projectRefByName(out, runID, orgID)
		if err != nil {
			continue
		}
		consolelog.Println(p.out, fmt.Sprintf("supabase: found project %s by name", ref))
		return ref
	}
	consolelog.Println(p.out, fmt.Sprintf("supabase: no project named %s exists; nothing to clean up", runID))
	return ""
}

// selfCleanupOrPartialOutputs is called when a provisioning step fails after a
// project has already been created (ref is known). It attempts to delete the
// project itself, using a fresh background context so a cancelled/expired run
// context (e.g. Ctrl+C, an ACTIVE_HEALTHY poll timeout) doesn't also abort the
// cleanup. If self-cleanup succeeds, the local workdir is removed and the
// original error is returned with no outputs (nothing left to tear down). If
// self-cleanup itself fails, it returns enough outputs (project_ref, profile,
// run_id) for `benchctl teardown <run-id>` to finish the job manually.
func (p *Provider) selfCleanupOrPartialOutputs(env []string, profile, ref, runID, workDir string, origErr error) (engine.Outputs, error) {
	consolelog.Println(p.out, fmt.Sprintf("supabase: provisioning failed; deleting project %s to avoid an orphaned resource", ref))
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cleanupCancel()
	if delErr := p.deleteProject(cleanupCtx, env, profile, ref); delErr != nil {
		fmt.Fprintf(p.out, "warning: supabase: cleanup after failed provision: delete project %s: %v%s\n", ref, delErr, authHint(delErr.Error()))
		return engine.Outputs{
			outputKeyProjectRef: ref,
			outputKeyProfile:    profile,
			outputKeyRunID:      runID,
		}, fmt.Errorf("%w (cleanup failed: run 'benchctl teardown %s' to delete project %s manually)", origErr, runID, ref)
	}
	if rmErr := os.RemoveAll(workDir); rmErr != nil && !os.IsNotExist(rmErr) {
		fmt.Fprintf(p.out, "warning: supabase: remove workdir %s: %v\n", workDir, rmErr)
	}
	return nil, origErr
}

// deleteProject deletes the Supabase project identified by ref via the CLI.
func (p *Provider) deleteProject(ctx context.Context, env []string, profile, ref string) error {
	args := globalArgs(profile)
	args = append(args, "projects", "delete", "--yes", ref)
	consolelog.Println(p.out, fmt.Sprintf("supabase: deleting project %s", ref))
	if out, stderr, err := p.runCaptureErr(ctx, env, args...); err != nil {
		if alreadyDeleted(stderr) {
			consolelog.Println(p.out, fmt.Sprintf("supabase: project %s already deleted, treating teardown as successful", ref))
		} else {
			detail := cliErrorDetail(out)
			return fmt.Errorf("supabase: projects delete %s: %w%s%s", ref, err, detail, authHint(err.Error()+detail))
		}
	}
	return nil
}

// Teardown deletes the Supabase project identified by project_ref in outputs.
// On success it also removes the local per-run working directory (see
// workDirForRun).
func (p *Provider) Teardown(ctx context.Context, outputs engine.Outputs) error {
	ref := outputs[outputKeyProjectRef]
	if ref == "" {
		return fmt.Errorf("supabase: teardown: missing %s in outputs", outputKeyProjectRef)
	}

	if err := p.checkCLIOrDefault(); err != nil {
		return err
	}
	profile := outputs[outputKeyProfile]
	runID := outputs[outputKeyRunID]

	env := p.buildEnv()

	// deleteProject passes no --workdir: the local state dir won't exist on a
	// fresh CI runner and project deletion only needs the project_ref and auth
	// token. It also treats an already-removed project as success, so a
	// redundant teardown (after a provisioning rollback, a retry, or a manual
	// delete) doesn't turn into a hard failure that leaves the run
	// un-terminated.
	if err := p.deleteProject(ctx, env, profile, ref); err != nil {
		return err
	}

	// Remove local state only after a successful delete so the workdir
	// remains for debugging on any failure.
	if runID != "" {
		wd := workDirForRun(runID)
		if err := os.RemoveAll(wd); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(p.out, "warning: supabase: remove workdir %s: %v\n", wd, err)
		}
	}
	return nil
}

// RunMetadata implements engine.TargetMetadata, surfacing the project id as
// collector metadata under the label key "project_id".
func (p *Provider) RunMetadata(outputs engine.Outputs) map[string]string {
	if ref := outputs[outputKeyProjectRef]; ref != "" {
		return map[string]string{"project_id": ref}
	}
	return nil
}

// waitForProject polls supabase projects list until the project with the given
// ref reaches ACTIVE_HEALTHY status, then returns the database host. If
// connectionMode uses the Supavisor pooler, it additionally waits poolerGrace
// for tenant registration before returning.
func (p *Provider) waitForProject(ctx context.Context, env []string, profile, workDir, ref, connectionMode string) (string, error) {
	timeout := p.pollTimeoutOrDefault()
	interval := p.pollIntervalOrDefault()
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return "", fmt.Errorf("project %s did not become ready within %s", ref, timeout)
		}
		args := globalArgs(profile, workDir)
		args = append(args, "projects", "list", "--output-format", "json")
		out, err := p.runCapture(ctx, env, args...)
		if err != nil {
			detail := cliErrorDetail(out)
			return "", fmt.Errorf("projects list: %w%s%s", err, detail, authHint(err.Error()+detail))
		}
		status, host, err := projectStatusAndHost(out, ref)
		if err != nil {
			if !errors.Is(err, errProjectNotListed) {
				return "", err
			}
			consolelog.Println(p.out, "supabase: project not yet visible in projects list, retrying")
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(interval):
			}
			continue
		}
		consolelog.Println(p.out, fmt.Sprintf("supabase: project status: %s", status))
		if status == "ACTIVE_HEALTHY" {
			if !usesPooler(connectionMode) {
				return host, nil
			}
			consolelog.Println(p.out, fmt.Sprintf("supabase: waiting %s for pooler registration", poolerGrace))
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(poolerGrace):
			}
			return host, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}
	}
}

// usesPooler reports whether the given connection mode routes through the
// Supavisor pooler rather than connecting directly to the database.
func usesPooler(mode string) bool {
	return mode == connModeSupavisorSession || mode == connModeSupavisorTransaction
}

// runCapture runs the supabase CLI and returns captured stdout.
// stderr is forwarded to p.out for user-visible progress messages, and also
// captured (via runCaptureErr) for callers that need to inspect it.
func (p *Provider) runCapture(ctx context.Context, env []string, args ...string) ([]byte, error) {
	stdout, _, err := p.runCaptureErr(ctx, env, args...)
	return stdout, err
}

// runCaptureErr behaves like runCapture but also returns the command's stderr,
// still forwarded live to p.out (via io.MultiWriter) so progress output is
// unaffected. Callers need stderr to tell apart error conditions that
// cmd.Run()'s error hides; an ExitError carries no message text of its own.
func (p *Provider) runCaptureErr(ctx context.Context, env []string, args ...string) (stdout, stderr []byte, err error) {
	fmt.Fprintf(p.out, "$ supabase %s\n", redactArgs(args))
	return p.cliRunner()(ctx, env, p.out, args...)
}

// globalArgs returns the CLI global flags (--profile and/or --workdir) that
// must appear before any subcommand. Empty strings are omitted.
func globalArgs(profile string, workDir ...string) []string {
	var args []string
	if profile != "" {
		args = append(args, "--profile", profile)
	}
	if len(workDir) > 0 && workDir[0] != "" {
		args = append(args, "--workdir", workDir[0])
	}
	return args
}

// workDirForRun returns the stable per-run working directory path, rooted
// under the user's home directory so benchctl can be invoked from any
// directory (see tofustate.RunDir for the equivalent OpenTofu logic).
func workDirForRun(runID string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("supabase-state", runID)
	}
	return filepath.Join(home, ".benchctl", "supabase-state", runID)
}

// buildEnv returns the process environment with SUPABASE_ACCESS_TOKEN injected
// for the child `supabase` CLI process, when the configured Supabase access
// token is set.
func (p *Provider) buildEnv() []string {
	env := os.Environ()
	if tok := p.cfg.Supabase.AccessToken; tok != "" {
		env = append(env, "SUPABASE_ACCESS_TOKEN="+tok)
	}
	return env
}

// generatePassword creates a cryptographically random 24-byte hex password.
func generatePassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// checkInstalled verifies that the supabase binary is available in PATH.
func checkInstalled() error {
	if _, err := exec.LookPath("supabase"); err != nil {
		return fmt.Errorf("supabase provider: supabase CLI not found in PATH\n" +
			"Install the beta version: https://supabase.com/docs/guides/local-development/cli/getting-started#beta-channel\n" +
			"The binary must be invokable as `supabase` from the shell (npm/npx wrappers are not supported).")
	}
	return nil
}

// alreadyDeleted reports whether stderr from a failed `projects delete`
// indicates the project is already gone rather than a genuine failure, e.g.
// torn down by an earlier attempt: a run's own provisioning-failure rollback,
// a retried teardown, or a manual deletion.
// Teardown must be idempotent against this so a redundant teardown call
// never turns into a hard failure.
func alreadyDeleted(stderr []byte) bool {
	return bytes.Contains(stderr, []byte("Resource has been removed"))
}

// authHint returns a hint about authentication when the given text (an error
// message, optionally combined with cliErrorDetail's output) looks auth-related.
func authHint(msg string) string {
	if strings.Contains(msg, "Unauthorized") || strings.Contains(msg, "401") ||
		strings.Contains(msg, "token") || strings.Contains(msg, "login") {
		return "\nTip: run `supabase login` or set BENCHCTL_SUPABASE_ACCESS_TOKEN"
	}
	return ""
}

// cliErrorDetail extracts a human-readable detail string from JSON-formatted
// CLI stdout. With --output-format json, the CLI writes its error object to
// stdout on failure, not stderr, so it never appears via the live stderr
// forwarding in runCaptureErr; callers that discard stdout on error get a bare
// "exit status 1" with no indication of what actually went wrong. Returns ""
// if stdout isn't the CLI's JSON error shape (e.g. it's from --output-format
// text, or empty).
func cliErrorDetail(stdout []byte) string {
	var e struct {
		Tag   string `json:"_tag"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(stdout), &e); err != nil || e.Error.Message == "" {
		return ""
	}
	if e.Error.Code != "" {
		return fmt.Sprintf(": %s (%s)", e.Error.Message, e.Error.Code)
	}
	return ": " + e.Error.Message
}

// parseProjectRef extracts the project reference from the JSON output of
// `supabase projects create`.
func parseProjectRef(data []byte) (string, error) {
	var v struct {
		Ref string `json:"ref"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return "", fmt.Errorf("parse JSON: %w (raw: %s)", err, truncate(data, 200))
	}
	if v.Ref == "" {
		return "", fmt.Errorf("ref field absent or empty in response: %s", truncate(data, 200))
	}
	return v.Ref, nil
}

// projectStatusAndHost parses the JSON output of `supabase projects list` and
// returns the status and database host for the project with the given ref.
func projectStatusAndHost(data []byte, ref string) (status, host string, err error) {
	var v struct {
		Projects []struct {
			Ref      string `json:"ref"`
			Status   string `json:"status"`
			Database struct {
				Host string `json:"host"`
			} `json:"database"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return "", "", fmt.Errorf("parse projects list JSON: %w", err)
	}
	for _, p := range v.Projects {
		if p.Ref == ref {
			return p.Status, p.Database.Host, nil
		}
	}
	return "", "", fmt.Errorf("%w: %s", errProjectNotListed, ref)
}

// errProjectNotListed indicates the target ref is absent from an otherwise
// well-formed `projects list` response. waitForProject treats this as
// transient, since the API can take a moment to list a just-created project,
// and keeps polling rather than failing the run outright.
var errProjectNotListed = errors.New("project not found in projects list")

// projectRefByName parses the JSON output of `supabase projects list` and
// returns the ref of the project whose name matches the given name. Used by
// recoverCreatedProject to find a project the CLI created but didn't report;
// project names are always set to the run ID at creation time (see Provision).
//
// orgID confines the match to the org being provisioned into, so a same-named
// project in another org the token can see is never mistaken for this run's and
// deleted. The CLI reports the org as both organization_id and
// organization_slug; either matching is enough. An empty orgID matches on name
// alone.
func projectRefByName(data []byte, name, orgID string) (string, error) {
	var v struct {
		Projects []struct {
			Ref              string `json:"ref"`
			Name             string `json:"name"`
			OrganizationID   string `json:"organization_id"`
			OrganizationSlug string `json:"organization_slug"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return "", fmt.Errorf("parse projects list JSON: %w", err)
	}
	for _, p := range v.Projects {
		if p.Name != name {
			continue
		}
		if orgID != "" && p.OrganizationID != orgID && p.OrganizationSlug != orgID {
			continue
		}
		return p.Ref, nil
	}
	return "", fmt.Errorf("no project named %q found in projects list", name)
}

// connectionDetails returns the host, port, and user for the given connection
// mode. It derives the pooler hostname from the direct DB host so no
// hardcoded production/staging domain assumptions are needed.
//
// Direct host format: db.<ref>.<domain>  (e.g. db.abc123.supabase.co)
// Pooler host format: aws-0-<region>.pooler.<domain>
func connectionDetails(dbHost, ref, mode, region string) (host, port, user string) {
	switch mode {
	case connModeDirect:
		return dbHost, "5432", "postgres"
	case connModeSupavisorSession:
		return poolerHost(dbHost, region), "5432", "postgres." + ref
	case connModeSupavisorTransaction:
		return poolerHost(dbHost, region), "6543", "postgres." + ref
	default:
		// parseConfig rejects any other mode, so this only guards against a new
		// mode reaching here without a case above.
		return dbHost, "5432", "postgres"
	}
}

// poolerHost derives the Supavisor pooler hostname from the direct DB host
// and the region. The pooler and direct endpoints use different domains in
// each environment, so a mapping is applied.
//
// Production: db.abc.supabase.co  → aws-0-<region>.pooler.supabase.com
// Staging:    db.abc.supabase.red → aws-0-<region>.pooler.supabase.green
func poolerHost(dbHost, region string) string {
	// dbHost = "db.<ref>.<domain>"; split into at most 3 parts.
	parts := strings.SplitN(dbHost, ".", 3)
	directDomain := ""
	if len(parts) == 3 {
		directDomain = parts[2]
	}
	poolerDomain := directDomain
	switch directDomain {
	case "supabase.co":
		poolerDomain = "supabase.com"
	case "supabase.red":
		poolerDomain = "supabase.green"
	}
	if poolerDomain == "" {
		poolerDomain = "supabase.com"
	}
	return fmt.Sprintf("aws-0-%s.pooler.%s", region, poolerDomain)
}

// resizeDiskIfNeeded grows the project's disk to at least sizeGB via the
// Management API's disk-config endpoint, the same action the dashboard's
// "increase disk size" control performs. The Supabase CLI has no equivalent
// subcommand (confirmed against `supabase projects --help` / `db --help`).
//
// A no-op when sizeGB is 0 (the default; opt-in only, so scenarios that don't
// set disk_size_gb are unaffected) or when the disk is already large enough.
//
// iops is the requested IOPS; 0 defaults to gp3's free baseline (3000).
// Deliberately not hard-pinned to 3000 for every tier: pass the compute
// tier's own documented baseline IOPS (e.g. XL's 6000, per
// https://supabase.com/docs/guides/platform/compute-and-disk) so the disk
// matches what that tier actually gets, rather than silently downgrading it.
// Tiers whose documented IOPS exceed gp3's cap (min(500 IOPS/GB, 16,000))
// need diskType "io2" instead, since gp3 rejects them with an HTTP 400.
//
// throughputMibps is the requested gp3 throughput in MiB/s; 0 means "not
// set", so the field is omitted from the request entirely and the
// Management API picks its own default (throughput is never meaningfully 0).
// io2 disks don't accept this attribute at all, so it's always omitted when
// diskType is "io2".
//
// A successful POST response only confirms the resize request was accepted
// and enqueued, not that it was applied: the platform's modifyDisk handler
// waits at most a few seconds for the underlying AWS volume modification job
// to report completion, and silently returns success if that window elapses
// without a result either way. So after the POST accepts the request, this
// polls GET .../config/disk (defaultDiskResizePollInterval /
// defaultDiskResizePollTimeout) until the reported attributes actually match
// what was requested, returning an error if that never happens. Observed
// live against a staging project where the POST returned 201 but the disk's
// real attributes never changed.
func (p *Provider) resizeDiskIfNeeded(ctx context.Context, dbHost, ref string, sizeGB, iops, throughputMibps int, diskType string) error {
	if sizeGB == 0 {
		return nil
	}
	if diskType == "" {
		diskType = diskTypeGP3
	}
	if iops == 0 {
		iops = 3000 // gp3 free baseline
	}
	token := p.cfg.Supabase.AccessToken
	if token == "" {
		return fmt.Errorf("resize disk: no access token (set BENCHCTL_SUPABASE_ACCESS_TOKEN)")
	}
	url := "https://" + managementAPIHost(dbHost) + "/v1/projects/" + ref + "/config/disk"

	current, err := p.getDiskAttrs(ctx, url, token)
	if err != nil {
		return fmt.Errorf("resize disk: get current size: %w", err)
	}
	if current.SizeGB >= sizeGB {
		consolelog.Println(p.out, fmt.Sprintf("supabase: disk already %d GB (requested %d GB), skipping resize", current.SizeGB, sizeGB))
		return nil
	}

	consolelog.Println(p.out, fmt.Sprintf("supabase: resizing disk %d GB -> %d GB at %d IOPS (%s)", current.SizeGB, sizeGB, iops, diskType))
	attrs := fmt.Sprintf(`"type":%q,"size_gb":%d,"iops":%d`, diskType, sizeGB, iops)
	if throughputMibps > 0 && diskType != diskTypeIO2 {
		attrs += fmt.Sprintf(`,"throughput_mibps":%d`, throughputMibps)
	}
	body := fmt.Sprintf(`{"attributes":{%s}}`, attrs)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("resize disk: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.doer.Do(req)
	if err != nil {
		return fmt.Errorf("resize disk: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("resize disk: HTTP %d: %s", resp.StatusCode, truncate(respBody, 500))
	}

	return p.waitForDiskResize(ctx, url, token, sizeGB, iops, diskType)
}

// waitForDiskResize polls GET .../config/disk until the reported attributes
// meet the requested size/type/IOPS, or returns an error once
// diskResizePollTimeoutOrDefault() elapses without that happening. See
// resizeDiskIfNeeded's doc comment for why this confirmation can't be
// skipped.
func (p *Provider) waitForDiskResize(ctx context.Context, url, token string, sizeGB, iops int, diskType string) error {
	interval := p.diskResizePollIntervalOrDefault()
	deadline := time.Now().Add(p.diskResizePollTimeoutOrDefault())
	var last diskAttrs
	for {
		attrs, err := p.getDiskAttrs(ctx, url, token)
		if err != nil {
			return fmt.Errorf("resize disk: verify: %w", err)
		}
		last = attrs
		if attrs.SizeGB >= sizeGB && attrs.Type == diskType && attrs.IOPS >= iops {
			consolelog.Println(p.out, fmt.Sprintf("supabase: disk resized to %d GB", attrs.SizeGB))
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("resize disk: timed out waiting for %d GB/%d IOPS/%s after %s; disk is still %d GB/%d IOPS/%s",
				sizeGB, iops, diskType, p.diskResizePollTimeoutOrDefault(), last.SizeGB, last.IOPS, last.Type)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// diskAttrs is the subset of GET .../config/disk's response resizeDiskIfNeeded
// and waitForDiskResize need to compare current vs. requested state.
type diskAttrs struct {
	SizeGB int
	IOPS   int
	Type   string
}

// getDiskAttrs reads the current disk attributes via GET .../config/disk.
func (p *Provider) getDiskAttrs(ctx context.Context, url, token string) (diskAttrs, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return diskAttrs{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := p.doer.Do(req)
	if err != nil {
		return diskAttrs{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return diskAttrs{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(respBody, 500))
	}
	var v struct {
		Attributes struct {
			SizeGB int    `json:"size_gb"`
			IOPS   int    `json:"iops"`
			Type   string `json:"type"`
		} `json:"attributes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return diskAttrs{}, fmt.Errorf("decode response: %w", err)
	}
	return diskAttrs{SizeGB: v.Attributes.SizeGB, IOPS: v.Attributes.IOPS, Type: v.Attributes.Type}, nil
}

// managementAPIHost derives the Management API host from the project's direct
// DB host domain, the same way poolerHost derives the pooler domain. This
// avoids hardcoding a single (production) host and silently calling the wrong
// environment's disk API when a scenario targets a non-default --profile.
//
// Production: db.abc.supabase.co  → api.supabase.com
// Staging:    db.abc.supabase.red → api.supabase.green (confirmed live against
// a staging project during implementation)
func managementAPIHost(dbHost string) string {
	parts := strings.SplitN(dbHost, ".", 3)
	directDomain := ""
	if len(parts) == 3 {
		directDomain = parts[2]
	}
	switch directDomain {
	case "supabase.red":
		return "api.supabase.green"
	default:
		return "api.supabase.com"
	}
}

// redactArgs returns args as a single string with --db-password values hidden.
func redactArgs(args []string) string {
	out := make([]string, len(args))
	copy(out, args)
	for i, a := range out {
		if a == "--db-password" && i+1 < len(out) {
			out[i+1] = "[redacted]"
		}
	}
	return strings.Join(out, " ")
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
