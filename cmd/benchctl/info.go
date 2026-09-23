package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dbarena/benchctl/internal/schema"
)

var infoSetFlags []string

var infoCmd = &cobra.Command{
	Use:   "info <scenario-file>",
	Short: "Show a scenario's inputs, fixtures, and benchmark steps",
	Long: `info prints a scenario's declared inputs, fixtures, and benchmark steps
without running it.

Each input shows its resolved value, from the default or a --set override, and
whether it is required. Each fixture shows its resolved values, with any
{{ inputs.X }} reference substituted, plus the total number of combinations.
Use --set to preview an override before committing to a run.`,
	Args: cobra.ExactArgs(1),
	RunE: runInfo,
}

func init() {
	infoCmd.Flags().StringArrayVar(&infoSetFlags, "set", nil, "Override a scenario input (key=value, repeatable) and show its effect")
}

func runInfo(_ *cobra.Command, args []string) error {
	s, err := schema.Load(args[0])
	if err != nil {
		return err
	}

	overrides, err := parseSetFlags(infoSetFlags)
	if err != nil {
		return err
	}
	for name := range overrides {
		if _, ok := s.Inputs[name]; !ok {
			return fmt.Errorf("unknown input %q (not declared in scenario)", name)
		}
	}
	resolved, resolveErrs := s.ResolveInputsPartial(overrides)

	w := os.Stdout
	fmt.Fprintf(w, "Scenario: %s\n\n", s.Metadata.Name)
	printInputsTable(w, s, resolved, resolveErrs, overrides)
	fmt.Fprintln(w)
	printFixtures(w, s.Suite.Fixtures, resolved)
	fmt.Fprintln(w)
	printIterations(w, s.Suite.Iterations, resolved)
	fmt.Fprintln(w)
	printBenchmarks(w, s.Suite.Benchmarks)
	if len(s.Suite.BetweenBenchmarks) > 0 {
		fmt.Fprintln(w)
		printBetweenBenchmarks(w, s.Suite.BetweenBenchmarks)
	}
	return nil
}

