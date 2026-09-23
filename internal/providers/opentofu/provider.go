// Package opentofu implements a TargetProvider that provisions the benchmark
// database via OpenTofu (open-source Terraform fork).
package opentofu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dbarena/benchctl/internal/bundle"
	"github.com/dbarena/benchctl/internal/config"
	"github.com/dbarena/benchctl/internal/engine"
	"github.com/dbarena/benchctl/internal/tags"
	"github.com/dbarena/benchctl/internal/tofustate"
)

// Provider implements engine.TargetProvider using OpenTofu.
type Provider struct {
	out      io.Writer
	preApply PreApplyFunc
	cfg      *config.Config
}

// RunFunc runs `tofu <args...>` in the provider's per-run working directory,
// streaming output the same way Provision's own tofu invocations do.
type RunFunc func(ctx context.Context, args ...string) error

// PreApplyFunc customizes vars after `tofu init` and before `tofu apply`.
// cfg is the target's raw scenario config (Provision's cfg parameter); vars
// is what will be written to terraform.tfvars.json. Implementations that
// don't apply to a given cfg (e.g. wrong module) should return vars
// unchanged. Used for module-specific pre-apply steps that don't belong in
// this generic provider (e.g. adopting shared, out-of-band resources via
// `tofu import`; see internal/providers/gcpcloudsql).
type PreApplyFunc func(ctx context.Context, out io.Writer, run RunFunc, cfg, vars map[string]any) (map[string]any, error)

// New constructs a Provider. preApply may be nil when no module-specific
// pre-apply behavior is needed.
func New(preApply PreApplyFunc, cfg *config.Config) *Provider {
	return &Provider{out: os.Stderr, preApply: preApply, cfg: cfg}
}

// Provision runs `tofu init` + `tofu apply` in the configured module directory
// and returns all tofu outputs as string values.
//
// The working directory is auto-derived as ./tofu-state/<runID>/<module-basename>,
// giving each run its own stable state path that follow-up commands (teardown,
// status) can locate by run ID.
//
// Expected config keys:
//
//	module    string         path to the tofu module (required)
//	vars      map[string]any tofu variable overrides (optional)
func (p *Provider) Provision(ctx context.Context, runID string, cfg map[string]any) (engine.Outputs, error) {
	module, vars, err := parseConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("opentofu: %w", err)
	}

	vars = tags.Inject(vars, runID, p.cfg)
	vars = p.injectMetricsVars(vars)

	workDir := deriveWorkDir(runID, module)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, fmt.Errorf("opentofu: mkdir %s: %w", workDir, err)
	}

	absModule, cleanupModule, err := bundle.Dir(module)
	if err != nil {
		return nil, fmt.Errorf("opentofu: resolve module path: %w", err)
	}
	defer cleanupModule()

	// Copy .tf files into workDir so that tofu state lives there rather than
	// in the module source directory. Modern OpenTofu no longer accepts a path
	// argument to `tofu init`; the working directory must contain the files.
	fmt.Fprintf(p.out, "==> opentofu: syncing module %s → %s\n", absModule, workDir)
	if err := syncTFFiles(absModule, workDir); err != nil {
		return nil, fmt.Errorf("opentofu: sync module: %w", err)
	}

	if err := p.tofuInit(ctx, workDir); err != nil {
		return nil, fmt.Errorf("opentofu: init: %w", err)
	}

	if p.preApply != nil {
		runTofu := func(ctx context.Context, args ...string) error { return p.run(ctx, workDir, "tofu", args...) }
		vars, err = p.preApply(ctx, p.out, runTofu, cfg, vars)
		if err != nil {
			return nil, fmt.Errorf("opentofu: pre-apply: %w", err)
		}
	}

	// Persist vars to terraform.tfvars.json so `tofu destroy` picks them up
	// automatically; required variables with no default would otherwise fail
	// teardown when no -var flags are passed.
	if err := writeTFVars(workDir, vars); err != nil {
		return nil, fmt.Errorf("opentofu: write tfvars: %w", err)
	}

	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("opentofu: resolve work dir: %w", err)
	}

	applyArgs := []string{"apply", "-auto-approve", "-input=false"}
	fmt.Fprintf(p.out, "==> opentofu: apply\n")
	if err := p.run(ctx, workDir, "tofu", applyArgs...); err != nil {
		fmt.Fprintf(p.out, "==> opentofu: apply failed; attempting cleanup\n")
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cleanupCancel()
		if destroyErr := p.run(cleanupCtx, workDir, "tofu", "destroy", "-auto-approve", "-input=false", "-suppress-forget-errors"); destroyErr != nil {
			fmt.Fprintf(p.out, "warning: opentofu: cleanup after failed apply: %v\n", destroyErr)
			// Return work dir so the runner can persist state for manual teardown.
			return engine.Outputs{engine.OutputKeyTofuWorkDir: absWorkDir}, fmt.Errorf("opentofu: apply: %w", err)
		}
		// Destroy succeeded, so no infrastructure is left to tear down.
		// Returning no outputs means nothing will call Teardown for this run,
		// so remove the local workdir here.
		if err := os.RemoveAll(workDir); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(p.out, "warning: opentofu: remove workdir %s: %v\n", workDir, err)
		}
		_ = os.Remove(filepath.Dir(workDir)) // best-effort: only succeeds once empty
		return nil, fmt.Errorf("opentofu: apply: %w", err)
	}

	outputs, err := p.readOutputs(ctx, workDir)
	if err != nil {
		// The apply already succeeded, so real infrastructure exists. Return
		// the work dir despite the unreadable outputs, so `benchctl teardown`
		// can destroy that infrastructure later instead of leaking it.
		return engine.Outputs{engine.OutputKeyTofuWorkDir: absWorkDir}, err
	}
	outputs[engine.OutputKeyTofuWorkDir] = absWorkDir

	return outputs, nil
}

