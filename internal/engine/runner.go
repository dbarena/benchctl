// Package engine drives a benchmark run from end to end. It owns the run
// lifecycle (provision, driver setup, workload execution, per-benchmark
// collection, teardown), sequences those phases and records their status in
// the run state store, and defines the provider, adapter, and collector
// interfaces (see interfaces.go) that concrete implementations satisfy.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dbarena/benchctl/internal/buildinfo"
	"github.com/dbarena/benchctl/internal/consolelog"
	"github.com/dbarena/benchctl/internal/runstate"
	"github.com/dbarena/benchctl/internal/schema"
	"github.com/dbarena/benchctl/internal/tofustate"
	"github.com/fatih/color"
)

// selfTeardownTimeout bounds how long Run/RunAsync wait for their own
// automatic teardown-on-failure to finish. The budget needs real headroom:
// a full cloud target stack (the ec2 provider's instance, VPC, subnet, IGW,
// route tables, security group, placement group, and key pair) routinely
// takes 20-30s just for the first couple of dependent resources, and too
// tight a budget kills tofu destroy mid-flight (context cancellation sends
// SIGKILL) and leaks whatever it had not reached yet.
const selfTeardownTimeout = 30 * time.Minute

// Runner drives a scenario through the full benchmark lifecycle:
//
//	provision → driver.setup → workload.execute → driver.collect → teardown
//
// Teardown always runs for every successfully provisioned resource, in
// reverse provisioning order, even when an earlier phase fails.
type Runner struct {
	Target       TargetProvider
	Driver       DriverProvider
	Workloads    map[string]WorkloadAdapter
	Collector    CollectorProvider
	Store        runstate.Store // nil disables run state persistence
	ScenarioPath string         // stored in run state so teardown can reload the scenario
	Out          io.Writer      // phase-level status messages; nil disables output
	// CreatedBy is stored in run state for RLS matching.
	// Human users: their Supabase auth UUID. CI: "ci/nightly" or "github:{actor}".
	CreatedBy string
	// CreatedByEmail is the human-readable email for CreatedBy, display-only.
	CreatedByEmail string
	// NoTeardown skips the teardown phase unconditionally, leaving infrastructure running.
	// Use for post-failure inspection; run benchctl teardown <run-id> when done.
	// benchctl status shows environment: running until teardown is called.
	NoTeardown bool
	// LingerOnFailure skips teardown only when the run exits with an error.
	// On success teardown proceeds normally. Useful for inspecting failed
	// infrastructure without disabling cleanup on healthy runs.
	LingerOnFailure bool
	// RunID overrides the generated run ID when set (via `benchctl run
	// --run-id`, sync or async). A caller can then durably record "run X is
	// about to be launched" before invoking benchctl, and so survive a crash
	// or reboot during the long local provision/driver.setup window RunAsync
	// has before handoff. Run honors it too, for one consistent code path.
	RunID string
	// stateMu is a pointer rather than a value sync.Mutex because cmd/benchctl
	// constructs and returns Runner by value, which with a value mutex would
	// trip go vet's copylocks check. It serializes Store.Update calls for this
	// run: without it the heartbeat goroutine (see startHeartbeat) and the
	// main Run/Resume goroutine race, both doing a read-modify-write against
	// the same store record, and whichever write lands second silently
	// reverts the other's change.
	stateMu *sync.Mutex
	// stepWindows accumulates the interval of every measured suite step and is
	// serialized to StepWindowsFilename as the suite advances. Written only
	// from the goroutine running the suite.
	stepWindows []StepWindow
	// stepWindowsUnwritable records that writing StepWindowsFilename already
	// failed, so the warning is printed once instead of once per step.
	stepWindowsUnwritable bool
}

var phaseStyle = color.New(color.FgHiCyan, color.Bold)

// initStateMu lazily creates stateMu. Must be called before any goroutine
// that writes run state (e.g. the heartbeat goroutine) is started; Run and
// Resume both call it while still single-goroutine.
func (r *Runner) initStateMu() {
	if r.stateMu == nil {
		r.stateMu = &sync.Mutex{}
	}
}

// logf writes a formatted phase message to r.Out. It is a no-op when Out is nil.
func (r *Runner) logf(format string, args ...any) {
	if r.Out != nil {
		fmt.Fprintln(r.Out, phaseStyle.Sprintf("==> "+consolelog.Timestamp()+" "+format, args...))
	}
}

// setState is a best-effort Store.Update; errors are logged but do not fail
// the run. Calls are serialized by stateMu so concurrent writers (the main
// goroutine and the heartbeat goroutine) can't lose each other's updates.
func (r *Runner) setState(runID string, fn func(*runstate.State)) {
	if r.Store == nil {
		return
	}
	if r.stateMu != nil {
		r.stateMu.Lock()
		defer r.stateMu.Unlock()
	}
	if err := r.Store.Update(runID, fn); err != nil && r.Out != nil {
		fmt.Fprintf(r.Out, "warning: run state update failed: %v\n", err)
	}
}

