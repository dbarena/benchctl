// Package sshdriver implements a cloud-agnostic engine.DriverProvider that
// provisions an SSH-reachable load-generator VM via OpenTofu, SCPs the
// benchctl binary and scenario YAML to it, and kicks off `benchctl resume` as
// a detached background process. Cloud-specific packages (ec2, gce) supply a
// Config with their own provider name, default SSH user, and instance-type
// architecture detection, and otherwise delegate everything to this package.
package sshdriver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dbarena/benchctl/internal/auth"
	"github.com/dbarena/benchctl/internal/buildinfo"
	"github.com/dbarena/benchctl/internal/bundle"
	"github.com/dbarena/benchctl/internal/config"
	"github.com/dbarena/benchctl/internal/consolelog"
	"github.com/dbarena/benchctl/internal/engine"
	"github.com/dbarena/benchctl/internal/hostmetrics"
	"github.com/dbarena/benchctl/internal/tags"
	"github.com/dbarena/benchctl/internal/tofustate"
)

const (
	outputKeyPublicIP   = "public_ip"
	outputKeySSHKeyPath = "ssh_private_key_path"
	remoteDir           = "~/benchctl"
	remoteBenchctlBin   = remoteDir + "/benchctl"
	remoteScenarioPath  = remoteDir + "/scenario.yaml"
	remoteResumeLog     = remoteDir + "/resume.log"
	remoteStatePath     = remoteDir + "/state.json"

	// cloudInitTimeout bounds how long bootstrap waits for cloud-init to
	// finish. Real bootstraps finish this in well under a minute; a stuck
	// user-data step (e.g. apt-get hanging on a mirror) should fail fast
	// into the scheduler's existing launch-failure/retry path instead of
	// blocking indefinitely.
	cloudInitTimeout = 15 * time.Minute
)

// The Provider satisfies the engine's optional interfaces structurally, so
// signature drift would otherwise surface only at runtime, for example as
// "driver does not implement async bootstrap" after provisioning.
var (
	_ engine.AsyncBootstrapper = (*Provider)(nil)
	_ engine.Connector         = (*Provider)(nil)
	_ engine.BenchctlHost      = (*Provider)(nil)
	_ engine.ArtifactFetcher   = (*Provider)(nil)
)

// Config parameterizes a Provider for a specific cloud.
type Config struct {
	// ProviderName identifies the cloud in log prefixes and error messages,
	// e.g. "ec2" or "gce".
	ProviderName string
	// DefaultSSHUser is used when cfg doesn't set ssh_user explicitly.
	DefaultSSHUser string
	// ArchFromInstanceType maps a vars["instance_type"] value to "arm64" or
	// "amd64", used to select the right local benchctl binary to SCP.
	ArchFromInstanceType func(instanceType string) string
	// Benchctl is the loaded user configuration, used to resolve the store
	// URL/anon key and metrics credentials to pass to the remote driver.
	Benchctl *config.Config
}

// Provider implements engine.DriverProvider, engine.AsyncBootstrapper,
// engine.Connector, and engine.ArtifactFetcher for SSH-reachable load
// generators provisioned via OpenTofu.
type Provider struct {
	cfg Config
	out io.Writer
}

// New constructs a Provider for the given cloud Config.
func New(cfg Config) *Provider { return &Provider{cfg: cfg, out: os.Stderr} }