// Teardown runs `tofu destroy` in the working directory stored in outputs.
// If the provider cache (.terraform/) is absent, e.g. after restoring state on
// a fresh CI runner, it runs `tofu init` first to re-download providers.
func (p *Provider) Teardown(ctx context.Context, outputs engine.Outputs) error {
	workDir := outputs[engine.OutputKeyTofuWorkDir]
	if workDir == "" {
		return fmt.Errorf("opentofu: teardown: missing work dir in outputs")
	}
	if _, err := os.Stat(filepath.Join(workDir, ".terraform")); os.IsNotExist(err) {
		fmt.Fprintf(p.out, "==> opentofu: init (provider cache missing, re-initializing)\n")
		if err := p.tofuInit(ctx, workDir); err != nil {
			return fmt.Errorf("opentofu: init: %w", err)
		}
	}
	fmt.Fprintf(p.out, "==> opentofu: destroy\n")
	// -suppress-forget-errors: some modules (e.g. gcp-cloudsql/postgres) mark
	// shared, never-destroyed resources with lifecycle.destroy = false; without
	// this flag, a destroy that "forgets" (rather than deletes) any resource
	// exits non-zero purely to flag that fact, which this provider would
	// otherwise treat as a genuine teardown failure. Harmless no-op for modules
	// with no such resources.
	if err := p.run(ctx, workDir, "tofu", "destroy", "-auto-approve", "-input=false", "-suppress-forget-errors"); err != nil {
		return fmt.Errorf("opentofu: destroy: %w", err)
	}

	// Remove local state only after a successful destroy so the workdir
	// remains for debugging on any failure.
	if err := os.RemoveAll(workDir); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(p.out, "warning: opentofu: remove workdir %s: %v\n", workDir, err)
	}
	_ = os.Remove(filepath.Dir(workDir)) // best-effort: only succeeds once empty

	return nil
}

// ConnectArgs implements engine.Connector. It returns the SSH argv for an
// interactive shell session on the target instance.
func (*Provider) ConnectArgs(outputs engine.Outputs) ([]string, error) {
	ip := outputs["public_ip"]
	if ip == "" {
		return nil, fmt.Errorf("opentofu: connect: missing public_ip in target outputs")
	}
	user := outputs["ssh_user"]
	if user == "" {
		return nil, fmt.Errorf("opentofu: connect: missing ssh_user in target outputs")
	}
	keyPath := outputs["ssh_private_key_path"]
	args := append([]string{"ssh"}, hostKeyArgs(keyPath)...)
	if keyPath != "" {
		args = append(args, "-i", keyPath)
	}
	args = append(args, user+"@"+ip)
	return args, nil
}

// hostKeyArgs returns the ssh host-key-verification flags for a connection
// whose private key lives at keyPath. See the identical helper in
// internal/engine/ssh.go for the rationale (per-run known_hosts colocated
// with the key, avoiding stale entries when a cloud provider reuses an IP).
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