// Run executes s with the given resolved inputs.
// If the run fails, any teardown errors are joined to the returned error.
// If the run succeeds but teardown fails, the teardown error is returned.
func (r *Runner) Run(ctx context.Context, s *schema.Scenario, inputs schema.ResolvedInputs) (runErr error) {
	r.initStateMu()
	if _, ok := r.Driver.(AsyncBootstrapper); ok {
		return fmt.Errorf("the %s driver requires --async mode; re-run with: benchctl run --async %s", s.Driver.Provider, r.ScenarioPath)
	}

	// --- initialize run state ---
	runID := r.RunID
	if runID == "" {
		runID = runstate.NewRunID(s.Metadata.Name)
	} else if err := runstate.ValidateRunID(runID); err != nil {
		return fmt.Errorf("invalid --run-id: %w", err)
	}
	if r.Store != nil {
		st := runstate.NewState(runID, s.Metadata.Name, r.ScenarioPath, inputs)
		st.TargetProvider = s.Target.Provider
		st.DriverProvider = s.Driver.Provider
		st.CreatedBy = r.CreatedBy
		st.CreatedByEmail = r.CreatedByEmail
		if err := r.Store.Create(st); err != nil {
			if r.Out != nil {
				fmt.Fprintf(r.Out, "warning: could not create run state: %v\n", err)
			}
		} else {
			r.logf("Run ID: %s", runID)
		}
	}

	var teardowns []func(context.Context) error
	defer func() {
		skipTeardown := r.NoTeardown || (r.LingerOnFailure && runErr != nil)
		if !skipTeardown {
			r.setState(runID, func(st *runstate.State) {
				st.Phases[runstate.PhaseTeardown] = runstate.StatusRunning
			})
			// Use a fresh context for teardown: the run context may already be
			// cancelled (e.g. Ctrl+C), and we must stop infrastructure regardless.
			tdCtx, tdCancel := context.WithTimeout(context.Background(), selfTeardownTimeout)
			defer tdCancel()
			var tdErr error
			// Stop at the first failure as a later-provisioned resource
			// (e.g. a co-located driver instance inside the target's VPC)
			// can still be attached to an earlier-provisioned one, so
			// tearing down the earlier one risks the same dependency-violation
			// failure mode benchctl teardown guards against.
			for i := len(teardowns) - 1; i >= 0; i-- {
				if err := teardowns[i](tdCtx); err != nil {
					tdErr = errors.Join(tdErr, fmt.Errorf("teardown: %w", err))
					break
				}
			}
			runErr = errors.Join(runErr, tdErr)
			now := time.Now().UTC()
			r.setState(runID, func(st *runstate.State) {
				st.CompletedAt = &now
				// Phases[teardown] reflects only whether teardown itself
				// succeeded (tdErr), not whether the run as a whole failed
				// (runErr). Otherwise a run that fails earlier (e.g.
				// provisioning) but still tears down cleanly gets recorded as
				// teardown having failed, which makes `benchctl teardown`'s
				// "already completed" guard never trigger and a later
				// re-run retries a delete against infra that's already gone.
				if tdErr == nil {
					st.TerminatedAt = &now
					st.Phases[runstate.PhaseTeardown] = runstate.StatusCompleted
				} else {
					st.Phases[runstate.PhaseTeardown] = runstate.StatusFailed
				}
				if runErr != nil {
					st.Error = runErr.Error()
				}
			})
		} else {
			// Teardown skipped: record completion without terminating
			// infrastructure. PhaseTeardown stays pending and TerminatedAt stays
			// nil, so benchctl status shows environment: running until teardown
			// is called.
			if r.LingerOnFailure && runErr != nil {
				r.logf("Run failed; infrastructure left running for inspection. Tear down with: benchctl teardown %s", runID)
			}
			now := time.Now().UTC()
			r.setState(runID, func(st *runstate.State) {
				st.CompletedAt = &now
				if runErr != nil {
					st.Error = runErr.Error()
				}
			})
		}
	}()

	tc := &TemplateContext{Inputs: inputs}
	r.writeInitialMetadata(runID, s, tc)
	toolVersions := collectToolVersionLabels(ctx, r.Workloads)

	if pf, ok := r.Collector.(CollectorPreflight); ok {
		collectorCfg, err := resolveConfigMap(s.Collector.Config, tc)
		if err != nil {
			return fmt.Errorf("resolve collector config: %w", err)
		}
		if err := pf.Preflight(collectorCfg); err != nil {
			return err
		}
	}

	// --- provision ---
	r.setState(runID, func(st *runstate.State) {
		st.Phases[runstate.PhaseProvision] = runstate.StatusRunning
	})

	r.logf("Provisioning target [%s]", s.Target.Provider)
	targetCfg, err := resolveConfigMap(s.Target.Config, tc)
	if err != nil {
		return fmt.Errorf("resolve target config: %w", err)
	}
	rawTargetOutputs, err := r.Target.Provision(ctx, runID, targetCfg)
	if err != nil {
		if len(rawTargetOutputs) > 0 {
			// Provision failed partway through (e.g. disk resize after project
			// create) but returned partial outputs identifying real
			// infrastructure whose own rollback failed. Register it for
			// teardown like a normal successful provision.
			teardowns = append(teardowns, func(ctx context.Context) error {
				r.logf("Tearing down target [%s]", s.Target.Provider)
				return r.Target.Teardown(ctx, rawTargetOutputs)
			})
			r.setState(runID, func(st *runstate.State) {
				st.TargetOutputs = map[string]string(rawTargetOutputs)
			})
		}
		return fmt.Errorf("provision target: %w", err)
	}
	tc.ProviderOutputs = rawTargetOutputs
	targetOutputs := rawTargetOutputs
	if len(s.Target.Outputs) > 0 {
		mapped, err := ResolveMap(s.Target.Outputs, tc)
		if err != nil {
			return fmt.Errorf("resolve target outputs: %w", err)
		}
		for k, v := range rawTargetOutputs {
			if strings.HasPrefix(k, "_") {
				mapped[k] = v
			}
		}
		targetOutputs = Outputs(mapped)
	}
	teardowns = append(teardowns, func(ctx context.Context) error {
		r.logf("Tearing down target [%s]", s.Target.Provider)
		return r.Target.Teardown(ctx, targetOutputs)
	})
	tc.TargetOutputs = targetOutputs

	// Persist target outputs now so that benchctl teardown works if the run
	// is left lingering (--no-teardown / --linger-on-failure) after a deploy failure.
	r.setState(runID, func(st *runstate.State) {
		st.TargetOutputs = map[string]string(targetOutputs)
	})

	if rawTargetOutputs["public_ip"] != "" {
		if err := r.deployRemoteServices(ctx, targetCfg, rawTargetOutputs); err != nil {
			return fmt.Errorf("deploy services: %w", err)
		}
	}

	r.logf("Provisioning driver [%s]", s.Driver.Provider)
	driverCfg, err := resolveConfigMap(s.Driver.Config, tc)
	if err != nil {
		return fmt.Errorf("resolve driver config: %w", err)
	}
	driverOutputs, err := r.Driver.Provision(ctx, runID, driverCfg)
	if err != nil {
		return fmt.Errorf("provision driver: %w", err)
	}
	teardowns = append(teardowns, func(ctx context.Context) error {
		r.logf("Tearing down driver [%s]", s.Driver.Provider)
		return r.Driver.Teardown(ctx, driverOutputs)
	})

	r.setState(runID, func(st *runstate.State) {
		st.Phases[runstate.PhaseProvision] = runstate.StatusCompleted
		st.TargetOutputs = map[string]string(targetOutputs)
		st.DriverOutputs = map[string]string(driverOutputs)
	})
	r.packTofuState(runID, targetOutputs, driverOutputs)

	// --- driver.setup ---
	r.setState(runID, func(st *runstate.State) {
		st.Phases[runstate.PhaseDriverSetup] = runstate.StatusRunning
	})
	r.logf("Setting up driver [%s]", s.Driver.Provider)
	if err := r.Driver.Setup(ctx, driverCfg, targetOutputs); err != nil {
		return fmt.Errorf("driver setup: %w", err)
	}
	r.setState(runID, func(st *runstate.State) {
		st.Phases[runstate.PhaseDriverSetup] = runstate.StatusCompleted
	})

	// --- suite execution + per-benchmark collection ---
	var infoMap map[string]string
	var workloadMetrics Metrics
	collectorProvider, _ := Resolve(s.Collector.Provider, tc)

	// Per-benchmark service lifecycle: either via BenchmarkLifecycleProvider
	// (e.g. local docker-compose) or via SSH for remote targets with public_ip.
	blp, _ := r.Target.(BenchmarkLifecycleProvider)
	remoteTarget := rawTargetOutputs["public_ip"] != ""
	var activeService string
	if blp != nil {
		teardowns = append(teardowns, func(ctx context.Context) error {
			if activeService != "" {
				return blp.StopBenchmark(ctx, targetCfg, activeService)
			}
			return nil
		})
	} else if remoteTarget {
		teardowns = append(teardowns, func(ctx context.Context) error {
			if activeService != "" {
				return r.stopRemoteService(ctx, targetCfg, rawTargetOutputs, activeService)
			}
			return nil
		})
	}

	fixtureCombos, err := schema.ExpandFixtures(s.Suite.Fixtures, inputs)
	if err != nil {
		return fmt.Errorf("expand fixtures: %w", err)
	}

	iterations, err := suiteIterations(s.Suite, tc)
	if err != nil {
		return err
	}
	for iter := 0; iter < iterations; iter++ {
		for fi, combo := range fixtureCombos {
			tc.Fixture = combo
			if len(s.Suite.Fixtures) > 0 {
				r.logf("Fixture [%s]", formatFixture(combo))
			}
			for i, entry := range s.Suite.Benchmarks {
				isLastOverall := iter == iterations-1 && fi == len(fixtureCombos)-1 && i == len(s.Suite.Benchmarks)-1
				infoMap = nil
				if entry.Using != "" {
					r.logf("Starting service [%s]", entry.Using)
					var started Outputs
					var startErr error
					if blp != nil {
						started, startErr = blp.StartBenchmark(ctx, targetCfg, entry.Using)
					} else if remoteTarget {
						started, startErr = r.startRemoteService(ctx, targetCfg, rawTargetOutputs, entry.Using)
					}
					if startErr != nil {
						return fmt.Errorf("start benchmark %q: %w", entry.Name, startErr)
					}
					activeService = entry.Using
					for k, v := range started {
						rawTargetOutputs[k] = v
					}
					if digest := rawTargetOutputs["service."+entry.Using+".image_digest"]; digest != "" {
						if infoMap == nil {
							infoMap = make(map[string]string)
						}
						infoMap["resolved_image"] = digest
					}
				}

				var stepOutputs Outputs
				if entry.Using != "" {
					stepOutputs = buildServiceOutputs(rawTargetOutputs, entry.Using)
				} else {
					stepOutputs = targetOutputs
				}
				lastWorkloadIdx := lastWorkloadStep(entry.Steps, r.Workloads)
				env := &suiteStepEnv{
					runID:           runID,
					tc:              tc,
					stepOutputs:     stepOutputs,
					benchmark:       entry.Name,
					combo:           combo,
					iteration:       iter,
					infoMap:         &infoMap,
					workloadMetrics: &workloadMetrics,
					workloadPending: func() bool { return true },
					failWorkload:    func(err error) error { return err },
				}
				for si, step := range entry.Steps {
					if err := r.runSuiteStep(ctx, env, step, isLastOverall && si == lastWorkloadIdx); err != nil {
						return err
					}
				}

				// --- collect for this benchmark ---
				mergeInfoIntoMetrics(infoMap, &workloadMetrics)
				r.setState(runID, func(st *runstate.State) {
					st.Phases[runstate.PhaseDriverCollect] = runstate.StatusRunning
				})
				r.logf("Collecting results [%s]", s.Driver.Provider)
				finalMetrics, err := r.Driver.Collect(ctx, driverCfg, driverOutputs, workloadMetrics)
				if err != nil {
					return fmt.Errorf("driver collect: %w", err)
				}
				r.logf("Storing results [%s]", collectorProvider)
				collectorCfg, err := resolveConfigMap(s.Collector.Config, tc)
				if err != nil {
					return fmt.Errorf("resolve collector config: %w", err)
				}
				if collectorCfg == nil {
					collectorCfg = make(map[string]any)
				}
				collectorCfg["run_id"] = runID
				collectorCfg["scenario"] = s.Metadata.Name
				var targetMeta map[string]string
				if tm, ok := r.Target.(TargetMetadata); ok {
					targetMeta = tm.RunMetadata(targetOutputs)
				}
				autoMeta := maps.Clone(targetMeta)
				if autoMeta == nil {
					autoMeta = make(map[string]string, len(toolVersions))
				}
				maps.Copy(autoMeta, toolVersions)
				r.injectAutoLabels(collectorCfg, entry, iter+1, combo, autoMeta, s.Metadata.Labels)
				if err := r.Collector.Collect(ctx, collectorCfg, finalMetrics); err != nil {
					return fmt.Errorf("collect: %w", err)
				}
				var collectorMeta map[string]string
				if cm, ok := r.Collector.(CollectorMetadata); ok {
					collectorMeta = cm.RunMetadata(collectorCfg)
				}
				metadata := buildRunMetadata(s.Metadata.Labels, infoMap, collectorCfg, collectorMeta)
				r.setState(runID, func(st *runstate.State) {
					if isLastOverall {
						st.Phases[runstate.PhaseDriverCollect] = runstate.StatusCompleted
					} else {
						st.Phases[runstate.PhaseDriverCollect] = runstate.StatusRunning
					}
					if len(metadata) > 0 {
						if st.Metadata == nil {
							st.Metadata = make(map[string]string)
						}
						maps.Copy(st.Metadata, metadata)
					}
				})
				workloadMetrics = nil

				if entry.Using != "" {
					r.logf("Stopping service [%s]", entry.Using)
					var stopErr error
					if blp != nil {
						stopErr = blp.StopBenchmark(ctx, targetCfg, entry.Using)
					} else if remoteTarget {
						stopErr = r.stopRemoteService(ctx, targetCfg, rawTargetOutputs, entry.Using)
					}
					if stopErr != nil {
						return fmt.Errorf("stop benchmark %q: %w", entry.Name, stopErr)
					}
					activeService = ""
				}

				if !isLastOverall {
					if err := r.runBetweenBenchmarksSteps(ctx, s.Suite.BetweenBenchmarks, rawTargetOutputs); err != nil {
						return fmt.Errorf("between-benchmarks: %w", err)
					}
				}
			}
		}
	}

	return nil
}