// Provision runs `tofu apply` in the driver module to start the instance.
//
// The working directory is auto-derived as ./tofu-state/<runID>/<module-basename>,
// so follow-up commands (teardown, status, connect) can locate per-run state by
// run ID.
//
// Expected config keys:
//
//	module             string         path to the tofu module (required)
//	vars               map[string]any tofu variable overrides (optional)
//	ssh_user           string         SSH username (default: cfg.DefaultSSHUser)
//	benchctl_binary    string         local path to benchctl (default: current executable)
func (p *Provider) Provision(ctx context.Context, runID string, cfg map[string]any) (engine.Outputs, error) {
	// Only the "scp" remote binary source needs a local binary prepared
	// ahead of Bootstrap; "fetch" (release channel) has the remote host pull
	// its own binary directly from GitHub once it's up.
	if remoteBinarySource(cfg) == "scp" {
		binPath := p.benchctlBinaryPath(cfg)
		if err := p.ensureBenchctlBinary(binPath); err != nil {
			return nil, fmt.Errorf("%s: %w", p.cfg.ProviderName, err)
		}
	}

	module, err := parseTofuConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.cfg.ProviderName, err)
	}

	rawVars, _ := cfg["vars"].(map[string]any)
	vars := tags.Inject(rawVars, runID, p.cfg.Benchctl)
	vars = p.injectMetricsVars(vars)

	workDir := filepath.Join(tofustate.RunDir(runID), filepath.Base(module))
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, fmt.Errorf("%s: mkdir %s: %w", p.cfg.ProviderName, workDir, err)
	}

	absModule, cleanupModule, err := bundle.Dir(module)
	if err != nil {
		return nil, fmt.Errorf("%s: resolve module path: %w", p.cfg.ProviderName, err)
	}
	defer cleanupModule()

	// Copy .tf/.tftpl/.sh files into workDir; modern OpenTofu no longer accepts
	// a path argument to `tofu init`; the working directory must contain the files.
	consolelog.Println(p.out, fmt.Sprintf("%s: syncing module %s → %s", p.cfg.ProviderName, absModule, workDir))
	if err := syncTFFiles(absModule, workDir); err != nil {
		return nil, fmt.Errorf("%s: sync module: %w", p.cfg.ProviderName, err)
	}

	consolelog.Println(p.out, fmt.Sprintf("%s: tofu init", p.cfg.ProviderName))
	if err := p.runTofu(ctx, workDir, "init", "-input=false"); err != nil {
		return nil, fmt.Errorf("%s: tofu init: %w", p.cfg.ProviderName, err)
	}

	// Persist vars to terraform.tfvars.json so `tofu destroy` picks them up
	// automatically; required variables with no default would otherwise fail
	// teardown when no -var flags are passed.
	if err := writeTFVars(workDir, vars); err != nil {
		return nil, fmt.Errorf("%s: write tfvars: %w", p.cfg.ProviderName, err)
	}

	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("%s: resolve work dir: %w", p.cfg.ProviderName, err)
	}

	applyArgs := []string{"apply", "-auto-approve", "-input=false"}
	consolelog.Println(p.out, fmt.Sprintf("%s: tofu apply", p.cfg.ProviderName))
	if err := p.runTofu(ctx, workDir, applyArgs...); err != nil {
		consolelog.Println(p.out, fmt.Sprintf("%s: tofu apply failed; attempting cleanup", p.cfg.ProviderName))
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cleanupCancel()
		if destroyErr := p.runTofu(cleanupCtx, workDir, "destroy", "-auto-approve", "-input=false"); destroyErr != nil {
			fmt.Fprintf(p.out, "warning: %s: cleanup after failed apply: %v\n", p.cfg.ProviderName, destroyErr)
			// Return work dir so the runner can persist state for manual teardown.
			return engine.Outputs{engine.OutputKeyTofuWorkDir: absWorkDir}, fmt.Errorf("%s: tofu apply: %w", p.cfg.ProviderName, err)
		}
		// Destroy succeeded, so no infrastructure is left to tear down.
		// Returning no outputs means nothing will call Teardown for this run,
		// so remove the local workdir here.
		if err := os.RemoveAll(workDir); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(p.out, "warning: %s: remove workdir %s: %v\n", p.cfg.ProviderName, workDir, err)
		}
		_ = os.Remove(filepath.Dir(workDir)) // best-effort: only succeeds once empty
		return nil, fmt.Errorf("%s: tofu apply: %w", p.cfg.ProviderName, err)
	}

	outputs, err := p.readTofuOutputs(ctx, workDir)
	if err != nil {
		// The apply already succeeded, so real infrastructure exists. Return
		// the work dir despite the unreadable outputs, so `benchctl teardown`
		// can destroy that infrastructure later instead of leaking it.
		return engine.Outputs{engine.OutputKeyTofuWorkDir: absWorkDir}, err
	}
	outputs[engine.OutputKeyTofuWorkDir] = absWorkDir
	return outputs, nil
}

// Setup is a no-op when used in synchronous mode. In async mode, driver setup
// runs on the remote instance itself via Bootstrap + `benchctl resume`.
func (*Provider) Setup(_ context.Context, _ map[string]any, _ engine.Outputs) error {
	return nil
}