// inputOrder returns the declared inputs' names in file declaration order,
// falling back to alphabetical when s.InputOrder is unavailable or stale
// (e.g. a hand-built Scenario rather than one loaded from YAML).
func inputOrder(s *schema.Scenario) []string {
	if len(s.InputOrder) == len(s.Inputs) {
		return s.InputOrder
	}
	names := make([]string, 0, len(s.Inputs))
	for name := range s.Inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func printInputsTable(out io.Writer, s *schema.Scenario, resolved schema.ResolvedInputs, resolveErrs map[string]error, overrides map[string]string) {
	fmt.Fprintln(out, "Inputs:")
	if len(s.Inputs) == 0 {
		fmt.Fprintln(out, "  (none)")
		return
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tTYPE\tVALUE\tREQUIRED\tDESCRIPTION")
	anyOverridden := false
	for _, name := range inputOrder(s) {
		def := s.Inputs[name]
		value := ""
		if v, ok := resolved[name]; ok {
			value = formatResolvedValue(v)
			if _, overridden := overrides[name]; overridden {
				value += " *"
				anyOverridden = true
			}
		} else if err, hasErr := resolveErrs[name]; hasErr {
			value = fmt.Sprintf("<%s>", err)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%t\t%s\n", name, def.Type, value, def.Required, def.Description)
	}
	w.Flush()
	if anyOverridden {
		fmt.Fprintln(out, "* overridden via --set")
	}
}

// formatResolvedValue renders one value from a schema.ResolvedInputs map. Such a
// value is a typed Go value: []string for a list, time.Duration for a duration,
// int64, bool, or string.
func formatResolvedValue(v any) string {
	switch val := v.(type) {
	case []string:
		return strings.Join(val, ",")
	case time.Duration:
		return val.String()
	default:
		return fmt.Sprintf("%v", val)
	}
}

func printFixtures(out io.Writer, fixtures []schema.FixtureDef, resolved schema.ResolvedInputs) {
	fmt.Fprintln(out, "Fixtures:")
	if len(fixtures) == 0 {
		fmt.Fprintln(out, "  (none)")
		return
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, fd := range fixtures {
		fmt.Fprintf(w, "%s (%s)\t%s\n", fd.Name, fd.Type, formatFixtureValues(fd, resolved))
	}
	w.Flush()
	if combos, err := schema.ExpandFixtures(fixtures, resolved); err == nil {
		fmt.Fprintf(out, "Total combinations: %d\n", len(combos))
	}
}

// formatFixtureValues renders a fixture's resolved values, substituting any
// "{{ inputs.X }}" reference from resolved. When the fixture will not resolve,
// for example because it references a required input with no default and no
// --set override, it returns the raw declared params instead.
func formatFixtureValues(fd schema.FixtureDef, resolved schema.ResolvedInputs) string {
	vals, err := schema.ExpandFixtureAxis(fd, resolved)
	if err != nil {
		return formatFixtureParams(fd) + " (unresolved)"
	}
	if fd.Type == schema.FixtureTypeRange && len(vals) > 1 {
		return fmt.Sprintf("%s..%s (%d values)", vals[0], vals[len(vals)-1], len(vals))
	}
	return strings.Join(vals, ", ")
}

// formatFixtureParams renders a fixture's params exactly as written in the
// scenario file, including any unresolved "{{ inputs.X }}" expression.
// formatFixtureValues falls back to this when a fixture will not resolve.
func formatFixtureParams(fd schema.FixtureDef) string {
	switch fd.Type {
	case schema.FixtureTypeConstant:
		return fmt.Sprintf("value: %v", fd.Params["value"])
	case schema.FixtureTypeList:
		switch vals := fd.Params["values"].(type) {
		case []any:
			parts := make([]string, len(vals))
			for i, v := range vals {
				parts[i] = fmt.Sprintf("%v", v)
			}
			return fmt.Sprintf("values: [%s]", strings.Join(parts, ", "))
		default:
			return fmt.Sprintf("values: %v", vals)
		}
	case schema.FixtureTypeRange:
		s := fmt.Sprintf("from: %v to: %v", fd.Params["min"], fd.Params["max"])
		if step, ok := fd.Params["step"]; ok {
			s += fmt.Sprintf(" step: %v", step)
		}
		return s
	default:
		parts := make([]string, 0, len(fd.Params))
		for k, v := range fd.Params {
			parts = append(parts, fmt.Sprintf("%s: %v", k, v))
		}
		sort.Strings(parts)
		return strings.Join(parts, ", ")
	}
}

func printIterations(out io.Writer, iterations string, resolved schema.ResolvedInputs) {
	if iterations == "" {
		fmt.Fprintln(out, "Iterations: 1 (default)")
		return
	}
	value := schema.ResolveTemplate(iterations, resolved)
	if strings.Contains(value, "{{") {
		fmt.Fprintf(out, "Iterations: %s (unresolved)\n", value)
		return
	}
	fmt.Fprintf(out, "Iterations: %s\n", value)
}

func printBenchmarks(out io.Writer, benchmarks []schema.SuiteEntry) {
	fmt.Fprintln(out, "Benchmarks:")
	if len(benchmarks) == 0 {
		fmt.Fprintln(out, "  (none)")
		return
	}
	for _, entry := range benchmarks {
		name := entry.Name
		if entry.Using != "" {
			name = fmt.Sprintf("%s (using: %s)", name, entry.Using)
		}
		fmt.Fprintln(out, name)
		for i, step := range entry.Steps {
			fmt.Fprintf(out, "%s%s\n", treeConnector(i, len(entry.Steps)), formatStep(step))
		}
	}
}

func printBetweenBenchmarks(out io.Writer, steps []schema.BetweenBenchmarksStep) {
	fmt.Fprintln(out, "Between benchmarks:")
	for i, step := range steps {
		fmt.Fprintf(out, "%s%s (%s: %s)\n", treeConnector(i, len(steps)), step.Name, step.Scope, step.Command)
	}
}

// formatStep renders a step's name with its type and, when present, its command:
// "get-pg-version (metadata: sql)", or "print-combo (shell)" for a step type
// that has no command sub-selector.
func formatStep(step schema.SuiteStep) string {
	if step.Command != "" {
		return fmt.Sprintf("%s (%s: %s)", step.Name, step.Type, step.Command)
	}
	return fmt.Sprintf("%s (%s)", step.Name, step.Type)
}

// treeConnector returns the box-drawing prefix for item i of n in a flat
// tree list: "├── " for all but the last item, "└── " for the last.
func treeConnector(i, n int) string {
	if i == n-1 {
		return "└── "
	}
	return "├── "
}