// RunAsync provisions the target and driver, writes run state with provider
// outputs, then calls driver.Bootstrap to hand off to the remote driver process.
// It returns the run ID so the caller can print it. Teardown is not
// registered; run it separately via `benchctl teardown <run-id>`.
func (r *Runner) RunAsync(ctx context.Context, s *schema.Scenario, inputs schema.ResolvedInputs) (runID string, runErr error) {
	bootstrapper, ok := r.Driver.(AsyncBootstrapper)
	if !ok {
		return "", fmt.Errorf("driver %q does not implement async bootstrap", s.Driver.Provider)
	}
	runID = r.RunID
	if runID == "" {
		runID = runstate.NewRunID(s.Metadata.Name)
	} else if err := runstate.ValidateRunID(runID); err != nil {
		return "", fmt.Errorf("invalid --run-id: %w", err)
	}
	if r.Store != nil {
		st := runstate.NewState(runID, s.Metadata.Name, r.ScenarioPath, inputs)
		st.TargetProvider = s.Target.Provider
		st.DriverProvider = s.Driver.Provider
		st.CreatedBy = r.CreatedBy
		st.CreatedByEmail = r.CreatedByEmail
		if err := r.Store.Create(st); err != nil {
			return "", fmt.Errorf("create run state: %w", err)
		}
		r.logf("Run ID: %s", runID)
	}

	var teardowns []func(context.Context) error

	// Finalize state on any error return below this point. Every early
	// failure funnels through here instead of each call site hand-rolling
	// its own teardown-then-record-state logic: it tears down whatever is
	// registered in `teardowns` so far (skipping if --no-teardown or
	// --linger-on-failure is set, exactly like Run()'s defer), marks
	// whichever phase was in flight as failed, and sets Error/CompletedAt so
	// the run becomes terminal instead of getting stuck showing
	// "running"/"pending" forever.
	// On success the remote resume process (after Bootstrap) owns
	// completion and teardown from here; see Resume()'s docstring.
	defer func() {
		if runErr == nil {
			return
		}
		skipTeardown := r.NoTeardown || r.LingerOnFailure
		var tdErr error
		ranTeardown := false
		if !skipTeardown && len(teardowns) > 0 {
			ranTeardown = true
			tdCtx, tdCancel := context.WithTimeout(context.Background(), selfTeardownTimeout)
			defer tdCancel()
			// Stop at the first failure: a later-provisioned resource (e.g.
			// the driver) can depend on an earlier one, so tearing down the
			// earlier one after a later teardown failed risks the same
			// dependency-violation failure mode benchctl teardown guards
			// against; see Run()'s defer for the same rationale.
			for i := len(teardowns) - 1; i >= 0; i-- {
				if err := teardowns[i](tdCtx); err != nil {
					tdErr = errors.Join(tdErr, fmt.Errorf("teardown: %w", err))
					break
				}
			}
			runErr = errors.Join(runErr, tdErr)
		} else if r.LingerOnFailure && len(teardowns) > 0 {
			r.logf("Run failed; infrastructure left running for inspection. Tear down with: benchctl teardown %s", runID)
		}
		now := time.Now().UTC()
		r.setState(runID, func(st *runstate.State) {
			for _, p := range runstate.AllPhases {
				if st.Phases[p] == runstate.StatusRunning {
					st.Phases[p] = runstate.StatusFailed
					break
				}
			}
			if ranTeardown {
				if tdErr == nil {
					st.TerminatedAt = &now
					st.Phases[runstate.PhaseTeardown] = runstate.StatusCompleted
				} else {
					st.Phases[runstate.PhaseTeardown] = runstate.StatusFailed
					r.logf("Partial infrastructure may remain. Tear down with: benchctl teardown %s", runID)
				}
			}
			st.Error = runErr.Error()
			st.CompletedAt = &now
		})
	}()

	tc := &TemplateContext{Inputs: inputs}
	r.writeInitialMetadata(runID, s, tc)

	// --- provision target ---
	r.setState(runID, func(st *runstate.State) {
		st.Phases[runstate.PhaseProvision] = runstate.StatusRunning
	})
	r.logf("Provisioning target [%s]", s.Target.Provider)
	targetCfg, err := resolveConfigMap(s.Target.Config, tc)
	if err != nil {
		return runID, fmt.Errorf("resolve target config: %w", err)
	}
	rawTargetOutputs, err := r.Target.Provision(ctx, runID, targetCfg)
	if err != nil {
		if len(rawTargetOutputs) > 0 {
			// Provision failed partway through (e.g. disk resize after project
			// create) but returned partial outputs identifying real
			// infrastructure whose own rollback failed. Register it for
			// teardown like a normal successful provision, and persist it so
			// `benchctl teardown` can finish the job if that also fails.
			teardowns = append(teardowns, func(ctx context.Context) error {
				r.logf("Tearing down target [%s]", s.Target.Provider)
				return r.Target.Teardown(ctx, rawTargetOutputs)
			})
			r.setState(runID, func(st *runstate.State) {
				st.TargetOutputs = map[string]string(rawTargetOutputs)
			})
			r.packTofuState(runID, rawTargetOutputs, nil)
		}
		return runID, fmt.Errorf("provision target: %w", err)
	}
	tc.ProviderOutputs = rawTargetOutputs
	targetOutputs := rawTargetOutputs
	if len(s.Target.Outputs) > 0 {
		mapped, err := ResolveMap(s.Target.Outputs, tc)
		if err != nil {
			return runID, fmt.Errorf("resolve target outputs: %w", err)
		}
		for k, v := range rawTargetOutputs {
			if strings.HasPrefix(k, "_") {
				mapped[k] = v
			}
		}
		targetOutputs = Outputs(mapped)
	}
	tc.TargetOutputs = targetOutputs
	teardowns = append(teardowns, func(ctx context.Context) error {
		r.logf("Tearing down target [%s]", s.Target.Provider)
		return r.Target.Teardown(ctx, targetOutputs)
	})

	// Persist target outputs now so teardown can locate the tofu work dir even if
	// deployRemoteServices or driver provisioning fails and we return early.
	r.setState(runID, func(st *runstate.State) {
		st.TargetOutputs = map[string]string(targetOutputs)
	})
	r.packTofuState(runID, targetOutputs, nil)

	if rawTargetOutputs["public_ip"] != "" {
		if err := r.deployRemoteServices(ctx, targetCfg, rawTargetOutputs); err != nil {
			return runID, fmt.Errorf("deploy services: %w", err)
		}
	}

	// --- provision driver ---
	r.logf("Provisioning driver [%s]", s.Driver.Provider)
	driverCfg, err := resolveConfigMap(s.Driver.Config, tc)
	if err != nil {
		return runID, fmt.Errorf("resolve driver config: %w", err)
	}
	driverOutputs, err := r.Driver.Provision(ctx, runID, driverCfg)
	if err != nil {
		if len(driverOutputs) > 0 {
			// The provider's automatic cleanup failed and returned a partial work dir.
			// Register it for teardown, and persist it so `benchctl teardown` can
			// finish the job if that also fails.
			teardowns = append(teardowns, func(ctx context.Context) error {
				r.logf("Tearing down driver [%s]", s.Driver.Provider)
				return r.Driver.Teardown(ctx, driverOutputs)
			})
			r.setState(runID, func(st *runstate.State) {
				st.TargetOutputs = map[string]string(targetOutputs)
				st.DriverOutputs = map[string]string(driverOutputs)
			})
			r.packTofuState(runID, targetOutputs, driverOutputs)
		}
		return runID, fmt.Errorf("provision driver: %w", err)
	}

	// Persist outputs now so teardown can locate tofu work dirs even if the key
	// copy below fails and we return early with running infra.
	r.setState(runID, func(st *runstate.State) {
		st.Phases[runstate.PhaseProvision] = runstate.StatusCompleted
		st.TargetOutputs = map[string]string(targetOutputs)
		st.DriverOutputs = map[string]string(driverOutputs)
	})
	r.packTofuState(runID, targetOutputs, driverOutputs)
	teardowns = append(teardowns, func(ctx context.Context) error {
		r.logf("Tearing down driver [%s]", s.Driver.Provider)
		return r.Driver.Teardown(ctx, driverOutputs)
	})

	// The target SSH key path is an absolute local path produced by tofu. Copy it
	// to the driver instance so the remote benchctl process can reach the target,
	// then update state with the driver-side path.
	newKeyPath, copyErr := r.copyTargetKeyToDriver(ctx, rawTargetOutputs["ssh_private_key_path"], driverOutputs)
	if copyErr != nil {
		return runID, fmt.Errorf("copy target key to driver: %w", copyErr)
	}
	if newKeyPath != rawTargetOutputs["ssh_private_key_path"] {
		// Record the driver-side path under a separate key so that the original
		// ssh_private_key_path (local absolute path) is preserved for
		// `benchctl connect target` which runs on the local machine.
		rawTargetOutputs[outputKeyDriverTargetKeyPath] = newKeyPath
		targetOutputs[outputKeyDriverTargetKeyPath] = newKeyPath
		r.setState(runID, func(st *runstate.State) {
			st.TargetOutputs = map[string]string(targetOutputs)
		})
	}

	// --- bootstrap remote driver ---
	r.logf("Bootstrapping remote driver [%s]", s.Driver.Provider)
	req := BootstrapRequest{
		Config:        driverCfg,
		DriverOutputs: driverOutputs,
		RunID:         runID,
		ScenarioPath:  r.ScenarioPath,
	}
	// A store the driver instance cannot read has to be seeded with the record,
	// or `benchctl resume` there has nothing to load. Seeding last means the
	// seed carries every output written above, including the driver-side
	// target key path.
	seed, err := r.stateSeed(runID)
	if err != nil {
		return runID, err
	}
	req.StateSeed = seed
	if err := bootstrapper.Bootstrap(ctx, req); err != nil {
		return runID, fmt.Errorf("bootstrap driver: %w", err)
	}

	// Handoff succeeded, so the driver instance owns the run from here and later
	// reads must go there for the truth. Only now: had Bootstrap failed, the
	// deferred finalizer above would record the outcome in this store, and
	// that record must stay authoritative.
	if owner, ok := r.Store.(runstate.Owner); ok && !r.storeIsShared() {
		if err := owner.Delegate(runID); err != nil {
			r.logf("warning: mark run as running on the driver instance: %v", err)
		}
	}

	return runID, nil
}

