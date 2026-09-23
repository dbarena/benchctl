package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dbarena/benchctl/internal/auth"
	"github.com/dbarena/benchctl/internal/engine"
	"github.com/dbarena/benchctl/internal/runstate"
)

var (
	statusAll    bool
	statusMine   bool
	statusOutput string
)

var statusCmd = &cobra.Command{
	Use:   "status [run-id]",
	Short: "Show the active runs, or the detail for one run ID",
	Args:  cobra.RangeArgs(0, 1),
	RunE:  runStatus,
}

func init() {
	statusCmd.Flags().BoolVarP(&statusAll, "all", "a", false, "Include runs whose environment has been torn down")
	statusCmd.Flags().BoolVarP(&statusMine, "mine", "m", false, "Only show runs created by the current user")
	statusCmd.Flags().StringVarP(&statusOutput, "output", "o", "text", "Output format: text | json")
}

func runStatus(_ *cobra.Command, args []string) error {
	if statusOutput != "text" && statusOutput != "json" {
		return fmt.Errorf("invalid --output %q: must be text or json", statusOutput)
	}

	store, err := openStore()
	if err != nil {
		return err
	}

	if len(args) == 1 {
		if statusOutput == "json" {
			return printRunDetailJSON(store, args[0])
		}
		return printRunDetail(store, args[0])
	}
	if statusOutput == "json" {
		return printRunListJSON(store)
	}
	return printRunList(store)
}

// printRunDetailJSON writes the full run state as JSON. It is the escape hatch
// for external tooling that needs fields the human-readable view omits, such as
// target_outputs.project_ref.
func printRunDetailJSON(store runstate.Store, runID string) error {
	state, err := store.Load(runID)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(state)
}

// listForStatus fetches the run list for a status listing. Without --all it
// prefers the store's server-side active-only filter, where the store has one,
// over the full List(), so the runs showsByDefault excludes never cross the
// wire: some carry large blob fields (tofu_state, error) that would otherwise be
// fetched and discarded.
func listForStatus(store runstate.Store) ([]*runstate.State, error) {
	if !statusAll {
		if al, ok := store.(runstate.ActiveLister); ok {
			return al.ListActive()
		}
	}
	return store.List()
}

// printRunListJSON writes the run list (respecting --all and --mine) as JSON.
func printRunListJSON(store runstate.Store) error {
	states, err := listForStatus(store)
	if err != nil {
		return err
	}
	states, err = filterRunList(states)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(states)
}

// filterRunList applies the --all and --mine flags to a run list.
func filterRunList(states []*runstate.State) ([]*runstate.State, error) {
	if !statusAll {
		var active []*runstate.State
		for _, st := range states {
			if showsByDefault(st) {
				active = append(active, st)
			}
		}
		states = active
	}
	if statusMine {
		me, _ := resolveCreatedBy(appCfg)
		if me == "" {
			return nil, fmt.Errorf("cannot determine the current user identity; log in with 'benchctl auth login' or set BENCHCTL_USERNAME")
		}
		var mine []*runstate.State
		for _, st := range states {
			if st.CreatedBy == me {
				mine = append(mine, st)
			}
		}
		states = mine
	}
	return states, nil
}

// showsByDefault reports whether a run appears in the default (non --all)
// status list: while the run itself is still active, or while its
// environment remains up even after the benchmark has finished.
func showsByDefault(st *runstate.State) bool {
	return !st.IsTerminal() || environmentStatus(st) == "running"
}

