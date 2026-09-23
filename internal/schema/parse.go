package schema

import (
	"fmt"
	"maps"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Load reads and parses a scenario YAML file.
func Load(path string) (*Scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var s Scenario
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	s.InputOrder = parseInputOrder(data)
	return &s, nil
}

// parseInputOrder returns the declaration order of the top-level inputs.<name>
// keys in a scenario YAML document, by walking the raw node tree (a
// map[string]InputDef loses order on unmarshal, since Go map iteration order
// is random). Returns nil if there's no top-level inputs mapping; callers
// should fall back to sorted order in that case.
func parseInputOrder(data []byte) []string {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil || len(doc.Content) == 0 {
		return nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "inputs" {
			continue
		}
		inputsNode := root.Content[i+1]
		if inputsNode.Kind != yaml.MappingNode {
			return nil
		}
		order := make([]string, 0, len(inputsNode.Content)/2)
		for j := 0; j+1 < len(inputsNode.Content); j += 2 {
			order = append(order, inputsNode.Content[j].Value)
		}
		return order
	}
	return nil
}

// Validate checks required fields and semantic correctness.
func (s *Scenario) Validate() error {
	if s.APIVersion != "bench/v1" {
		return fmt.Errorf("apiVersion must be bench/v1, got %q", s.APIVersion)
	}
	if s.Kind != "Scenario" {
		return fmt.Errorf("kind must be Scenario, got %q", s.Kind)
	}
	if s.Metadata.Name == "" {
		return fmt.Errorf("metadata.name is required")
	}
	if s.Target.Provider == "" {
		return fmt.Errorf("target.provider is required")
	}
	if s.Driver.Provider == "" {
		return fmt.Errorf("driver.provider is required")
	}
	if s.Collector.Provider == "" {
		return fmt.Errorf("collector.provider is required")
	}

	for name, def := range s.Inputs {
		switch InputType(def.Type) {
		case InputTypeInt, InputTypeString, InputTypeDuration, InputTypeBool, InputTypeList:
		default:
			return fmt.Errorf("inputs.%s.type must be int|string|duration|bool|list, got %q", name, def.Type)
		}
	}

	if it := s.Suite.Iterations; it != "" && !strings.Contains(it, "{{") {
		if n, err := strconv.Atoi(it); err != nil || n < 0 {
			return fmt.Errorf("suite.iterations: must be a non-negative integer or a {{ inputs.X }} template, got %q", it)
		}
	}

	seenFixture := make(map[string]bool, len(s.Suite.Fixtures))
	for i, fd := range s.Suite.Fixtures {
		if fd.Name == "" {
			return fmt.Errorf("suite.fixtures[%d]: missing name key", i)
		}
		if seenFixture[fd.Name] {
			return fmt.Errorf("suite.fixtures[%d]: duplicate fixture name %q", i, fd.Name)
		}
		seenFixture[fd.Name] = true
		switch fd.Type {
		case FixtureTypeConstant:
			if fd.Params["value"] == nil {
				return fmt.Errorf("suite.fixtures[%d] (%q): constant fixture requires params.value", i, fd.Name)
			}
		case FixtureTypeList:
			switch vals := fd.Params["values"].(type) {
			case []any:
				if len(vals) == 0 {
					return fmt.Errorf("suite.fixtures[%d] (%q): list fixture requires non-empty params.values", i, fd.Name)
				}
			case string:
				if strings.TrimSpace(vals) == "" {
					return fmt.Errorf("suite.fixtures[%d] (%q): list fixture requires non-empty params.values", i, fd.Name)
				}
			default:
				return fmt.Errorf("suite.fixtures[%d] (%q): list fixture params.values must be a sequence or a {{ inputs.X }} string reference", i, fd.Name)
			}
		case FixtureTypeRange:
			if fd.Params["min"] == nil {
				return fmt.Errorf("suite.fixtures[%d] (%q): range fixture requires params.min (or from: shorthand)", i, fd.Name)
			}
			if fd.Params["max"] == nil {
				return fmt.Errorf("suite.fixtures[%d] (%q): range fixture requires params.max (or to: shorthand)", i, fd.Name)
			}
		default:
			return fmt.Errorf("suite.fixtures[%d] (%q): unknown type %q (valid: constant, range, list)", i, fd.Name, fd.Type)
		}
	}

	if len(s.Suite.Benchmarks) == 0 {
		return fmt.Errorf("suite.benchmarks is required and must have at least one entry")
	}
	for i, entry := range s.Suite.Benchmarks {
		if len(entry.Steps) == 0 {
			return fmt.Errorf("suite.benchmarks[%d] (%q): at least one step is required", i, entry.Name)
		}
		for j, step := range entry.Steps {
			if step.Type == "sql" && step.Args["query"] == "" {
				return fmt.Errorf("suite.benchmarks[%d].steps[%d] (%q): type \"sql\" requires args.query", i, j, step.Name)
			}
			if step.Type == "metadata" {
				if step.Args["name"] == "" {
					return fmt.Errorf("suite.benchmarks[%d].steps[%d] (%q): type \"metadata\" requires args.name", i, j, step.Name)
				}
				switch step.Command {
				case "", MetadataCommandSQL:
					if step.Args["query"] == "" {
						return fmt.Errorf("suite.benchmarks[%d].steps[%d] (%q): type \"metadata\" with command %q requires args.query", i, j, step.Name, MetadataCommandSQL)
					}
				case MetadataCommandRTT:
				default:
					return fmt.Errorf("suite.benchmarks[%d].steps[%d] (%q): type \"metadata\" has unknown command %q (want %q or %q)", i, j, step.Name, step.Command, MetadataCommandSQL, MetadataCommandRTT)
				}
			}
			if step.Type == "shell" && step.Args["command"] == "" {
				return fmt.Errorf("suite.benchmarks[%d].steps[%d] (%q): type \"shell\" requires args.command", i, j, step.Name)
			}
		}
	}
	for i, step := range s.Suite.BetweenBenchmarks {
		if step.Name == "" {
			return fmt.Errorf("suite.between-benchmarks[%d]: name is required", i)
		}
		if step.Scope == "" {
			return fmt.Errorf("suite.between-benchmarks[%d] (%q): scope is required", i, step.Name)
		}
		switch step.Command {
		case "shell":
			if step.Args == "" {
				return fmt.Errorf("suite.between-benchmarks[%d] (%q): command \"shell\" requires args", i, step.Name)
			}
		case "reboot":
			if step.Scope == "driver" {
				return fmt.Errorf("suite.between-benchmarks[%d] (%q): command \"reboot\" is not valid for scope \"driver\"", i, step.Name)
			}
		case "trim":
			if step.Scope == "driver" {
				return fmt.Errorf("suite.between-benchmarks[%d] (%q): command \"trim\" is not valid for scope \"driver\"", i, step.Name)
			}
			if step.Timeout != "" {
				if _, err := time.ParseDuration(step.Timeout); err != nil {
					return fmt.Errorf("suite.between-benchmarks[%d] (%q): invalid timeout %q: %w", i, step.Name, step.Timeout, err)
				}
			}
		case "":
			return fmt.Errorf("suite.between-benchmarks[%d] (%q): command is required", i, step.Name)
		default:
			return fmt.Errorf("suite.between-benchmarks[%d] (%q): unknown command %q (valid: shell, reboot, trim)", i, step.Name, step.Command)
		}
	}

	var serviceNames map[string]bool
	if s.Target.Config["services"] != nil {
		defs, err := ParseServiceDefs(s.Target.Config["services"])
		if err != nil {
			return fmt.Errorf("target.config.services: %w", err)
		}
		serviceNames = make(map[string]bool, len(defs))
		for _, svc := range defs {
			serviceNames[svc.Name] = true
		}
	}

	// Every using: reference must have a matching service definition.
	for _, entry := range s.Suite.Benchmarks {
		if entry.Using != "" && !serviceNames[entry.Using] {
			return fmt.Errorf("benchmark %q uses service %q but it is not defined in target.config.services", entry.Name, entry.Using)
		}
	}

	return nil
}

// ParseServiceDefs parses the services key from a YAML-decoded config map.
// raw is expected to be []any where each element is map[string]any.
// Returns an error if any entry is missing name or definition.
func ParseServiceDefs(raw any) ([]ServiceDef, error) {
	if raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("must be a sequence, got %T", raw)
	}
	services := make([]ServiceDef, 0, len(list))
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("entry %d: must be a mapping, got %T", i, item)
		}
		var svc ServiceDef
		svc.Name, _ = m["name"].(string)
		if svc.Name == "" {
			return nil, fmt.Errorf("entry %d: name is required", i)
		}
		svc.Definition, _ = m["definition"].(string)
		if svc.Definition == "" {
			return nil, fmt.Errorf("entry %d: definition is required", i)
		}
		if vars, ok := m["vars"].(map[string]any); ok {
			svc.Vars = make(map[string]string, len(vars))
			for k, v := range vars {
				svc.Vars[k] = fmt.Sprintf("%v", v)
			}
		}
		services = append(services, svc)
	}
	return services, nil
}