// stateSeed returns the run record as JSON when the store cannot be read from
// the driver instance, and nil when it can.
func (r *Runner) stateSeed(runID string) ([]byte, error) {
	if r.Store == nil || r.storeIsShared() {
		return nil, nil
	}
	state, err := r.Store.Load(runID)
	if err != nil {
		return nil, fmt.Errorf("read run state for handoff: %w", err)
	}
	seed, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("serialize run state for handoff: %w", err)
	}
	return seed, nil
}

// Resume picks up a run from its first pending post-provision phase and executes
// driver.setup → workload.execute → driver.collect.
// It never runs teardown; that is always orchestrator-side.
//
// Inputs are taken from state.Inputs (already resolved at run creation time);
// the scenario's InputDef types are used to restore the correct Go types after
// JSON round-tripping.
func (r *Runner) Resume(ctx context.Context, s *schema.Scenario, state *runstate.State) error {
	r.initStateMu()
	runID := state.RunID
	inputs := rehydrateInputs(state.Inputs, s.Inputs)
	targetOutputs := Outputs(state.TargetOutputs)
	tc := &TemplateContext{Inputs: inputs, TargetOutputs: targetOutputs}
	tc.ProviderOutputs = targetOutputs
	toolVersions := collectToolVersionLabels(ctx, r.Workloads)

	if pf, ok := r.Collector.(CollectorPreflight); ok {
		collectorCfg, err := resolveConfigMap(s.Collector.Config, tc)
		if err != nil {
			return fmt.Errorf("resolve collector config: %w", err)
		}
		if err := pf.Preflight(collectorCfg); err != nil {
			return err
		}
	}

	targetCfgResume, err := resolveConfigMap(s.Target.Config, tc)
	if err != nil {
		return fmt.Errorf("resolve target config: %w", err)
	}
	driverCfg, err := resolveConfigMap(s.Driver.Config, tc)
	if err != nil {
		return fmt.Errorf("resolve driver config: %w", err)
	}

	pending := func(p runstate.Phase) bool {
		return state.Phases[p] != runstate.StatusCompleted
	}
	fail := func(p runstate.Phase, err error) error {
		now := time.Now().UTC()
		r.setState(runID, func(st *runstate.State) {
			st.Phases[p] = runstate.StatusFailed
			st.Error = err.Error()
			st.CompletedAt = &now
		})
		return err
	}

	// --- driver.setup ---
	if pending(runstate.PhaseDriverSetup) {
		r.setState(runID, func(st *runstate.State) {
			st.Phases[runstate.PhaseDriverSetup] = runstate.StatusRunning
		})
		r.logf("Setting up driver [%s]", s.Driver.Provider)
		if err := r.Driver.Setup(ctx, driverCfg, targetOutputs); err != nil {
			return fail(runstate.PhaseDriverSetup, fmt.Errorf("driver setup: %w", err))
		}
		r.setState(runID, func(st *runstate.State) {
			st.Phases[runstate.PhaseDriverSetup] = runstate.StatusCompleted
		})
	}

	// --- suite execution + per-benchmark collection ---
	var infoMap map[string]string
	var workloadMetrics Metrics
	driverOutputs := Outputs(state.DriverOutputs)
	collectorProvider, _ := Resolve(s.Collector.Provider, tc)
	resumeBlp, _ := r.Target.(BenchmarkLifecycleProvider)
	resumeRemote := targetOutputs["public_ip"] != ""

	fixtureCombos, err := schema.ExpandFixtures(s.Suite.Fixtures, inputs)
	if err != nil {
		return fmt.Errorf("expand fixtures: %w", err)
	}

	iterations, err := suiteIterations(s.Suite, tc)
	if err != nil {
		return err
	}
	for iter := 0; iter < iterations; iter++ {
		for fi, combo := range fixtureCombos {
			tc.Fixture = combo
			if len(s.Suite.Fixtures) > 0 {
				r.logf("Fixture [%s]", formatFixture(combo))
			}
			for i, entry := range s.Suite.Benchmarks {
				isLastOverall := iter == iterations-1 && fi == len(fixtureCombos)-1 && i == len(s.Suite.Benchmarks)-1
				infoMap = nil
				if entry.Using != "" {
					r.logf("Starting service [%s]", entry.Using)
					var started Outputs
					var startErr error
					if resumeBlp != nil {
						started, startErr = resumeBlp.StartBenchmark(ctx, targetCfgResume, entry.Using)
					} else if resumeRemote {
						started, startErr = r.startRemoteService(ctx, targetCfgResume, targetOutputs, entry.Using)
					}
					if startErr != nil {
						return fmt.Errorf("start benchmark %q: %w", entry.Name, startErr)
					}
					for k, v := range started {
						targetOutputs[k] = v
					}
					if digest := targetOutputs["service."+entry.Using+".image_digest"]; digest != "" {
						if infoMap == nil {
							infoMap = make(map[string]string)
						}
						infoMap["resolved_image"] = digest
					}
				}

				var stepOutputs Outputs
				if entry.Using != "" {
					stepOutputs = buildServiceOutputs(targetOutputs, entry.Using)
				} else {
					stepOutputs = targetOutputs
				}
				lastWorkloadIdx := lastWorkloadStep(entry.Steps, r.Workloads)
				env := &suiteStepEnv{
					runID:           runID,
					tc:              tc,
					stepOutputs:     stepOutputs,
					benchmark:       entry.Name,
					combo:           combo,
					iteration:       iter,
					infoMap:         &infoMap,
					workloadMetrics: &workloadMetrics,
					workloadPending: func() bool { return pending(runstate.PhaseWorkloadExecute) },
					failWorkload:    func(err error) error { return fail(runstate.PhaseWorkloadExecute, err) },
				}
				for si, step := range entry.Steps {
					if err := r.runSuiteStep(ctx, env, step, isLastOverall && si == lastWorkloadIdx); err != nil {
						return err
					}
				}

				// --- collect for this benchmark ---
				if pending(runstate.PhaseDriverCollect) {
					mergeInfoIntoMetrics(infoMap, &workloadMetrics)
					r.setState(runID, func(st *runstate.State) {
						st.Phases[runstate.PhaseDriverCollect] = runstate.StatusRunning
					})
					r.logf("Collecting results [%s]", s.Driver.Provider)
					finalMetrics, err := r.Driver.Collect(ctx, driverCfg, driverOutputs, workloadMetrics)
					if err != nil {
						return fail(runstate.PhaseDriverCollect, fmt.Errorf("driver collect: %w", err))
					}
					r.logf("Storing results [%s]", collectorProvider)
					collectorCfg, err := resolveConfigMap(s.Collector.Config, tc)
					if err != nil {
						return fail(runstate.PhaseDriverCollect, fmt.Errorf("resolve collector config: %w", err))
					}
					if collectorCfg == nil {
						collectorCfg = make(map[string]any)
					}
					collectorCfg["run_id"] = runID
					collectorCfg["scenario"] = s.Metadata.Name
					var targetMeta map[string]string
					if tm, ok := r.Target.(TargetMetadata); ok {
						targetMeta = tm.RunMetadata(targetOutputs)
					}
					autoMeta := maps.Clone(targetMeta)
					if autoMeta == nil {
						autoMeta = make(map[string]string, len(toolVersions))
					}
					maps.Copy(autoMeta, toolVersions)
					r.injectAutoLabels(collectorCfg, entry, iter+1, combo, autoMeta, s.Metadata.Labels)
					if err := r.Collector.Collect(ctx, collectorCfg, finalMetrics); err != nil {
						return fail(runstate.PhaseDriverCollect, fmt.Errorf("collect: %w", err))
					}
					var collectorMeta map[string]string
					if cm, ok := r.Collector.(CollectorMetadata); ok {
						collectorMeta = cm.RunMetadata(collectorCfg)
					}
					metadata := buildRunMetadata(s.Metadata.Labels, infoMap, collectorCfg, collectorMeta)
					r.setState(runID, func(st *runstate.State) {
						if isLastOverall {
							st.Phases[runstate.PhaseDriverCollect] = runstate.StatusCompleted
							now := time.Now().UTC()
							st.CompletedAt = &now
						} else {
							st.Phases[runstate.PhaseDriverCollect] = runstate.StatusRunning
						}
						if len(metadata) > 0 {
							st.Metadata = metadata
						}
					})
					workloadMetrics = nil
				}

				if entry.Using != "" {
					r.logf("Stopping service [%s]", entry.Using)
					var stopErr error
					if resumeBlp != nil {
						stopErr = resumeBlp.StopBenchmark(ctx, targetCfgResume, entry.Using)
					} else if resumeRemote {
						stopErr = r.stopRemoteService(ctx, targetCfgResume, targetOutputs, entry.Using)
					}
					if stopErr != nil {
						return fmt.Errorf("stop benchmark %q: %w", entry.Name, stopErr)
					}
				}

				if !isLastOverall {
					if err := r.runBetweenBenchmarksSteps(ctx, s.Suite.BetweenBenchmarks, targetOutputs); err != nil {
						return fmt.Errorf("between-benchmarks: %w", err)
					}
				}
			}
		}
	}

	// Defensive fallback: CompletedAt is normally set atomically with the
	// final driver.collect completion above. It is still nil here when the
	// state this Resume() call loaded already had driver.collect completed,
	// so the block that sets both never ran (see pending() above).
	r.setState(runID, func(st *runstate.State) {
		if st.CompletedAt == nil {
			now := time.Now().UTC()
			st.CompletedAt = &now
		}
	})
	return nil
}

