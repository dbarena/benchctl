package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/dbarena/benchctl/internal/adapters/gotpc"
	"github.com/dbarena/benchctl/internal/adapters/k6"
	"github.com/dbarena/benchctl/internal/adapters/shell"
	"github.com/dbarena/benchctl/internal/auth"
	stdoutcollector "github.com/dbarena/benchctl/internal/collectors/stdout"
	vmcollector "github.com/dbarena/benchctl/internal/collectors/victoriametrics"
	"github.com/dbarena/benchctl/internal/config"
	"github.com/dbarena/benchctl/internal/engine"
	"github.com/dbarena/benchctl/internal/providers/dockercompose"
	"github.com/dbarena/benchctl/internal/providers/ec2"
	"github.com/dbarena/benchctl/internal/providers/gce"
	"github.com/dbarena/benchctl/internal/providers/gcpcloudsql"
	"github.com/dbarena/benchctl/internal/providers/local"
	opentofuprovider "github.com/dbarena/benchctl/internal/providers/opentofu"
	supabaseprovider "github.com/dbarena/benchctl/internal/providers/supabase"
	"github.com/dbarena/benchctl/internal/schema"
)

var (
	setFlags        []string
	asyncRun        bool
	noTeardown      bool
	lingerOnFailure bool
	runIDFlag       string
)

var runCmd = &cobra.Command{
	Use:   "run <scenario-file>",
	Short: "Run a benchmark scenario",
	Args:  cobra.ExactArgs(1),
	RunE:  runScenario,
}

func init() {
	runCmd.Flags().StringArrayVar(&setFlags, "set", nil, "Override a scenario input (key=value, repeatable)")
	runCmd.Flags().BoolVar(&asyncRun, "async", false, "Provision, hand off to the remote driver, then exit immediately and print the run ID")
	runCmd.Flags().BoolVar(&noTeardown, "no-teardown", false, "Skip teardown; leave infrastructure running for post-run inspection")
	runCmd.Flags().BoolVar(&lingerOnFailure, "linger-on-failure", false, "Skip teardown when the run fails; leave infrastructure running for post-failure inspection")
	runCmd.Flags().StringVar(&runIDFlag, "run-id", "", "Use this run ID instead of generating one; fails if it already exists")
}

func runScenario(_ *cobra.Command, args []string) error {
	scenarioPath := args[0]
	s, err := schema.Load(scenarioPath)
	if err != nil {
		return err
	}
	if err := s.Validate(); err != nil {
		return fmt.Errorf("invalid scenario: %w", err)
	}

	overrides, err := parseSetFlags(setFlags)
	if err != nil {
		return err
	}

	inputs, err := s.ResolveInputs(overrides)
	if err != nil {
		return err
	}

	store, err := openStore()
	if err != nil {
		return err
	}

	runner, err := buildRunner(appCfg, s, inputs)
	if err != nil {
		return err
	}
	runner.Store = store
	runner.ScenarioPath = scenarioPath
	runner.CreatedBy, runner.CreatedByEmail = resolveCreatedBy(appCfg)
	runner.NoTeardown = noTeardown
	runner.LingerOnFailure = lingerOnFailure
	runner.RunID = runIDFlag

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if asyncRun {
		runID, err := runner.RunAsync(ctx, s, inputs)
		if err != nil {
			return err
		}
		fmt.Println(runID)
		return nil
	}

	return runner.Run(ctx, s, inputs)
}

// buildRunner wires up the provider, adapter, and collector implementations for a
// scenario. It resolves provider and adapter names through inputs, so a scenario
// file can parameterize them with {{ inputs.<name> }} and override them with
// --set at runtime.
func buildRunner(cfg *config.Config, s *schema.Scenario, inputs schema.ResolvedInputs) (engine.Runner, error) {
	targetProvider, err := resolveProvider(s.Target.Provider, inputs)
	if err != nil {
		return engine.Runner{}, err
	}
	driverProvider, err := resolveProvider(s.Driver.Provider, inputs)
	if err != nil {
		return engine.Runner{}, err
	}
	collectorProvider, err := resolveProvider(s.Collector.Provider, inputs)
	if err != nil {
		return engine.Runner{}, err
	}

	target, err := buildTargetProvider(cfg, targetProvider)
	if err != nil {
		return engine.Runner{}, err
	}
	driver, err := buildDriverProvider(cfg, driverProvider)
	if err != nil {
		return engine.Runner{}, err
	}
	workloads, err := buildWorkloadAdapters(s.Suite.Benchmarks)
	if err != nil {
		return engine.Runner{}, err
	}
	collector, err := buildCollector(cfg, collectorProvider)
	if err != nil {
		return engine.Runner{}, err
	}

	return engine.Runner{
		Target:    target,
		Driver:    driver,
		Workloads: workloads,
		Collector: collector,
		Out:       os.Stderr,
	}, nil
}