// Collect is otherwise a pass-through -- metrics are produced in-process on
// the remote machine and written to the collector directly by `benchctl
// resume` -- aside from folding in any driver-side CPU/network utilization
// Vector captured (if present) into the result.
func (p *Provider) Collect(_ context.Context, _ map[string]any, _ engine.Outputs, metrics engine.Metrics) (engine.Metrics, error) {
	return hostmetrics.AppendTo(metrics, "", p.out), nil
}

// Teardown runs `tofu destroy` using the work dir stored in outputs.
func (p *Provider) Teardown(ctx context.Context, outputs engine.Outputs) error {
	workDir := outputs[engine.OutputKeyTofuWorkDir]
	if workDir == "" {
		return fmt.Errorf("%s: teardown: missing tofu work dir in outputs", p.cfg.ProviderName)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".terraform")); os.IsNotExist(err) {
		consolelog.Println(p.out, fmt.Sprintf("%s: init (provider cache missing, re-initializing)", p.cfg.ProviderName))
		if err := p.runTofu(ctx, workDir, "init", "-input=false"); err != nil {
			return fmt.Errorf("%s: tofu init: %w", p.cfg.ProviderName, err)
		}
	}
	consolelog.Println(p.out, fmt.Sprintf("%s: tofu destroy", p.cfg.ProviderName))
	if err := p.runTofu(ctx, workDir, "destroy", "-auto-approve", "-input=false"); err != nil {
		return fmt.Errorf("%s: tofu destroy: %w", p.cfg.ProviderName, err)
	}

	// Remove local state only after a successful destroy so the workdir
	// remains for debugging on any failure.
	if err := os.RemoveAll(workDir); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(p.out, "warning: %s: remove workdir %s: %v\n", p.cfg.ProviderName, workDir, err)
	}
	_ = os.Remove(filepath.Dir(workDir)) // best-effort: only succeeds once empty

	return nil
}