// startHeartbeat launches a goroutine that writes LastHeartbeat to run state
// every 30 seconds. The returned stop function must be called when the step
// completes (or fails) to clean up the goroutine. It is a no-op when Store is nil.
func (r *Runner) startHeartbeat(runID string) func() {
	if r.Store == nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				now := time.Now().UTC()
				r.setState(runID, func(st *runstate.State) {
					st.LastHeartbeat = &now
				})
			case <-done:
				return
			}
		}
	}()
	return func() { close(done) }
}

// suiteStepEnv is the per-benchmark context one suite step runs in. Run and
// Resume each build one per benchmark entry and reuse it for every step in
// that entry, so step dispatch has a single implementation rather than two
// copies that drift apart.
type suiteStepEnv struct {
	runID       string
	tc          *TemplateContext
	stepOutputs Outputs
	benchmark   string
	combo       schema.ResolvedFixture
	iteration   int

	// infoMap accumulates `type: metadata` results and workloadMetrics
	// accumulates everything the collector will receive. Both belong to the
	// caller and outlive any single step, hence the pointers.
	infoMap         *map[string]string
	workloadMetrics *Metrics

	// workloadPending reports whether workload.execute still has work to do.
	// Resume returns false once an earlier attempt completed it; Run always
	// returns true.
	workloadPending func() bool
	// failWorkload converts an adapter error into the error the caller
	// returns. Resume records the phase as failed first; Run passes it
	// through unchanged.
	failWorkload func(error) error
}