func buildWorkloadAdapters(suite []schema.SuiteEntry) (map[string]engine.WorkloadAdapter, error) {
	adapters := make(map[string]engine.WorkloadAdapter)
	for _, entry := range suite {
		for _, step := range entry.Steps {
			// The runner handles built-in step types directly; they have no
			// pluggable adapter to wire up here.
			if step.Type == "metadata" || step.Type == "sql" || step.Type == "collect" {
				continue
			}
			if _, ok := adapters[step.Type]; ok {
				continue
			}
			adapter, err := buildWorkloadAdapter(step.Type)
			if err != nil {
				return nil, err
			}
			adapters[step.Type] = adapter
		}
	}
	return adapters, nil
}

// resolveProvider resolves a provider/adapter name that may contain {{ inputs.<name> }}
// placeholders. Plain strings (no template expressions) are returned unchanged.
func resolveProvider(raw string, inputs schema.ResolvedInputs) (string, error) {
	tc := &engine.TemplateContext{Inputs: inputs}
	resolved, err := engine.Resolve(raw, tc)
	if err != nil {
		return "", fmt.Errorf("resolve provider %q: %w", raw, err)
	}
	return resolved, nil
}

func buildTargetProvider(cfg *config.Config, name string) (engine.TargetProvider, error) {
	switch name {
	case "local", "noop":
		return local.New(), nil
	case "docker-compose":
		return dockercompose.New(), nil
	case "opentofu":
		return opentofuprovider.New(gcpcloudsql.PreApply, cfg), nil
	case "supabase":
		return supabaseprovider.New(cfg), nil
	default:
		return nil, fmt.Errorf("unsupported target provider %q", name)
	}
}

func buildDriverProvider(cfg *config.Config, name string) (engine.DriverProvider, error) {
	switch name {
	case "local":
		return local.New(), nil
	case "ec2":
		return ec2.New(cfg), nil
	case "gce":
		return gce.New(cfg), nil
	default:
		return nil, fmt.Errorf("unsupported driver provider %q", name)
	}
}

func buildWorkloadAdapter(name string) (engine.WorkloadAdapter, error) {
	switch name {
	case "go-tpc":
		return gotpc.New(), nil
	case "k6":
		return k6.New(), nil
	case "shell":
		return shell.New(), nil
	default:
		return nil, fmt.Errorf("unsupported workload adapter %q", name)
	}
}

func buildCollector(cfg *config.Config, name string) (engine.CollectorProvider, error) {
	switch name {
	case "stdout":
		return stdoutcollector.New(), nil
	case "victoriametrics":
		return vmcollector.New(cfg), nil
	default:
		return nil, fmt.Errorf("unsupported collector %q", name)
	}
}

// resolveCreatedBy returns the identity to store in created_by, plus a
// human-readable email for display (empty unless resolved from a JWT).
// A valid JWT wins over cfg.Username: the JWT's sub is the Supabase user UUID
// and must match auth.uid() for the RLS policy on runs to accept the insert.
// cfg.Username (e.g. "ci/nightly", or "<owner>/<repo>:<workflow>:<job>" set by
// CI) applies only when there is no valid session. The local store ignores it,
// and a service role bypasses RLS.
func resolveCreatedBy(cfg *config.Config) (createdBy, createdByEmail string) {
	creds, _ := auth.LoadCredentials()
	return resolveCreatedByFrom(cfg.Username, creds)
}

// resolveCreatedByFrom implements the precedence documented on resolveCreatedBy,
// factored out so it's testable without touching ~/.benchctl/credentials.
func resolveCreatedByFrom(username string, creds *auth.Credentials) (createdBy, createdByEmail string) {
	if creds != nil && creds.Valid() {
		if sub := jwtSub(creds.AccessToken); sub != "" {
			return sub, jwtEmail(creds.AccessToken)
		}
	}
	if username != "" {
		return username, ""
	}
	return "", ""
}

// jwtSub extracts the sub claim from a JWT without validating the signature.
// The sub is the Supabase user UUID; it matches auth.uid() in RLS policies.
func jwtSub(token string) string {
	parts := strings.SplitN(token, ".", 3)
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.Sub
}

// jwtEmail extracts the email claim from a JWT without validating the signature.
// Used for display only; the UUID (sub) is what gets stored and used for RLS.
func jwtEmail(token string) string {
	parts := strings.SplitN(token, ".", 3)
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.Email
}

// parseSetFlags converts ["key=value", ...] from --set flags into a map.
func parseSetFlags(sets []string) (map[string]string, error) {
	overrides := make(map[string]string, len(sets))
	for _, s := range sets {
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			return nil, fmt.Errorf("--set %q: expected key=value format", s)
		}
		overrides[k] = v
	}
	return overrides, nil
}