// ResolveInputs merges declared defaults with CLI overrides and returns typed values.
// overrides maps input name to raw string value (from --set flags).
func (s *Scenario) ResolveInputs(overrides map[string]string) (ResolvedInputs, error) {
	// Reject overrides for undeclared inputs early.
	for name := range overrides {
		if _, ok := s.Inputs[name]; !ok {
			return nil, fmt.Errorf("unknown input %q (not declared in scenario)", name)
		}
	}

	resolved := make(ResolvedInputs, len(s.Inputs))
	for name, def := range s.Inputs {
		raw, hasOverride := overrides[name]
		v, ok, err := resolveInput(def, raw, hasOverride)
		if err != nil {
			return nil, fmt.Errorf("input %s: %w", name, err)
		}
		if !ok {
			if def.Required {
				return nil, fmt.Errorf("input %q is required: provide a value with --set %s=<value>", name, name)
			}
			// No default and no override: input is unset (omitted).
			continue
		}
		resolved[name] = v
	}
	return resolved, nil
}

// ResolveInputsPartial resolves as many inputs as possible from declared
// defaults and the provided overrides, for display purposes (e.g. `benchctl
// info --set`). Unlike ResolveInputs, it never fails: an input that can't be
// resolved (a required input with no default or override, or a value that
// fails to parse) is simply left out of resolved, and the reason is recorded
// in errs, keyed by input name. Overrides for undeclared inputs are ignored
// here; callers that need to reject those (as ResolveInputs does) should
// check separately.
func (s *Scenario) ResolveInputsPartial(overrides map[string]string) (resolved ResolvedInputs, errs map[string]error) {
	resolved = make(ResolvedInputs, len(s.Inputs))
	errs = make(map[string]error)
	for name, def := range s.Inputs {
		raw, hasOverride := overrides[name]
		v, ok, err := resolveInput(def, raw, hasOverride)
		if err != nil {
			errs[name] = err
			continue
		}
		if !ok {
			if def.Required {
				errs[name] = fmt.Errorf("required, no default or override provided")
			}
			continue
		}
		resolved[name] = v
	}
	return resolved, errs
}