func printRunDetail(store runstate.Store, runID string) error {
	state, err := store.Load(runID)
	if err != nil {
		return err
	}

	resolve := buildCreatedByResolver()
	fmt.Printf("Run:         %s\n", state.RunID)
	fmt.Printf("Scenario:    %s (%s)\n", state.ScenarioName, state.ScenarioPath)
	if state.CreatedBy != "" {
		fmt.Printf("Created by:  %s\n", resolve(state))
	}
	fmt.Printf("Started:     %s\n", state.StartedAt.Format("2006-01-02 15:04:05 UTC"))
	if state.CompletedAt != nil {
		fmt.Printf("Completed:   %s\n", state.CompletedAt.Format("2006-01-02 15:04:05 UTC"))
	}
	fmt.Printf("Environment: %s", environmentStatus(state))
	if state.TerminatedAt != nil {
		fmt.Printf(" (%s)", state.TerminatedAt.Format("2006-01-02 15:04:05 UTC"))
	}
	fmt.Println()
	if ts := tofuStateDesc(state); ts != "" {
		fmt.Printf("Tofu state:  %s\n", ts)
	}
	if state.LastHeartbeat != nil {
		fmt.Printf("Heartbeat:   %s\n", state.LastHeartbeat.Format("2006-01-02 15:04:05 UTC"))
	}
	if state.IsStale() {
		if state.RefreshedAt.IsZero() {
			fmt.Printf("Snapshot:    never refreshed from the driver instance: %s\n", state.RefreshError)
		} else {
			fmt.Printf("Snapshot:    %s old, refresh failed: %s\n",
				time.Since(state.RefreshedAt).Round(time.Second), state.RefreshError)
		}
	}
	if state.CompletedAt == nil && state.CurrentStep != "" {
		fmt.Printf("Current step: %s", state.CurrentStep)
		// CurrentIteration is a zero-based loop index; iterations are reported
		// from 1 here and in the collector labels. Fixtures are optional, so
		// the iteration stands alone in a scenario that declares none.
		if len(state.CurrentFixture) > 0 {
			fmt.Printf(" (fixture: %s, iteration: %d)", formatFixtureMap(state.CurrentFixture), state.CurrentIteration+1)
		} else {
			fmt.Printf(" (iteration: %d)", state.CurrentIteration+1)
		}
		if state.StepStartedAt != nil {
			fmt.Printf(" [started %s]", state.StepStartedAt.Format("2006-01-02 15:04:05 UTC"))
		}
		fmt.Println()
	}

	fmt.Println()
	fmt.Println("Phases:")
	for _, p := range runstate.AllPhases {
		fmt.Printf("  %-24s %s\n", p, state.Phases[p])
	}

	if len(state.Metadata) > 0 {
		fmt.Println()
		fmt.Println("Metadata:")
		keys := make([]string, 0, len(state.Metadata))
		for k := range state.Metadata {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("  %-20s %s\n", k+":", state.Metadata[k])
		}
	}

	if state.Error != "" {
		fmt.Printf("\nError: %s\n", state.Error)
	}

	return nil
}

func printRunList(store runstate.Store) error {
	states, err := listForStatus(store)
	if err != nil {
		return err
	}

	states, err = filterRunList(states)
	if err != nil {
		return err
	}

	if len(states) == 0 {
		switch {
		case statusAll && statusMine:
			fmt.Fprintln(os.Stderr, "no runs found for the current user")
		case statusAll:
			fmt.Fprintln(os.Stderr, "no runs found")
		case statusMine:
			fmt.Fprintln(os.Stderr, "no active runs or running environments for the current user (use --all to include torn-down runs)")
		default:
			fmt.Fprintln(os.Stderr, "no active runs or running environments (use --all to include torn-down runs)")
		}
		return nil
	}

	resolve := buildCreatedByResolver()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "RUN ID\tSCENARIO\tCREATED BY\tSTARTED\tSTATUS\tENVIRONMENT\tEFFECTIVE DATE\tMETRICS\tTOFU STATE")
	for _, st := range states {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			st.RunID,
			st.ScenarioName,
			resolve(st),
			st.StartedAt.Format("2006-01-02 15:04:05"),
			overallStatus(st)+staleSuffix(st),
			environmentStatus(st),
			st.Metadata["effective_date"],
			metricsStore(st),
			tofuStateShort(st),
		)
	}
	return w.Flush()
}

// staleThreshold is how long without a heartbeat before a running run is flagged [stale].
const staleThreshold = 10 * time.Minute

// formatFixtureMap formats a fixture combination for display, e.g.
// "clients=8" or "clients=8, protocol=simple" for multiple axes, in
// deterministic (sorted) key order.
func formatFixtureMap(fixture map[string]string) string {
	keys := make([]string, 0, len(fixture))
	for k := range fixture {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, fixture[k]))
	}
	return strings.Join(parts, ", ")
}