// readOutputs runs `tofu output -json` and converts the result to a flat
// string map (only string, number, and bool output values are supported).
func (p *Provider) readOutputs(ctx context.Context, workDir string) (engine.Outputs, error) {
	cmd := exec.CommandContext(ctx, "tofu", "output", "-json")
	cmd.Dir = workDir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = p.out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("opentofu: output: %w", err)
	}

	var raw map[string]struct {
		Value any    `json:"value"`
		Type  string `json:"type"`
	}
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		return nil, fmt.Errorf("opentofu: parse outputs: %w", err)
	}

	out := make(engine.Outputs, len(raw))
	for k, v := range raw {
		out[k] = fmt.Sprintf("%v", v.Value)
	}
	return out, nil
}

func (p *Provider) run(ctx context.Context, dir, name string, args ...string) error {
	return p.runEnv(ctx, dir, nil, name, args...)
}

// runEnv is like run but appends extraEnv to the child process's environment
// (e.g. TF_PLUGIN_CACHE_DIR for tofuInit).
func (p *Provider) runEnv(ctx context.Context, dir string, extraEnv []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout = p.out
	cmd.Stderr = p.out
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	fmt.Fprintf(p.out, "$ %s %s\n", name, strings.Join(args, " "))
	return cmd.Run()
}

// tofuInitMaxAttempts and tofuInitRetryDelays bound the retry/backoff applied
// to `tofu init`, the step that downloads and checksum-verifies providers
// from the registry (which, for both the OpenTofu and Terraform registries,
// proxies to GitHub Releases). Modules ship a committed .terraform.lock.hcl
// (see syncTFFiles) so, together with the shared plugin cache, this path
// should rarely touch the network at all; the retry/backoff below remains as
// a safety net for cases that do — a first-ever init for a new module or
// provider version, or a platform not yet covered by the lock file — where
// GitHub Releases is known to intermittently return 5xx errors, sometimes for
// several minutes at a time.
const tofuInitMaxAttempts = 6

var tofuInitRetryDelays = []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second, 60 * time.Second, 90 * time.Second}

// tofuInit runs `tofu init` against a persistent, shared provider plugin
// cache (see pluginCacheDir) so a given provider version is downloaded from
// the registry at most once per machine rather than once per run, and
// retries transient failures with backoff.
func (p *Provider) tofuInit(ctx context.Context, workDir string) error {
	cacheDir, err := pluginCacheDir()
	if err != nil {
		return fmt.Errorf("plugin cache dir: %w", err)
	}
	prunePluginCache(p.out, cacheDir)

	env := []string{"TF_PLUGIN_CACHE_DIR=" + cacheDir}
	var lastErr error
	for attempt := 1; attempt <= tofuInitMaxAttempts; attempt++ {
		if attempt > 1 {
			fmt.Fprintf(p.out, "==> opentofu: init retry %d/%d after: %v\n", attempt, tofuInitMaxAttempts, lastErr)
		} else {
			fmt.Fprintf(p.out, "==> opentofu: init\n")
		}
		lastErr = p.runEnv(ctx, workDir, env, "tofu", "init", "-input=false")
		if lastErr == nil {
			return nil
		}
		if attempt < tofuInitMaxAttempts {
			if err := sleepOrDone(ctx, tofuInitRetryDelays[attempt-1]); err != nil {
				return err
			}
		}
	}
	return lastErr
}