// Bootstrap implements engine.AsyncBootstrapper. It:
//  1. Delivers the benchctl binary to the driver, via one of two channels
//     (see remoteBinarySource): "scp" transfers a locally built/cross-compiled
//     binary; "fetch" has the driver download the matching tagged release
//     directly from GitHub.
//  2. SCPs the scenario YAML to the instance.
//  3. Imports req.StateSeed into the driver's own store, when the
//     orchestrator's store is one the driver cannot read.
//  4. SSHs to run `benchctl resume <runID> --scenario <path>` as a detached
//     background process, passing store credentials as environment variables.
func (p *Provider) Bootstrap(ctx context.Context, req engine.BootstrapRequest) error {
	cfg, driverOutputs, runID := req.Config, req.DriverOutputs, req.RunID
	ip := driverOutputs[outputKeyPublicIP]
	if ip == "" {
		return fmt.Errorf("%s: bootstrap: missing public_ip in driver outputs", p.cfg.ProviderName)
	}
	keyPath := driverOutputs[outputKeySSHKeyPath]
	sshUser := p.sshUser(cfg)

	driverToken, err := resolveDriverToken(p.cfg.Benchctl)
	if err != nil {
		return fmt.Errorf("%s: bootstrap: %w", p.cfg.ProviderName, err)
	}

	target := fmt.Sprintf("%s@%s", sshUser, ip)

	// Wait for sshd; the instance exists in the cloud but needs time to boot.
	consolelog.Println(p.out, fmt.Sprintf("%s: bootstrap: waiting for SSH on %s", p.cfg.ProviderName, ip))
	if err := waitForSSH(ctx, ip, 2*time.Minute); err != nil {
		return fmt.Errorf("%s: bootstrap: wait for SSH: %w", p.cfg.ProviderName, err)
	}

	// Wait for cloud-init to finish so startup-script tools (go-tpc, etc.) are ready.
	consolelog.Println(p.out, fmt.Sprintf("%s: bootstrap: waiting for cloud-init to complete", p.cfg.ProviderName))
	cloudInitCtx, cancelCloudInit := context.WithTimeout(ctx, cloudInitTimeout)
	err = p.ssh(cloudInitCtx, keyPath, target, "cloud-init status --wait")
	timedOut := cloudInitCtx.Err() == context.DeadlineExceeded
	cancelCloudInit()
	if err != nil {
		if timedOut {
			return fmt.Errorf("%s: bootstrap: cloud-init wait exceeded %s: %w", p.cfg.ProviderName, cloudInitTimeout, err)
		}
		return fmt.Errorf("%s: bootstrap: cloud-init wait: %w", p.cfg.ProviderName, err)
	}

	consolelog.Println(p.out, fmt.Sprintf("%s: bootstrap: preparing remote directory", p.cfg.ProviderName))
	if err := p.ssh(ctx, keyPath, target, "mkdir -p "+remoteDir); err != nil {
		return fmt.Errorf("%s: bootstrap: mkdir: %w", p.cfg.ProviderName, err)
	}

	switch remoteBinarySource(cfg) {
	case "fetch":
		arch := p.targetArch(cfg)
		consolelog.Println(p.out, fmt.Sprintf("%s: bootstrap: fetching benchctl %s release binary on remote (%s)", p.cfg.ProviderName, buildinfo.Version, arch))
		if err := p.fetchReleaseBinary(ctx, keyPath, target, arch); err != nil {
			return fmt.Errorf("%s: bootstrap: fetch benchctl release: %w", p.cfg.ProviderName, err)
		}
	default: // "scp"
		benchctlBin := p.benchctlBinaryPath(cfg)
		consolelog.Println(p.out, fmt.Sprintf("%s: bootstrap: SCP benchctl binary", p.cfg.ProviderName))
		if err := p.scp(ctx, keyPath, benchctlBin, scpHost(target)+":"+remoteBenchctlBin); err != nil {
			return fmt.Errorf("%s: bootstrap: scp benchctl: %w", p.cfg.ProviderName, err)
		}
		if err := p.ssh(ctx, keyPath, target, "chmod +x "+remoteBenchctlBin); err != nil {
			return fmt.Errorf("%s: bootstrap: chmod: %w", p.cfg.ProviderName, err)
		}
	}

	// SCP scenario YAML.
	absScenario, err := filepath.Abs(req.ScenarioPath)
	if err != nil {
		return fmt.Errorf("%s: bootstrap: resolve scenario path: %w", p.cfg.ProviderName, err)
	}
	consolelog.Println(p.out, fmt.Sprintf("%s: bootstrap: SCP scenario", p.cfg.ProviderName))
	if err := p.scp(ctx, keyPath, absScenario, scpHost(target)+":"+remoteScenarioPath); err != nil {
		return fmt.Errorf("%s: bootstrap: scp scenario: %w", p.cfg.ProviderName, err)
	}

	// SCP every subdirectory next to the scenario YAML into remoteDir so that
	// relative paths in suite steps (e.g. "edge/100_funcs/load.js") resolve to
	// real files when benchctl resume runs on the driver. Sibling YAML files
	// are intentionally skipped: only the scenario being run is uploaded
	// (as scenario.yaml above).
	scenarioDir := filepath.Dir(absScenario)
	entries, err := os.ReadDir(scenarioDir)
	if err != nil {
		return fmt.Errorf("%s: bootstrap: read scenario dir: %w", p.cfg.ProviderName, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		src := filepath.Join(scenarioDir, e.Name())
		consolelog.Println(p.out, fmt.Sprintf("%s: bootstrap: SCP scripts %s", p.cfg.ProviderName, e.Name()))
		if err := p.scpDir(ctx, keyPath, src, scpHost(target)+":"+remoteDir+"/"); err != nil {
			return fmt.Errorf("%s: bootstrap: scp scripts %s: %w", p.cfg.ProviderName, e.Name(), err)
		}
	}

	// Seed the run record into the driver's own store. Without it `benchctl
	// resume` there has nothing to load, since a store that is local to the
	// orchestrator holds the only copy.
	if len(req.StateSeed) > 0 {
		consolelog.Println(p.out, fmt.Sprintf("%s: bootstrap: seeding run state", p.cfg.ProviderName))
		if err := p.seedState(ctx, keyPath, target, req.StateSeed); err != nil {
			return fmt.Errorf("%s: bootstrap: seed run state: %w", p.cfg.ProviderName, err)
		}
	}

	// SSH: start benchctl resume as a detached background process.
	resumeCmd := buildResumeCmd(p.cfg.Benchctl, driverToken, runID)
	consolelog.Println(p.out, fmt.Sprintf("%s: bootstrap: starting benchctl resume", p.cfg.ProviderName))
	if err := p.ssh(ctx, keyPath, target, resumeCmd); err != nil {
		return fmt.Errorf("%s: bootstrap: resume: %w", p.cfg.ProviderName, err)
	}

	consolelog.Println(p.out, fmt.Sprintf("%s: bootstrap complete; resume log at %s:%s", p.cfg.ProviderName, ip, remoteResumeLog))
	return nil
}

// buildResumeCmd renders the shell command that starts `benchctl resume` on
// the driver instance, prefixed with every BENCHCTL_* variable needed to
// reproduce cfg there (see config.Config.RemoteEnv). The driver instance has no
// config file, so the store settings, BENCHCTL_STORE_MODE and
// BENCHCTL_STORE_ANON_KEY included, must travel in this command or the remote
// process cannot reach the shared store. driverToken replaces whatever auth
// token cfg carries, since the remote process needs the driver-specific
// credential. It emits assignments in sorted order so the command stays stable
// across invocations.
func buildResumeCmd(cfg *config.Config, driverToken, runID string) string {
	remoteEnv := cfg.RemoteEnv()
	remoteEnv["BENCHCTL_STORE_AUTH_TOKEN"] = driverToken

	envKeys := make([]string, 0, len(remoteEnv))
	for k := range remoteEnv {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)

	envAssignments := make([]string, 0, len(envKeys))
	for _, k := range envKeys {
		envAssignments = append(envAssignments, k+"="+shellQuote(remoteEnv[k]))
	}

	return fmt.Sprintf(
		"%s nohup %s resume %s --scenario %s > %s 2>&1 &",
		strings.Join(envAssignments, " "),
		remoteBenchctlBin, shellQuote(runID),
		remoteScenarioPath, remoteResumeLog,
	)
}

// seedState writes the run record to the driver instance and imports it into the
// store there. It goes via a file rather than stdin because scp is already
// how everything else in Bootstrap reaches the instance.
func (p *Provider) seedState(ctx context.Context, keyPath, target string, seed []byte) error {
	local, err := os.CreateTemp("", "benchctl-state-*.json")
	if err != nil {
		return fmt.Errorf("write seed: %w", err)
	}
	defer os.Remove(local.Name())
	if _, err := local.Write(seed); err != nil {
		local.Close()
		return fmt.Errorf("write seed: %w", err)
	}
	if err := local.Close(); err != nil {
		return fmt.Errorf("write seed: %w", err)
	}

	if err := p.scp(ctx, keyPath, local.Name(), scpHost(target)+":"+remoteStatePath); err != nil {
		return fmt.Errorf("scp seed: %w", err)
	}
	if err := p.ssh(ctx, keyPath, target, buildStateImportCmd()); err != nil {
		return fmt.Errorf("import seed: %w", err)
	}
	return nil
}

// buildStateImportCmd renders the command that loads the seeded record into
// the driver instance's own store.
//
// BENCHCTL_STORE_MODE is pinned to local regardless of how the orchestrator
// is configured: the driver is the machine executing this run, so its record
// is the authoritative one, and it must not be written to a store the
// orchestrator would then read back from the driver.
func buildStateImportCmd() string {
	return fmt.Sprintf("BENCHCTL_STORE_MODE=local %s state import %s", remoteBenchctlBin, remoteStatePath)
}

// RemoteBenchctlPath implements engine.BenchctlHost: where Bootstrap installs
// the binary, so a machine-local store can read a run's state back from here.
func (*Provider) RemoteBenchctlPath() string { return remoteBenchctlBin }

// ConnectArgs implements engine.Connector. It returns the SSH argv for an
// interactive shell session on the driver instance.
func (p *Provider) ConnectArgs(outputs engine.Outputs) ([]string, error) {
	ip := outputs[outputKeyPublicIP]
	if ip == "" {
		return nil, fmt.Errorf("%s: connect: missing public_ip in driver outputs", p.cfg.ProviderName)
	}
	keyPath := outputs[outputKeySSHKeyPath]
	args := append([]string{"ssh"}, hostKeyArgs(keyPath)...)
	if keyPath != "" {
		args = append(args, "-i", keyPath)
	}
	args = append(args, p.cfg.DefaultSSHUser+"@"+ip)
	return args, nil
}

// FetchArtifacts implements engine.ArtifactFetcher. It packages the resume
// log and any result files the stdout collector wrote to $HOME (see
// internal/collectors/stdout) into a single tarball on the instance, so one
// scp transfer suffices regardless of how a given OpenSSH version handles
// remote glob expansion for multi-file scp, then extracts it into localDest.
// Missing files are silently skipped: fetch is expected to be called even
// when the run hasn't reached the phase that writes some or all of them yet.
func (p *Provider) FetchArtifacts(ctx context.Context, outputs engine.Outputs, localDest string) error {
	ip := outputs[outputKeyPublicIP]
	if ip == "" {
		return fmt.Errorf("%s: fetch: missing public_ip in driver outputs", p.cfg.ProviderName)
	}
	keyPath := outputs[outputKeySSHKeyPath]
	target := p.cfg.DefaultSSHUser + "@" + ip

	const remoteTar = "/tmp/benchctl-fetch.tar.gz"
	packCmd := "set -e; d=$(mktemp -d); " +
		"for f in benchctl/resume.log results_*.json raw_samples_*.csv; do cp -p $HOME/$f \"$d/\" 2>/dev/null || true; done; " +
		"tar -czf " + remoteTar + " -C \"$d\" .; rm -rf \"$d\""
	if err := p.ssh(ctx, keyPath, target, packCmd); err != nil {
		return fmt.Errorf("%s: fetch: package artifacts on driver: %w", p.cfg.ProviderName, err)
	}
	defer func() {
		// Best-effort: a leftover tmp file on an instance about to be torn
		// down isn't worth failing the fetch over.
		_ = p.ssh(context.Background(), keyPath, target, "rm -f "+remoteTar)
	}()

	if err := os.MkdirAll(localDest, 0o755); err != nil {
		return fmt.Errorf("%s: fetch: mkdir %s: %w", p.cfg.ProviderName, localDest, err)
	}
	localTar := filepath.Join(localDest, ".benchctl-fetch.tar.gz")
	if err := p.scp(ctx, keyPath, scpHost(target)+":"+remoteTar, localTar); err != nil {
		return fmt.Errorf("%s: fetch: download artifacts: %w", p.cfg.ProviderName, err)
	}
	defer os.Remove(localTar)

	if err := extractTarGz(localTar, localDest); err != nil {
		return fmt.Errorf("%s: fetch: extract artifacts: %w", p.cfg.ProviderName, err)
	}
	return nil
}

// extractTarGz extracts a local gzip-compressed tar archive into destDir,
// which must already exist. Shells out to the system tar binary, same as the
// rest of this package does for ssh/scp rather than reimplementing a remote
// protocol in Go.
func extractTarGz(archivePath, destDir string) error {
	cmd := exec.Command("tar", "-xzf", archivePath, "-C", destDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tar -xzf: %w: %s", err, out)
	}
	return nil
}

func (p *Provider) ssh(ctx context.Context, keyPath, target, cmd string) error {
	args := append(hostKeyArgs(keyPath), "-o", "BatchMode=yes")
	if keyPath != "" {
		args = append(args, "-i", keyPath)
	}
	args = append(args, target, cmd)
	c := exec.CommandContext(ctx, "ssh", args...)
	c.Stdout = p.out
	c.Stderr = p.out
	return c.Run()
}

// scpHost renders a user@host prefix safe to use in an scp target. scp splits
// its argument on the first colon to separate host from path, so a bare IPv6
// literal such as 2600:1f18::1 is parsed as host "2600" and path "1f18::1".
// Bracketing the literal is the documented way to disambiguate. IPv4 hosts and
// DNS names are returned unchanged, and an already-bracketed host is left
// alone so callers cannot double-bracket.
func scpHost(target string) string {
	user, host, found := strings.Cut(target, "@")
	if !found {
		user, host = "", target
	}
	if !strings.HasPrefix(host, "[") && net.ParseIP(host) != nil && strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if user == "" {
		return host
	}
	return user + "@" + host
}

func (p *Provider) scp(ctx context.Context, keyPath, src, dst string) error {
	args := append(hostKeyArgs(keyPath), "-o", "BatchMode=yes")
	if keyPath != "" {
		args = append(args, "-i", keyPath)
	}
	args = append(args, src, dst)
	c := exec.CommandContext(ctx, "scp", args...)
	c.Stdout = p.out
	c.Stderr = p.out
	return c.Run()
}

func (p *Provider) scpDir(ctx context.Context, keyPath, src, dst string) error {
	args := append([]string{"-r"}, hostKeyArgs(keyPath)...)
	args = append(args, "-o", "BatchMode=yes")
	if keyPath != "" {
		args = append(args, "-i", keyPath)
	}
	args = append(args, src, dst)
	c := exec.CommandContext(ctx, "scp", args...)
	c.Stdout = p.out
	c.Stderr = p.out
	return c.Run()
}

// hostKeyArgs returns the ssh/scp host-key-verification flags for a
// connection whose private key lives at keyPath. See the identical helper in
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

func (p *Provider) runTofu(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "tofu", args...)
	cmd.Dir = dir
	cmd.Stdout = p.out
	cmd.Stderr = p.out
	fmt.Fprintf(p.out, "$ tofu %s\n", strings.Join(args, " "))
	return cmd.Run()
}