// runSuiteStep executes one step of a benchmark entry and folds its result
// into env. isFinalWorkloadStep marks the last workload step of the whole
// suite: after it, workload.execute is completed rather than still running.
//
// A step that dispatchSuiteStep reports as measured also gets its interval
// recorded in StepWindowsFilename, including when it failed.
func (r *Runner) runSuiteStep(ctx context.Context, env *suiteStepEnv, step schema.SuiteStep, isFinalWorkloadStep bool) error {
	stepStart := time.Now().UTC()
	r.setState(env.runID, func(st *runstate.State) {
		st.CurrentFixture = env.combo
		st.CurrentIteration = env.iteration
		st.CurrentStep = step.Name
		st.StepStartedAt = &stepStart
	})
	resolvedArgs, err := ResolveMap(step.Args, env.tc)
	if err != nil {
		// No window: the step never got as far as doing anything.
		return fmt.Errorf("suite step %s: resolve args: %w", step.Name, err)
	}

	measured, stepErr := r.dispatchSuiteStep(ctx, env, step, resolvedArgs, isFinalWorkloadStep)
	if measured {
		r.recordStepWindow(StepWindow{
			Step:      step.Name,
			Type:      step.Type,
			Benchmark: env.benchmark,
			Iteration: env.iteration + 1,
			Fixture:   maps.Clone(env.combo),
			StartedAt: stepStart,
			EndedAt:   time.Now().UTC(),
			Failed:    stepErr != nil,
		})
	}
	return stepErr
}

// dispatchSuiteStep runs the step and reports whether it was a measurement
// interval worth recording a window for.
//
// Only steps that occupy real time qualify: a workload step that actually
// executed, and a `type: collect` step, which samples the target at a point
// the caller wants to correlate against provider-side data. `metadata` and
// `sql` steps are sub-second bookkeeping, and recording them would bury the
// handful of intervals that matter under dozens of near-zero ones.
func (r *Runner) dispatchSuiteStep(ctx context.Context, env *suiteStepEnv, step schema.SuiteStep, resolvedArgs map[string]string, isFinalWorkloadStep bool) (measured bool, err error) {
	switch step.Type {
	case "metadata":
		r.logf("Collecting metadata [%s]", step.Name)
		val, err := collectMetadata(ctx, r.Out, env.stepOutputs, step.Command, resolvedArgs["name"], resolvedArgs["query"])
		if err != nil {
			return false, fmt.Errorf("suite step %s: %w", step.Name, err)
		}
		if *env.infoMap == nil {
			*env.infoMap = make(map[string]string)
		}
		(*env.infoMap)[resolvedArgs["name"]] = val
		env.tc.Info = *env.infoMap
		return false, nil

	case "sql":
		r.logf("Running SQL [%s]", step.Name)
		if err := runSQLStatement(ctx, r.Out, env.stepOutputs, step.Name, resolvedArgs["query"]); err != nil {
			return false, fmt.Errorf("suite step %s: %w", step.Name, err)
		}
		return false, nil

	case "collect":
		r.logf("Collecting PG metrics [%s]", step.Name)
		pts, err := collectPGMetrics(ctx, r.Out, env.stepOutputs, step.Name, resolvedArgs["family"], resolvedArgs["query"])
		if err != nil {
			return true, fmt.Errorf("suite step %s: %w", step.Name, err)
		}
		if len(pts) > 0 {
			tagPointsWithStep(pts, step.Name)
			if *env.workloadMetrics == nil {
				*env.workloadMetrics = make(Metrics)
			}
			sm, _ := (*env.workloadMetrics)[StructuredKey].(StructuredMetrics)
			sm.Points = append(sm.Points, pts...)
			(*env.workloadMetrics)[StructuredKey] = sm
		}
		return true, nil

	default:
		adapter, ok := r.Workloads[step.Type]
		if !ok {
			return false, fmt.Errorf("suite step %s: unknown workload adapter %q", step.Name, step.Type)
		}
		if !env.workloadPending() {
			// Already completed on an earlier attempt of this run; its window
			// was recorded then, and recording a second near-zero one now
			// would misreport the benchmark as having taken no time.
			return false, nil
		}
		resolvedStep := schema.SuiteStep{Name: step.Name, Type: step.Type, Command: resolveCommandPath(step.Command, r.ScenarioPath), Args: resolvedArgs}
		r.setState(env.runID, func(st *runstate.State) {
			st.Phases[runstate.PhaseWorkloadExecute] = runstate.StatusRunning
		})
		r.logf("Running workload [%s]", step.Name)
		stopHB := r.startHeartbeat(env.runID)
		stepMetrics, execErr := adapter.Run(ctx, env.stepOutputs, resolvedStep)
		stopHB()
		if execErr != nil {
			return true, env.failWorkload(fmt.Errorf("execute: %w", execErr))
		}
		labelStepMetrics(stepMetrics, step.Name)
		mergeMetrics(env.workloadMetrics, stepMetrics)
		r.setState(env.runID, func(st *runstate.State) {
			if isFinalWorkloadStep {
				st.Phases[runstate.PhaseWorkloadExecute] = runstate.StatusCompleted
			} else {
				st.Phases[runstate.PhaseWorkloadExecute] = runstate.StatusRunning
			}
		})
		return true, nil
	}
}

// runBetweenBenchmarksSteps executes suite.between-benchmarks steps between
// consecutive benchmark entries. Steps with scope "driver" run locally; steps
// with scope "target" run on the target host via SSH.
func (r *Runner) runBetweenBenchmarksSteps(ctx context.Context, steps []schema.BetweenBenchmarksStep, targetOutputs Outputs) error {
	if len(steps) == 0 {
		return nil
	}
	for _, step := range steps {
		r.logf("Running between-benchmarks step [%s]", step.Name)
		var err error
		switch step.Scope {
		case "driver":
			err = r.runLocalShell(ctx, step.Args)
		case "target":
			ip := targetOutputs["public_ip"]
			switch step.Command {
			case "shell":
				if ip == "" {
					// No SSH-capable target, so run the command locally.
					err = r.runLocalShell(ctx, step.Args)
				} else {
					keyPath := targetKeyPath(targetOutputs)
					user := targetOutputs["ssh_user"]
					if user == "" {
						return fmt.Errorf("step %q: missing ssh_user in target outputs", step.Name)
					}
					err = RunSSH(ctx, r.Out, keyPath, user, ip, step.Args)
				}
			case "reboot":
				if ip == "" {
					return fmt.Errorf("step %q: command \"reboot\" requires an SSH-capable target (public_ip not available)", step.Name)
				}
				keyPath := targetKeyPath(targetOutputs)
				user := targetOutputs["ssh_user"]
				if user == "" {
					return fmt.Errorf("step %q: missing ssh_user in target outputs", step.Name)
				}
				err = RunSSHIgnoreDisconnect(ctx, r.Out, keyPath, user, ip, "sudo reboot")
				if err == nil {
					err = WaitForSSHDown(ctx, r.Out, ip, 30*time.Second)
				}
				if err == nil {
					err = WaitForSSH(ctx, r.Out, ip, 5*time.Minute)
				}
			case "trim":
				if ip == "" {
					return fmt.Errorf("step %q: command \"trim\" requires an SSH-capable target (public_ip not available)", step.Name)
				}
				keyPath := targetKeyPath(targetOutputs)
				user := targetOutputs["ssh_user"]
				if user == "" {
					return fmt.Errorf("step %q: missing ssh_user in target outputs", step.Name)
				}
				wait := true
				if step.Wait != nil {
					wait = *step.Wait
				}
				timeout := 30 * time.Minute
				if step.Timeout != "" {
					timeout, _ = time.ParseDuration(step.Timeout) // pre-validated in schema
				}
				err = runTrimStep(ctx, r.Out, keyPath, user, ip, wait, timeout)
			}
		default:
			return fmt.Errorf("step %q: unknown scope %q", step.Name, step.Scope)
		}
		if err != nil {
			return fmt.Errorf("step %q: %w", step.Name, err)
		}
	}
	return nil
}