// resolveInput resolves a single input's typed value from an override
// (when hasOverride) or its declared default. ok is false when there is
// neither an override nor a default: the input is simply unset, and the caller
// decides whether that's an error (it is, only when the input is required).
func resolveInput(def InputDef, raw string, hasOverride bool) (v any, ok bool, err error) {
	if !hasOverride {
		if def.Default == nil {
			return nil, false, nil
		}
		raw, err = defaultToString(def.Default)
		if err != nil {
			return nil, false, fmt.Errorf("default: %w", err)
		}
	}
	v, err = coerce(InputType(def.Type), raw)
	if err != nil {
		return nil, false, err
	}
	return v, true, nil
}

// defaultToString converts a YAML-decoded default value to its string representation.
// A []any default (a YAML sequence, used for list-typed inputs) is joined with
// commas rather than formatted as a Go slice.
func defaultToString(v any) (string, error) {
	if v == nil {
		return "", fmt.Errorf("nil default")
	}
	if items, ok := v.([]any); ok {
		parts := make([]string, len(items))
		for i, item := range items {
			parts[i] = fmt.Sprintf("%v", item)
		}
		return strings.Join(parts, ","), nil
	}
	return fmt.Sprintf("%v", v), nil
}

// fixtureInputRe matches {{ inputs.<name> }} in fixture param strings.
var fixtureInputRe = regexp.MustCompile(`\{\{\s*inputs\.(\w+)\s*\}\}`)

// ResolveTemplate substitutes "{{ inputs.X }}" references in s using the
// provided resolved inputs, for fixture params and suite.iterations.
// Only the inputs namespace is supported; a reference to an input not present
// in inputs (e.g. a required input with no default and no --set override) is
// left as-is.
func ResolveTemplate(s string, inputs ResolvedInputs) string {
	return fixtureInputRe.ReplaceAllStringFunc(s, func(match string) string {
		sub := fixtureInputRe.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		v, ok := inputs[sub[1]]
		if !ok {
			return match
		}
		if list, ok := v.([]string); ok {
			return strings.Join(list, ",")
		}
		return fmt.Sprintf("%v", v)
	})
}