func (p *Provider) readTofuOutputs(ctx context.Context, workDir string) (engine.Outputs, error) {
	cmd := exec.CommandContext(ctx, "tofu", "output", "-json")
	cmd.Dir = workDir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = p.out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: tofu output: %w", p.cfg.ProviderName, err)
	}
	var raw map[string]struct {
		Value any `json:"value"`
	}
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		return nil, fmt.Errorf("%s: parse tofu outputs: %w", p.cfg.ProviderName, err)
	}
	out := make(engine.Outputs, len(raw))
	for k, v := range raw {
		out[k] = fmt.Sprintf("%v", v.Value)
	}
	return out, nil
}

func parseTofuConfig(cfg map[string]any) (module string, err error) {
	module, _ = cfg["module"].(string)
	if module == "" {
		return "", fmt.Errorf("config.module is required")
	}
	return module, nil
}

func (p *Provider) sshUser(cfg map[string]any) string {
	if u, _ := cfg["ssh_user"].(string); u != "" {
		return u
	}
	return p.cfg.DefaultSSHUser
}

// waitForSSH polls port 22 on host until it accepts a TCP connection or the
// context or timeout expires. A successful TCP dial is enough to know sshd is
// ready, so it performs no SSH handshake.
func waitForSSH(ctx context.Context, host string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	addr := net.JoinHostPort(host, "22")
	for {
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err == nil {
			conn.Close()
			time.Sleep(1 * time.Second)
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("SSH not ready on %s after %s: %w", host, timeout, err)
		case <-time.After(5 * time.Second):
		}
	}
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
	if ep := p.cfg.Benchctl.Metrics.Endpoint; ep != "" {
		vars["vector_sink_victoriametrics_endpoint"] = ep
	}
	if tok := p.cfg.Benchctl.Metrics.Token; tok != "" {
		vars["vector_sink_victoriametrics_token"] = tok
	}
	return vars
}