// sleepOrDone sleeps for d, returning early with ctx.Err() if ctx is done
// first, so retry backoff doesn't outlive a cancelled run.
func sleepOrDone(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// pluginCacheDir returns the shared OpenTofu provider plugin cache path,
// ~/.benchctl/tofu-plugin-cache, creating it if needed. Rooting it under the
// user's home directory (like tofustate.RunDir) keeps it shared across every
// run's otherwise-independent working directory, regardless of which
// directory benchctl is invoked from.
func pluginCacheDir() (string, error) {
	home, err := os.UserHomeDir()
	var dir string
	if err != nil {
		dir = filepath.Join("tofu-plugin-cache")
	} else {
		dir = filepath.Join(home, ".benchctl", "tofu-plugin-cache")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// pluginCacheMaxAge bounds how long an unused provider version may sit in
// the shared plugin cache. Provider version constraints in the modules float
// (e.g. "~> 5.0"), so old exact versions stop being reused as newer releases
// come out; left alone the cache would grow forever.
const pluginCacheMaxAge = 60 * 24 * time.Hour

// prunePluginCache removes provider files from cacheDir that haven't been
// (re)written within pluginCacheMaxAge, keeping the shared plugin cache from
// growing unbounded. A pruned provider version is simply re-downloaded on
// the next `tofu init` if a module still needs it, so this is safe: it only
// ever affects a provider version that would otherwise require a fresh
// download anyway. Best-effort: it logs failures instead of returning them,
// since a stale cache costs performance, not correctness.
func prunePluginCache(out io.Writer, cacheDir string) {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return // nothing to prune yet (e.g. first run)
	}
	cutoff := time.Now().Add(-pluginCacheMaxAge)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(cacheDir, e.Name())
		if err := pruneOldDirs(path, cutoff); err != nil {
			fmt.Fprintf(out, "warning: opentofu: prune plugin cache %s: %v\n", path, err)
		}
	}
}

// pruneOldDirs recursively removes leaf directories (directories containing
// no subdirectories, i.e. the <os>_<arch> dirs holding an actual provider
// binary) under dir whose mtime is older than cutoff, then removes any
// parent directories left empty as a result. This doesn't hardcode the
// registry-host/namespace/type/version nesting depth of the plugin cache
// layout, so it keeps working if that layout changes.
func pruneOldDirs(dir string, cutoff time.Time) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	hasSubdir := false
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		hasSubdir = true
		if err := pruneOldDirs(filepath.Join(dir, e.Name()), cutoff); err != nil {
			return err
		}
	}

	if hasSubdir {
		remaining, err := os.ReadDir(dir)
		if err == nil && len(remaining) == 0 {
			return os.Remove(dir)
		}
		return nil
	}

	// Leaf directory: prune it wholesale if stale.
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if info.ModTime().Before(cutoff) {
		return os.RemoveAll(dir)
	}
	return nil
}

func parseConfig(cfg map[string]any) (module string, vars map[string]any, err error) {
	module, _ = cfg["module"].(string)
	if module == "" {
		return "", nil, fmt.Errorf("config.module is required")
	}
	if raw, ok := cfg["vars"].(map[string]any); ok {
		vars = raw
	}
	return module, vars, nil
}

// deriveWorkDir returns a stable, run-scoped tofu working directory:
// ./tofu-state/<runID>/<module-basename>. The runID prefix keeps multiple
// concurrent runs from sharing state; the module basename disambiguates
// target vs. driver invocations within the same run.
func deriveWorkDir(runID, module string) string {
	return filepath.Join(tofustate.RunDir(runID), filepath.Base(module))
}

// writeTFVars serializes vars to terraform.tfvars.json in dir so that
// `tofu destroy` (and subsequent applies) pick them up automatically without
// needing -var flags.
func writeTFVars(dir string, vars map[string]any) error {
	if len(vars) == 0 {
		return nil
	}
	data, err := json.MarshalIndent(vars, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, "terraform.tfvars.json"), data, 0o644)
}

// injectMetricsVars injects the configured metrics endpoint and token as
// vector_sink_victoriametrics_endpoint and vector_sink_victoriametrics_token
// into vars so the Terraform module can configure the Vector sink without
// exposing credentials in the scenario YAML.
func (p *Provider) injectMetricsVars(vars map[string]any) map[string]any {
	if vars == nil {
		vars = make(map[string]any)
	}
	if ep := p.cfg.Metrics.Endpoint; ep != "" {
		vars["vector_sink_victoriametrics_endpoint"] = ep
	}
	if tok := p.cfg.Metrics.Token; tok != "" {
		vars["vector_sink_victoriametrics_token"] = tok
	}
	return vars
}

// syncModuleFiles copies every *.tf, *.sh, and *.tftpl file from srcDir into
// dstDir, overwriting any existing copies. This lets workDir own the tofu state
// while the module source directory remains read-only. Shell scripts (*.sh) and
// template files (*.tftpl) are included so that file() and templatefile()
// references in *.tf templates resolve correctly. .terraform.lock.hcl is also
// copied when present, so a module's committed provider hashes carry over into
// the work dir: paired with the shared plugin cache (see pluginCacheDir), this
// lets `tofu init` verify already-cached providers without any network access,
// instead of re-fetching checksums from the registry (which proxies to GitHub
// Releases) on every run.
func syncTFFiles(srcDir, dstDir string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		if ext != ".tf" && ext != ".sh" && ext != ".tftpl" && e.Name() != ".terraform.lock.hcl" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(srcDir, e.Name()))
		if err != nil {
			return fmt.Errorf("read %s: %w", e.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dstDir, e.Name()), data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", e.Name(), err)
		}
	}
	return nil
}