// overallStatus derives a single human-readable status from the phase map.
func overallStatus(st *runstate.State) string {
	if st.Error != "" {
		return "failed"
	}
	if st.CompletedAt != nil {
		return "completed"
	}
	for i := len(runstate.AllPhases) - 1; i >= 0; i-- {
		p := runstate.AllPhases[i]
		switch st.Phases[p] {
		case runstate.StatusRunning:
			// LastHeartbeat is written while a workload step runs (see
			// Runner.startHeartbeat), so staleness is only meaningful for
			// workload.execute; it says nothing about liveness of any other
			// phase.
			if p == runstate.PhaseWorkloadExecute &&
				st.LastHeartbeat != nil && time.Since(*st.LastHeartbeat) > staleThreshold {
				return fmt.Sprintf("running (%s) [stale]", p)
			}
			return fmt.Sprintf("running (%s)", p)
		case runstate.StatusFailed:
			return fmt.Sprintf("failed (%s)", p)
		}
	}
	return "pending"
}

// staleSuffix flags a run whose state benchctl could not read back from the
// driver instance executing it, warning that the phases shown below may have
// moved on. A current snapshot gets an empty suffix.
func staleSuffix(st *runstate.State) string {
	if !st.IsStale() {
		return ""
	}
	if st.RefreshedAt.IsZero() {
		return " [unreachable]"
	}
	return fmt.Sprintf(" [stale, %s old]", time.Since(st.RefreshedAt).Round(time.Second))
}

// metricsStore returns the metrics destination for the run: the endpoint URL
// when metrics were shipped to VictoriaMetrics, or "local" for stdout runs.
func metricsStore(st *runstate.State) string {
	if ep := st.Metadata["endpoint"]; ep != "" {
		return ep
	}
	return "local"
}

// tofuStateShort returns "remote", "local", or "-" for the list view.
func tofuStateShort(st *runstate.State) string {
	if st.TofuState != "" {
		return "remote"
	}
	if st.TargetOutputs[engine.OutputKeyTofuWorkDir] != "" || st.DriverOutputs[engine.OutputKeyTofuWorkDir] != "" {
		return "local"
	}
	return "-"
}

// tofuStateDesc returns a human-readable description of where the OpenTofu
// state for a run is stored. Returns "" when no tofu state is present.
func tofuStateDesc(st *runstate.State) string {
	if st.TofuState != "" {
		return "stored (in the run record)"
	}
	var parts []string
	if wd := st.TargetOutputs[engine.OutputKeyTofuWorkDir]; wd != "" {
		parts = append(parts, "target: "+wd)
	}
	if wd := st.DriverOutputs[engine.OutputKeyTofuWorkDir]; wd != "" {
		parts = append(parts, "driver: "+wd)
	}
	if len(parts) == 0 {
		return ""
	}
	return "local (" + strings.Join(parts, ", ") + ")"
}

// buildCreatedByResolver returns a function that maps a run's created_by to
// a human-readable display string. It prefers the email persisted on the run
// itself (CreatedByEmail, captured at run creation, so it displays correctly
// for every user). For runs created before that field existed, it falls back
// to substituting the current viewer's own UUID with their email, the only UUID
// a JWT can resolve without an admin API lookup. Any other UUID, or a CI
// identity like "github:{actor}", passes through unchanged. Empty is "-".
func buildCreatedByResolver() func(*runstate.State) string {
	uuidToEmail := make(map[string]string)
	if creds, err := auth.LoadCredentials(); err == nil && creds.Valid() {
		if sub := jwtSub(creds.AccessToken); sub != "" {
			if email := jwtEmail(creds.AccessToken); email != "" {
				uuidToEmail[sub] = email
			}
		}
	}
	return func(st *runstate.State) string {
		if st.CreatedByEmail != "" {
			return st.CreatedByEmail
		}
		if st.CreatedBy == "" {
			return "-"
		}
		if display, ok := uuidToEmail[st.CreatedBy]; ok {
			return display
		}
		return st.CreatedBy
	}
}

// environmentStatus reports the infrastructure state for a run.
// Returns "terminated" once TerminatedAt is set, "running" once provision has
// completed (TargetOutputs is non-nil), and "-" before any infrastructure exists.
func environmentStatus(st *runstate.State) string {
	if st.TerminatedAt != nil {
		return "terminated"
	}
	if st.TargetOutputs != nil {
		return "running"
	}
	return "-"
}