// syncTFFiles copies every *.tf, *.tftpl, and *.sh file from srcDir into
// dstDir, overwriting any existing copies. This lets workDir own the tofu
// state while the module source directory stays read-only. The *.tftpl and
// *.sh files come along so that templatefile() and file() references still
// resolve; the *.sh files cover scripts symlinked into the module directory,
// such as a shared tuning script. os.ReadFile follows symlinks, so a symlink
// to a file outside srcDir lands in dstDir as a real file.
func syncTFFiles(srcDir, dstDir string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.TrimPrefix(filepath.Ext(e.Name()), ".")
		if ext != "tf" && ext != "tftpl" && ext != "sh" {
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

// resolveDriverToken returns the credential to pass to the driver for writing
// back to the run state store. Resolution order:
//  1. cfg.Store.ServiceRoleKey (CI and superuser path)
//  2. User JWT from ~/.benchctl/credentials (interactive user path)
//
// Note: the driver receives only the access token, not the refresh token, so it
// cannot renew its credentials. Store state updates on the driver will silently
// fail once the token expires; the benchmark itself continues. Use the service
// role key path for CI or unattended long-running benchmarks.
func resolveDriverToken(cfg *config.Config) (string, error) {
	if cfg.Store.ServiceRoleKey != "" {
		return cfg.Store.ServiceRoleKey, nil
	}
	creds, err := auth.LoadCredentials()
	if err == nil && creds.Valid() {
		return creds.AccessToken, nil
	}
	return "", fmt.Errorf("no auth token available for the driver: set BENCHCTL_STORE_SERVICE_ROLE_KEY or run 'benchctl auth login'")
}

// benchctlBinaryPath returns the local path to the benchctl binary for the
// driver's target architecture. If benchctl_binary is explicitly set in cfg it
// is used as-is (escape hatch); otherwise the path is derived from the
// instance_type in cfg.vars via cfg.ArchFromInstanceType.
func (p *Provider) benchctlBinaryPath(cfg map[string]any) string {
	if v, _ := cfg["benchctl_binary"].(string); v != "" {
		return v
	}
	name := "benchctl-linux-" + p.targetArch(cfg)
	if buildinfo.SourceDir != "" {
		return filepath.Join(buildinfo.SourceDir, "bin", name)
	}
	return filepath.Join("bin", name)
}

// targetArch returns "amd64" or "arm64" for the driver instance, based on
// cfg.vars.instance_type via cfg.ArchFromInstanceType.
func (p *Provider) targetArch(cfg map[string]any) string {
	vars, _ := cfg["vars"].(map[string]any)
	instanceType, _ := vars["instance_type"].(string)
	return p.cfg.ArchFromInstanceType(instanceType)
}

// remoteBinarySource decides how Bootstrap delivers the benchctl binary to
// the remote host: "scp" (build/cross-compile locally, then SCP; the only
// option that can carry a developer's uncommitted changes) or "fetch" (the
// remote host downloads the matching tagged release directly from GitHub,
// no SCP of a multi-tens-of-MB binary needed).
//
// Default is derived from buildinfo.BuildKind: "dev" builds use "scp",
// "release" builds use "fetch". cfg["remote_binary_source"] is an escape
// hatch to force either channel, e.g. for an air-gapped remote target on a
// release build.
func remoteBinarySource(cfg map[string]any) string {
	if v, _ := cfg["remote_binary_source"].(string); v == "scp" || v == "fetch" {
		return v
	}
	if buildinfo.BuildKind == "release" {
		return "fetch"
	}
	return "scp"
}

// ensureBenchctlBinary guarantees that a benchctl binary exists at binPath.
// Only the "scp" remote binary source calls it (see remoteBinarySource);
// release-channel bootstraps need no local binary.
//
// With a live checkout (buildinfo.BuildKind == "dev" and buildinfo.SourceDir
// set) it always rebuilds via `mise run build-linux`, run from the checkout
// root rather than the process's CWD so that benchctl works when invoked from
// an arbitrary directory. Without a checkout to build from, the binary must
// already exist at binPath.
func (p *Provider) ensureBenchctlBinary(binPath string) error {
	if buildinfo.BuildKind != "dev" || buildinfo.SourceDir == "" {
		if _, err := os.Stat(binPath); err == nil {
			return nil // available
		}
		return fmt.Errorf("benchctl binary %q not found and no live checkout to build it from", binPath)
	}
	consolelog.Println(p.out, fmt.Sprintf("%s: building %s with: mise run build-linux", p.cfg.ProviderName, binPath))
	// Do NOT use CommandContext: interrupting a build mid-way leaves a broken binary.
	cmd := exec.Command("mise", "run", "build-linux")
	cmd.Dir = buildinfo.SourceDir
	cmd.Stdout = p.out
	cmd.Stderr = p.out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build benchctl binary: mise run build-linux: %w", err)
	}
	if _, err := os.Stat(binPath); err != nil {
		return fmt.Errorf("mise run build-linux completed but %q was not produced", binPath)
	}
	return nil
}

// fetchReleaseBinary has the remote host (target, over SSH) download the
// benchctl release binary matching buildinfo.Version directly from GitHub,
// verify it against the release's checksums.txt, and install it at
// remoteBenchctlBin. Mirrors the go-tpc install pattern already used in
// cloud-init user_data (see deployments/ec2/loaddriver_gotpc/main.tf).
func (p *Provider) fetchReleaseBinary(ctx context.Context, keyPath, target, arch string) error {
	binURL := fmt.Sprintf("https://github.com/%s/releases/download/%s/benchctl-linux-%s", buildinfo.Repo, buildinfo.Version, arch)
	checksumsURL := fmt.Sprintf("https://github.com/%s/releases/download/%s/checksums.txt", buildinfo.Repo, buildinfo.Version)
	cmd := fmt.Sprintf(`set -euo pipefail
curl -fsSL --retry 5 --retry-delay 10 -o /tmp/benchctl %s
curl -fsSL --retry 5 --retry-delay 10 -o /tmp/checksums.txt %s
(cd /tmp && grep ' benchctl-linux-%s$' checksums.txt | sha256sum -c -)
chmod +x /tmp/benchctl
mv /tmp/benchctl %s`,
		shellQuote(binURL), shellQuote(checksumsURL), arch, remoteBenchctlBin)
	return p.ssh(ctx, keyPath, target, cmd)
}

// shellQuote wraps s in single quotes, escaping any single quotes within.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