// runLocalShell executes cmd via "sh -c" on the local machine, streaming output to r.Out.
func (r *Runner) runLocalShell(ctx context.Context, cmd string) error {
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Stdout = r.Out
	c.Stderr = r.Out
	return c.Run()
}

// mergeInfoIntoMetrics copies infoMap into the flat Metrics map and into
// StructuredMetrics.InfoLabels so collectors like VictoriaMetrics can attach
// text metadata (pg_version, extension_version, and so on) to benchctl_run_info.
func mergeInfoIntoMetrics(infoMap map[string]string, metrics *Metrics) {
	if len(infoMap) == 0 {
		return
	}
	if *metrics == nil {
		*metrics = make(Metrics)
	}
	for k, v := range infoMap {
		(*metrics)[k] = v
	}
	if sm, ok := (*metrics)[StructuredKey].(StructuredMetrics); ok {
		if sm.InfoLabels == nil {
			sm.InfoLabels = make(map[string]string)
		}
		maps.Copy(sm.InfoLabels, infoMap)
		(*metrics)[StructuredKey] = sm
	}
}

// lastWorkloadStep returns the index of the final step in steps handled by a
// workload adapter, or -1 when there is none. workload.execute is marked
// completed at that step, so a trailing metadata, sql, or collect step cannot
// leave the phase reading running for the rest of the run.
func lastWorkloadStep(steps []schema.SuiteStep, workloads map[string]WorkloadAdapter) int {
	last := -1
	for i, step := range steps {
		if _, ok := workloads[step.Type]; ok {
			last = i
		}
	}
	return last
}

// mergeMetrics folds src into dst, appending structured points rather than
// replacing them so that several steps of one benchmark entry each contribute.
// A step that measures nothing (a data load) returns empty Metrics and leaves
// dst untouched.
func mergeMetrics(dst *Metrics, src Metrics) {
	if len(src) == 0 {
		return
	}
	if *dst == nil {
		*dst = make(Metrics, len(src))
	}
	srcPoints, _ := src[StructuredKey].(StructuredMetrics)
	for k, v := range src {
		if k == StructuredKey {
			continue
		}
		(*dst)[k] = v
	}
	if len(srcPoints.Points) == 0 && srcPoints.InfoLabels == nil {
		return
	}
	sm, _ := (*dst)[StructuredKey].(StructuredMetrics)
	sm.Points = append(sm.Points, srcPoints.Points...)
	if srcPoints.InfoLabels != nil {
		if sm.InfoLabels == nil {
			sm.InfoLabels = make(map[string]string, len(srcPoints.InfoLabels))
		}
		maps.Copy(sm.InfoLabels, srcPoints.InfoLabels)
	}
	(*dst)[StructuredKey] = sm
}

// labelStepMetrics adds a "step" label to every structured point and
// namespaces m["raw_samples_csv"] (go-tpc's per-tick CSV, if present) by
// step name via RawSamplesCSVKey so that multiple steps contributing to
// the same metric family (e.g. a warm-up run followed by the measured run)
// stay distinguishable.
func labelStepMetrics(m Metrics, stepName string) {
	if sm, ok := m[StructuredKey].(StructuredMetrics); ok {
		tagPointsWithStep(sm.Points, stepName)
		m[StructuredKey] = sm
	}
	if raw, ok := m["raw_samples_csv"].(string); ok && raw != "" {
		delete(m, "raw_samples_csv")
		m[RawSamplesCSVKey(stepName)] = raw
	}
}

// tagPointsWithStep sets a "step" label to stepName on every point.
func tagPointsWithStep(points []MetricPoint, stepName string) {
	for i := range points {
		if points[i].Labels == nil {
			points[i].Labels = map[string]string{}
		}
		points[i].Labels["step"] = stepName
	}
}

// writeInitialMetadata resolves the collector config and writes a partial
// metadata record (scenario labels + effective_date + collector-provided keys
// such as endpoint) to run state immediately at run start. infoMap is not yet
// available at this point; Run and Resume merge it in after driver.collect.
// Errors are silently ignored: metadata is best-effort and must never block a run.
func (r *Runner) writeInitialMetadata(runID string, s *schema.Scenario, tc *TemplateContext) {
	cfg, err := resolveConfigMap(s.Collector.Config, tc)
	if err != nil {
		return
	}
	var collectorMeta map[string]string
	if cm, ok := r.Collector.(CollectorMetadata); ok {
		collectorMeta = cm.RunMetadata(cfg)
	}
	m := buildRunMetadata(s.Metadata.Labels, nil, cfg, collectorMeta)
	if len(m) == 0 {
		return
	}
	r.setState(runID, func(st *runstate.State) {
		st.Metadata = m
	})
}

// buildRunMetadata merges four sources into a flat string map (later keys win):
//  1. scenarioLabels: static labels from scenario.metadata.labels
//  2. infoMap: workload.info results (e.g. pg_version, extension_version)
//  3. collectorCfg: the effective_date key, if non-empty, resolved to a
//     concrete timestamp
//  4. collectorMeta: collector-provided metadata (e.g. endpoint resolved
//     from env)
func buildRunMetadata(scenarioLabels, infoMap map[string]string, collectorCfg map[string]any, collectorMeta map[string]string) map[string]string {
	m := make(map[string]string)
	maps.Copy(m, scenarioLabels)
	maps.Copy(m, infoMap)
	if ed, _ := collectorCfg["effective_date"].(string); ed != "" {
		if resolved := resolveEffectiveDate(ed); resolved != "" {
			m["effective_date"] = resolved
		}
	}
	maps.Copy(m, collectorMeta)
	if len(m) == 0 {
		return nil
	}
	return m
}

// resolveEffectiveDate converts the effective_date input to a concrete UTC timestamp
// string ("YYYY-MM-DD 00:00:00") so the state store always records what was actually
// used rather than an opaque token like "auto".
//
//   - "auto"     → midnight UTC today
//   - "YYYYMMDD" → midnight UTC of that date
//   - ""         → "" (no effective date; caller should not store)
func resolveEffectiveDate(ed string) string {
	switch {
	case ed == "":
		return ""
	case ed == "auto":
		now := time.Now().UTC()
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Format("2006-01-02 15:04:05")
	default:
		t, err := time.ParseInLocation("20060102", ed, time.UTC)
		if err != nil {
			return ed // unrecognized format; store as-is, the VM collector surfaces the error
		}
		return t.Format("2006-01-02 15:04:05")
	}
}

