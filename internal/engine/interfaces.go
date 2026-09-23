package engine

import (
	"context"

	"github.com/dbarena/benchctl/internal/schema"
)

// Outputs is the typed key/value map a provider returns after provisioning
// (e.g. host, port, subnet_id).
type Outputs map[string]string

// OutputKeyTofuWorkDir is the Outputs key the opentofu provider uses to carry
// the working directory path between Provision and Teardown.
const OutputKeyTofuWorkDir = "_tofu_work_dir"

// Metrics is the opaque result produced by a workload run and consumed by the
// collector. Adapters populate it; collectors store or print it.
type Metrics map[string]any

// TargetProvider provisions and tears down the database under test.
// cfg is the scenario's target.config with templates already resolved.
// runID identifies the current run and is used by providers that need a
// stable per-run working directory (e.g. opentofu state).
//
// Provision must never create real infrastructure and then return a bare
// error once a later step fails: the runner (and `benchctl teardown`) only
// ever act on Outputs returned alongside an error. If a later step fails
// after infrastructure already exists, Provision must do one of two things:
//
//  1. Tear that infrastructure down itself before returning.
//  2. Return enough Outputs for Teardown to finish the job.
//
// Otherwise the resource leaks, with no local or remote record of it.
type TargetProvider interface {
	Provision(ctx context.Context, runID string, cfg map[string]any) (Outputs, error)
	Teardown(ctx context.Context, outputs Outputs) error
}

// DriverProvider provisions, sets up, collects results from, and tears down
// the driver instance that generates the load.
//
// For the "local" provider, Provision and Teardown are no-ops, Setup is a
// no-op, and Collect passes workload metrics through unchanged.
//
// Provision is subject to the same leak-avoidance contract as
// TargetProvider.Provision (see its doc comment): never return a bare error
// once real infrastructure exists without either tearing it down or
// returning enough Outputs for Teardown to finish the job.
type DriverProvider interface {
	Provision(ctx context.Context, runID string, cfg map[string]any) (Outputs, error)
	// Setup installs tools, downloads datasets, and runs fixture scripts on
	// the driver instance once the target is ready.
	Setup(ctx context.Context, cfg map[string]any, targetOutputs Outputs) error
	// Collect gathers results from wherever the workload tool wrote them,
	// transforms them, and returns final Metrics for the collector. For local
	// drivers this is a pass-through; remote drivers may SSH or read from S3.
	Collect(ctx context.Context, cfg map[string]any, driverOutputs Outputs, workloadMetrics Metrics) (Metrics, error)
	Teardown(ctx context.Context, outputs Outputs) error
}

// WorkloadAdapter runs a single workload step of a benchmark.
// targetOutputs carries the connection details emitted by the TargetProvider.
// step holds the command and already-resolved args from the scenario YAML.
type WorkloadAdapter interface {
	// Run executes the step and returns the metrics it recorded. A step that
	// only creates schema or loads data measures nothing and returns empty
	// Metrics; which commands those are is the adapter's business.
	Run(ctx context.Context, targetOutputs Outputs, step schema.SuiteStep) (Metrics, error)
}

// WorkloadVersionInfo is an optional interface a WorkloadAdapter may
// implement to report the load generator's own version/commit, merged into
// collector labels for every metric emitted during the run. Best-effort: an
// adapter should swallow its own errors and return nil rather than fail the
// run; this is descriptive metadata, not load-bearing.
type WorkloadVersionInfo interface {
	VersionInfo(ctx context.Context) map[string]string
}

// CollectorProvider stores or prints the metrics produced by a workload run.
// cfg is the scenario's collector.config with templates already resolved.
type CollectorProvider interface {
	Collect(ctx context.Context, cfg map[string]any, metrics Metrics) error
}

// CollectorPreflight is an optional interface a CollectorProvider may implement
// to validate its config before the benchmark lifecycle starts. The runner calls
// it immediately after building the runner, before any provisioning. cfg is
// partially resolved (inputs + env only; target outputs are not yet available).
type CollectorPreflight interface {
	Preflight(cfg map[string]any) error
}