// ExpandFixtureAxis resolves one fixture's declared params into the concrete,
// ordered list of string values it takes on, substituting any
// "{{ inputs.X }}" references using inputs, without combining it with any
// other fixture. See ExpandFixtures for the full Cartesian product across
// fixtures.
func ExpandFixtureAxis(fd FixtureDef, inputs ResolvedInputs) ([]string, error) {
	switch fd.Type {
	case FixtureTypeConstant:
		raw := fmt.Sprintf("%v", fd.Params["value"])
		return []string{ResolveTemplate(raw, inputs)}, nil

	case FixtureTypeList:
		switch raw := fd.Params["values"].(type) {
		case []any:
			vals := make([]string, 0, len(raw))
			for _, item := range raw {
				vals = append(vals, ResolveTemplate(fmt.Sprintf("%v", item), inputs))
			}
			return vals, nil
		case string:
			resolved := ResolveTemplate(raw, inputs)
			var vals []string
			for _, part := range strings.Split(resolved, ",") {
				if part = strings.TrimSpace(part); part != "" {
					vals = append(vals, part)
				}
			}
			return vals, nil
		default:
			return nil, fmt.Errorf("fixture %q: list fixture params.values must be a sequence or a {{ inputs.X }} string reference", fd.Name)
		}

	case FixtureTypeRange:
		minStr := ResolveTemplate(fmt.Sprintf("%v", fd.Params["min"]), inputs)
		maxStr := ResolveTemplate(fmt.Sprintf("%v", fd.Params["max"]), inputs)
		stepStr := "1"
		if s := fd.Params["step"]; s != nil {
			stepStr = ResolveTemplate(fmt.Sprintf("%v", s), inputs)
		}
		minV, err := strconv.ParseInt(minStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("fixture %q: min value %q is not an integer: %w", fd.Name, minStr, err)
		}
		maxV, err := strconv.ParseInt(maxStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("fixture %q: max value %q is not an integer: %w", fd.Name, maxStr, err)
		}
		stepV, err := strconv.ParseInt(stepStr, 10, 64)
		if err != nil || stepV <= 0 {
			return nil, fmt.Errorf("fixture %q: step value %q must be a positive integer", fd.Name, stepStr)
		}
		var vals []string
		for v := minV; v <= maxV; v += stepV {
			vals = append(vals, strconv.FormatInt(v, 10))
		}
		if len(vals) == 0 {
			return nil, fmt.Errorf("fixture %q: range [%d, %d] step %d produces no values", fd.Name, minV, maxV, stepV)
		}
		return vals, nil

	default:
		return nil, fmt.Errorf("fixture %q: unknown type %q (valid: constant, range, list)", fd.Name, fd.Type)
	}
}

// ExpandFixtures resolves template expressions in fixture params, generates
// values for each fixture axis, then returns the Cartesian product of all axes
// as a slice of ResolvedFixture maps.
//
// When no fixtures are declared, ExpandFixtures returns a single empty
// ResolvedFixture so the caller's loop runs exactly once (backwards compatible).
func ExpandFixtures(fixtures []FixtureDef, inputs ResolvedInputs) ([]ResolvedFixture, error) {
	if len(fixtures) == 0 {
		return []ResolvedFixture{{}}, nil
	}

	type axis struct {
		name   string
		values []string
	}
	axes := make([]axis, 0, len(fixtures))

	for _, fd := range fixtures {
		vals, err := ExpandFixtureAxis(fd, inputs)
		if err != nil {
			return nil, err
		}
		axes = append(axes, axis{name: fd.Name, values: vals})
	}

	// Iterative Cartesian product: start with one empty combo, multiply in one axis at a time.
	result := []ResolvedFixture{{}}
	for _, ax := range axes {
		next := make([]ResolvedFixture, 0, len(result)*len(ax.values))
		for _, combo := range result {
			for _, val := range ax.values {
				c := maps.Clone(combo)
				c[ax.name] = val
				next = append(next, c)
			}
		}
		result = next
	}
	return result, nil
}

// FixtureNames returns the fixture names in declaration order.
func FixtureNames(fixtures []FixtureDef) []string {
	names := make([]string, len(fixtures))
	for i, fd := range fixtures {
		names[i] = fd.Name
	}
	return names
}

// coerce parses a raw string into the Go type corresponding to t.
// int → int64, string → string, duration → time.Duration, bool → bool.
func coerce(t InputType, raw string) (any, error) {
	switch t {
	case InputTypeString:
		return raw, nil
	case InputTypeInt:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("expected int, got %q", raw)
		}
		return n, nil
	case InputTypeBool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("expected bool, got %q", raw)
		}
		return b, nil
	case InputTypeDuration:
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("expected duration (e.g. 5m, 30s), got %q", raw)
		}
		return d, nil
	case InputTypeList:
		parts := make([]string, 0)
		for _, part := range strings.Split(raw, ",") {
			if part = strings.TrimSpace(part); part != "" {
				parts = append(parts, part)
			}
		}
		if len(parts) == 0 {
			return nil, fmt.Errorf("expected a non-empty comma-separated list, got %q", raw)
		}
		return parts, nil
	default:
		return nil, fmt.Errorf("unknown type %q", t)
	}
}