// suiteIterations resolves the number of times to repeat the full benchmark
// sequence, expanding any "{{ inputs.X }}" template in suite.Iterations.
// Returns 1 when Iterations is unset, resolves to zero, or is negative.
func suiteIterations(suite schema.Suite, tc *TemplateContext) (int, error) {
	if suite.Iterations == "" {
		return 1, nil
	}
	resolved, err := Resolve(suite.Iterations, tc)
	if err != nil {
		return 0, fmt.Errorf("suite.iterations: %w", err)
	}
	n, err := strconv.Atoi(resolved)
	if err != nil {
		return 0, fmt.Errorf("suite.iterations: expected int, got %q", resolved)
	}
	if n <= 0 {
		return 1, nil
	}
	return n, nil
}

// collectToolVersionLabels returns benchctl's own version plus, for every
// workload adapter registered for this scenario, whatever version info it
// can report. Called once per Run/Resume invocation, not per iteration;
// these values are static for the process's lifetime.
func collectToolVersionLabels(ctx context.Context, workloads map[string]WorkloadAdapter) map[string]string {
	labels := map[string]string{
		"benchctl_version": buildinfo.Version,
	}
	for _, adapter := range workloads {
		if vi, ok := adapter.(WorkloadVersionInfo); ok {
			maps.Copy(labels, vi.VersionInfo(ctx))
		}
	}
	return labels
}

// injectAutoLabels merges scenario-level and automatic labels into
// collectorCfg["labels"] and sets the top-level "iteration" key. Automatic
// labels take precedence over user-specified and scenario-level values, and
// overwriting an existing key logs a warning. Fixture values become
// "fixture_<name>" labels. targetMeta holds the labels contributed by the
// target provider (see TargetMetadata), e.g. the Supabase provider's
// "project_id"; scenarioLabels holds the scenario's static metadata.labels.
func (r *Runner) injectAutoLabels(collectorCfg map[string]any, entry schema.SuiteEntry, iteration int, fixture schema.ResolvedFixture, targetMeta map[string]string, scenarioLabels map[string]string) {
	labels, _ := collectorCfg["labels"].(map[string]any)
	if labels == nil {
		labels = make(map[string]any)
	}
	for k, v := range scenarioLabels {
		if existing, conflict := labels[k]; conflict && r.Out != nil {
			fmt.Fprintf(r.Out, "warning: collector label %q automatically set to %q, overwriting user-specified value %q\n", k, v, existing)
		}
		labels[k] = v
	}
	// service identifies what was benchmarked. For multi-service scenarios
	// (e.g. one engine vs another) it comes from the using: field; for
	// single-engine scenarios (e.g. multigres) where no service is selected,
	// fall back to the benchmark name so the label is always present.
	service := entry.Using
	if service == "" {
		service = entry.Name
	}
	auto := map[string]string{
		"benchmark": entry.Name,
		"service":   service,
	}
	for k, v := range targetMeta {
		auto[k] = v
	}
	for k, v := range fixture {
		auto["fixture_"+k] = v
	}
	for k, v := range auto {
		if existing, conflict := labels[k]; conflict && r.Out != nil {
			fmt.Fprintf(r.Out, "warning: collector label %q automatically set to %q, overwriting user-specified value %q\n", k, v, existing)
		}
		labels[k] = v
	}
	collectorCfg["labels"] = labels
	collectorCfg["iteration"] = iteration
}

// formatFixture formats a ResolvedFixture for log output: "clients=3 protocol=simple scale=10".
// Keys are sorted for determinism.
func formatFixture(combo schema.ResolvedFixture) string {
	if len(combo) == 0 {
		return ""
	}
	keys := make([]string, 0, len(combo))
	for k := range combo {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+combo[k])
	}
	return strings.Join(parts, " ")
}

// storeIsShared reports whether the configured store's records are readable
// from other machines (see runstate.SharedStore).
func (r *Runner) storeIsShared() bool {
	ss, ok := r.Store.(runstate.SharedStore)
	return ok && ss.IsShared()
}

// packTofuState packs the OpenTofu working directories from targetOutputs and
// driverOutputs into a single blob and writes it to the run state store.
//
// A no-op when the store is nil, when neither provider used OpenTofu, or when
// the store is not shared. The blob exists only so teardown can restore the
// work dir on a different machine; with a machine-local store the work dir is
// already on disk here, under ~/.benchctl/tofu-state/<run-id>. connect,
// teardown and fetch all fall back to the on-disk _tofu_work_dir when
// TofuState is empty.
func (r *Runner) packTofuState(runID string, targetOutputs, driverOutputs Outputs) {
	if r.Store == nil || !r.storeIsShared() {
		return
	}
	dirs := make(map[string]string, 2)
	if wd := targetOutputs[OutputKeyTofuWorkDir]; wd != "" {
		dirs["target"] = wd
	}
	if wd := driverOutputs[OutputKeyTofuWorkDir]; wd != "" {
		dirs["driver"] = wd
	}
	if len(dirs) == 0 {
		return
	}
	blob, err := tofustate.Pack(dirs)
	if err != nil {
		if r.Out != nil {
			fmt.Fprintf(r.Out, "warning: pack tofu state: %v\n", err)
		}
		return
	}
	r.setState(runID, func(st *runstate.State) {
		st.TofuState = blob
	})
}

// buildServiceOutputs extracts the connection outputs for the named service
// from the raw provider outputs. Keys like "service.acme.host" become "host".
func buildServiceOutputs(rawOutputs Outputs, using string) Outputs {
	out := make(Outputs)
	prefix := "service." + using + "."
	for k, v := range rawOutputs {
		if strings.HasPrefix(k, prefix) {
			out[strings.TrimPrefix(k, prefix)] = v
		}
	}
	return out
}

// resolveCommandPath makes a script-file command absolute relative to the
// scenario file. If the relative path resolves to an existing file on disk it
// is returned absolute; bare subcommand names (e.g. "run", "cleanup") or
// binaries that don't exist relative to the scenario directory are returned
// unchanged so the adapter / PATH lookup can handle them.
func resolveCommandPath(cmd, scenarioPath string) string {
	if cmd == "" || scenarioPath == "" || filepath.IsAbs(cmd) {
		return cmd
	}
	candidate := filepath.Join(filepath.Dir(scenarioPath), cmd)
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return cmd
}

// rehydrateInputs restores the correct Go types to inputs that have been
// round-tripped through JSON, where int64 becomes float64 and time.Duration
// becomes a float64 nanosecond count.
func rehydrateInputs(raw map[string]any, defs map[string]schema.InputDef) schema.ResolvedInputs {
	result := make(schema.ResolvedInputs, len(raw))
	for k, v := range raw {
		def, ok := defs[k]
		if !ok {
			result[k] = v
			continue
		}
		switch schema.InputType(def.Type) {
		case schema.InputTypeInt:
			if f, ok := v.(float64); ok {
				result[k] = int64(f)
			} else {
				result[k] = v
			}
		case schema.InputTypeDuration:
			switch val := v.(type) {
			case float64:
				// JSON stored time.Duration as nanoseconds.
				result[k] = time.Duration(int64(val))
			case string:
				if d, err := time.ParseDuration(val); err == nil {
					result[k] = d
				} else {
					result[k] = v
				}
			default:
				result[k] = v
			}
		case schema.InputTypeList:
			// JSON round-tripping through the run-state store decodes a
			// []string into []any (json.Unmarshal has no way to know the
			// original element type); ResolveTemplate's v.([]string)
			// check then silently fails, and the whole slice falls through
			// to fmt's default %v formatting (e.g. []any{"1"} prints as the
			// bracketed "[1]", not "1"), corrupting every {{ inputs.X }}
			// reference to a list input on resume. Reconstruct []string
			// explicitly here, the same way InputTypeInt/InputTypeDuration
			// above already undo JSON's type coercion.
			if list, ok := v.([]any); ok {
				strs := make([]string, len(list))
				for i, item := range list {
					strs[i] = fmt.Sprintf("%v", item)
				}
				result[k] = strs
			} else {
				result[k] = v
			}
		default:
			result[k] = v
		}
	}
	return result
}