// CollectorMetadata is an optional interface a CollectorProvider may implement
// to contribute key-value pairs to the run's persistent metadata after collection.
// Useful when the collector resolves configuration (e.g. endpoint URL) that
// is not visible in the scenario YAML and therefore not in cfg.
type CollectorMetadata interface {
	RunMetadata(cfg map[string]any) map[string]string
}

// TargetMetadata is an optional interface a TargetProvider may implement to
// contribute descriptive key-value pairs (e.g. project id) that should be
// surfaced as collector labels, derived from its own Outputs. Mirrors
// CollectorMetadata's role for CollectorProvider.
type TargetMetadata interface {
	RunMetadata(outputs Outputs) map[string]string
}

// Connector is an optional interface that TargetProvider and DriverProvider
// implementations may satisfy to open an interactive shell on a provisioned
// instance. Providers that do not support interactive access (e.g. local,
// docker-compose) do not need to implement this interface.
type Connector interface {
	// ConnectArgs returns the argv for an interactive shell session on the
	// provisioned instance. The caller may exec the returned command directly
	// (replacing the current process) or print it for copy-paste into other tools.
	ConnectArgs(outputs Outputs) ([]string, error)
}

// BenchctlHost is an optional interface a DriverProvider may implement to
// report where it installed the benchctl binary on the machine it provisions.
// A store that is not shared across machines uses it to read a run's state
// back from the driver instance that is executing it (see runstate.Dialer);
// providers that run the workload in this process (e.g. local) do not
// implement it.
type BenchctlHost interface {
	RemoteBenchctlPath() string
}

// BenchmarkLifecycleProvider is an optional interface a TargetProvider may
// implement to manage per-benchmark service resources. The runner calls
// StartBenchmark immediately before a benchmark entry's steps and StopBenchmark
// after collection completes, so providers that host multiple services (e.g.
// docker-compose with several databases) can start and stop each one in turn
// rather than keeping all of them running simultaneously.
//
// If the runner detects this interface, it also registers a deferred cleanup
// that calls StopBenchmark for the currently active service on failure.
type BenchmarkLifecycleProvider interface {
	// StartBenchmark starts the named service and returns its outputs
	// (e.g. service.<name>.host, service.<name>.port) merged into the caller's
	// raw target outputs.
	StartBenchmark(ctx context.Context, cfg map[string]any, service string) (Outputs, error)
	// StopBenchmark stops the named service and cleans up its resources.
	StopBenchmark(ctx context.Context, cfg map[string]any, service string) error
}

// ArtifactFetcher is an optional interface a DriverProvider may implement to
// support `benchctl fetch`: copying whatever artifacts (logs, result files) a
// run wrote on the provisioned instance down to the local machine, e.g.
// before teardown. Providers that never write artifacts anywhere but
// stdout/a remote metrics backend (e.g. local) do not need to implement this.
type ArtifactFetcher interface {
	// FetchArtifacts copies known artifact locations from the instance
	// identified by outputs into localDest (created if needed). It must not
	// fail just because some or all expected files don't exist yet, e.g.
	// when fetch runs before the run reached the phase that writes them.
	FetchArtifacts(ctx context.Context, outputs Outputs, localDest string) error
}

// BootstrapRequest is everything Bootstrap needs to hand a run off to a
// driver instance.
type BootstrapRequest struct {
	Config        map[string]any
	DriverOutputs Outputs
	RunID         string
	ScenarioPath  string
	// StateSeed, when non-empty, is the run's state as JSON. A store that is
	// not shared across machines supplies it so the driver instance's own
	// `benchctl resume` can load the record from its own store; with a shared
	// store the driver reads the same record the orchestrator wrote, and this
	// is empty.
	StateSeed []byte
}

// AsyncBootstrapper is an optional interface implemented by DriverProviders that
// support async handoff. Bootstrap delivers the benchctl binary and the scenario
// YAML to the remote driver instance, then starts `benchctl resume <runID>` as a
// detached background process. The orchestrator exits immediately after Bootstrap
// returns; the driver process writes run state autonomously until completion.
type AsyncBootstrapper interface {
	Bootstrap(ctx context.Context, req BootstrapRequest) error
}
