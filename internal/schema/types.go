// Package schema parses and validates the bench/v1 scenario YAML that
// describes a benchmark: its inputs, the target and driver providers, and the
// suite of benchmarks to run. It also resolves inputs and expands fixtures
// into the concrete value combinations a run iterates over.
package schema

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Scenario is the top-level resource described by a bench/v1 YAML file.
type Scenario struct {
	APIVersion string              `yaml:"apiVersion"`
	Kind       string              `yaml:"kind"`
	Metadata   Metadata            `yaml:"metadata"`
	Inputs     map[string]InputDef `yaml:"inputs"`
	// InputOrder preserves the declaration order of Inputs' keys as written
	// in the scenario file; Inputs is a map and Go map iteration order is
	// random. Populated by Load; empty for a Scenario built by hand (e.g. in
	// tests) rather than loaded from YAML.
	InputOrder []string  `yaml:"-"`
	Target     Target    `yaml:"target"`
	Suite      Suite     `yaml:"suite"`
	Driver     Driver    `yaml:"driver"`
	Collector  Collector `yaml:"collector"`
}

// Metadata holds scenario identity and labels.
type Metadata struct {
	Name   string            `yaml:"name"`
	Labels map[string]string `yaml:"labels"`
}

// InputDef declares a typed, optionally required parameter.
type InputDef struct {
	Type        string `yaml:"type"`
	Default     any    `yaml:"default"`
	Required    bool   `yaml:"required"`
	Description string `yaml:"description,omitempty"`
}

// InputType enumerates valid input types.
type InputType string

const (
	InputTypeInt      InputType = "int"
	InputTypeString   InputType = "string"
	InputTypeDuration InputType = "duration"
	InputTypeBool     InputType = "bool"
	// InputTypeList declares a list-valued input. It round-trips as a
	// comma-separated string on the CLI/YAML boundary (e.g. --set
	// threads=1,2,4,8) and as []string once resolved.
	InputTypeList InputType = "list"
)

// Target describes the database under test.
type Target struct {
	Provider string            `yaml:"provider"`
	Config   map[string]any    `yaml:"config"`
	Outputs  map[string]string `yaml:"outputs"`
}

// ServiceDef declares a service to deploy on the benchmark target.
type ServiceDef struct {
	Name       string            `yaml:"name"`
	Definition string            `yaml:"definition"`
	Vars       map[string]string `yaml:"vars"`
}

// Suite holds the benchmark entries and optional steps to run between them.
type Suite struct {
	BetweenBenchmarks []BetweenBenchmarksStep `yaml:"between-benchmarks"`
	// Iterations is the number of times to repeat the full benchmark sequence.
	// It accepts a literal integer (e.g. "3") or a "{{ inputs.X }}" template
	// expression, resolved at run time. Empty or unset means 1.
	Iterations string       `yaml:"iterations"`
	Fixtures   []FixtureDef `yaml:"fixtures"`
	Benchmarks []SuiteEntry `yaml:"benchmarks"`
}

// FixtureType enumerates the three fixture generator kinds.
type FixtureType string

const (
	FixtureTypeConstant FixtureType = "constant"
	FixtureTypeRange    FixtureType = "range"
	FixtureTypeList     FixtureType = "list"
)

// FixtureDef describes one fixture axis under suite.fixtures. It supports three
// syntaxes via custom YAML unmarshalling:
//
//   - Constant shorthand: "- scale: 10"
//
//   - Range shorthand (from/to as siblings):
//
//   - clients:
//     from: 1
//     to: 5
//
//   - List shorthand: "- protocols: [simple, extended]"
//
//   - Long form:
//
//   - scale:
//     type: constant
//     params:
//     value: 10
//
// Params values may contain "{{ inputs.X }}" template expressions. A list
// fixture's params.values may also be a single such string referencing a
// list-typed input (e.g. `values: "{{ inputs.threads }}"`), in which case it
// expands to one value per comma-separated element of that input.
type FixtureDef struct {
	Name   string
	Type   FixtureType
	Params map[string]any
}

// ResolvedFixture is one Cartesian-product combination: fixture name → string value.
type ResolvedFixture map[string]string

// BetweenBenchmarksStep is a cleanup or reset action run between consecutive
// benchmarks (after benchmark N, before benchmark N+1). It is not run after
// the final benchmark.
type BetweenBenchmarksStep struct {
	Name    string `yaml:"name"`
	Scope   string `yaml:"scope"`   // "target" | "driver"
	Command string `yaml:"command"` // "shell" | "reboot" | "trim"
	Args    string `yaml:"args"`    // shell command string (unused for "reboot"/"trim")
	// Trim command options (ignored for other commands).
	Wait    *bool  `yaml:"wait,omitempty"`    // trim: block until discards settle; default true
	Timeout string `yaml:"timeout,omitempty"` // trim: max wait duration (e.g. "30m"); negative = no limit
}

// SuiteEntry is one named benchmark in a scenario's suite: an ordered list of
// steps run against a single service from target.config.services.
type SuiteEntry struct {
	Name  string      `yaml:"name"`
	Using string      `yaml:"using"` // service name from target.config.services
	Steps []SuiteStep `yaml:"steps"`
}

// Values a `type: metadata` step may set for `command`. An empty command means
// MetadataCommandSQL, so scenarios written before `command` existed keep working.
const (
	MetadataCommandSQL = "sql"
	MetadataCommandRTT = "rtt"
)

// SuiteStep is a single unit of work within a SuiteEntry.
type SuiteStep struct {
	Name    string            `yaml:"name"`
	Type    string            `yaml:"type"`    // "metadata" | "sql" | "go-tpc" | "k6"
	Command string            `yaml:"command"` // "sql"|"rtt" (metadata); "prepare"|"run" (go-tpc); script path (k6); unused for "sql"
	Args    map[string]string `yaml:"args"`
}

// Driver describes where the load generator runs.
type Driver struct {
	Provider string         `yaml:"provider"`
	Config   map[string]any `yaml:"config"`
}

// Collector describes where results are stored.
type Collector struct {
	Provider string         `yaml:"provider"`
	Config   map[string]any `yaml:"config"`
}

// ResolvedInputs holds typed input values after defaults and overrides are applied.
// Values are typed: int64 for "int", string for "string", time.Duration for
// "duration", bool for "bool", []string for "list".
type ResolvedInputs map[string]any

// UnmarshalYAML implements yaml.Unmarshaler for FixtureDef. Each fixture list
// item decodes as a map and is interpreted according to its shape:
//
//   - Range shorthand:  top-level "from" key present → name is the other key
//   - Long form:        single key whose value is a map containing "type"
//   - List shorthand:   single key whose value is a []any
//   - Constant shorthand: single key with a scalar value
func (f *FixtureDef) UnmarshalYAML(value *yaml.Node) error {
	var raw map[string]any
	if err := value.Decode(&raw); err != nil {
		return err
	}

	// Range shorthand: "- clients:\n  from: 1\n  to: 5" decodes with from/to as
	// top-level sibling keys alongside the fixture name (which has a nil value).
	if _, hasFrom := raw["from"]; hasFrom {
		reserved := map[string]bool{"from": true, "to": true, "step": true}
		for k := range raw {
			if !reserved[k] {
				f.Name = k
				break
			}
		}
		if f.Name == "" {
			return fmt.Errorf("range fixture shorthand is missing a name key (only from/to/step found)")
		}
		f.Type = FixtureTypeRange
		f.Params = map[string]any{"min": raw["from"], "max": raw["to"]}
		if s, ok := raw["step"]; ok && s != nil {
			f.Params["step"] = s
		}
		return nil
	}

	// All other forms: the map must have exactly one key (the fixture name).
	// Its value determines which form this is.
	if len(raw) != 1 {
		return fmt.Errorf("fixture entry must have exactly one name key (got %d keys); use \"from:\" for a range shorthand", len(raw))
	}
	for k, v := range raw {
		f.Name = k
		switch val := v.(type) {
		case map[string]any:
			// Long form: "- scale:\n    type: constant\n    params:\n      value: 10"
			typeStr, _ := val["type"].(string)
			if typeStr == "" {
				return fmt.Errorf("fixture %q: long form requires a \"type:\" key inside the nested mapping", k)
			}
			f.Type = FixtureType(typeStr)
			if p, ok := val["params"].(map[string]any); ok {
				f.Params = p
			} else {
				f.Params = make(map[string]any)
			}
		case []any:
			// List shorthand: "- protocols: [simple, extended]"
			f.Type = FixtureTypeList
			f.Params = map[string]any{"values": val}
		default:
			// Constant shorthand: "- scale: 10"
			f.Type = FixtureTypeConstant
			f.Params = map[string]any{"value": fmt.Sprintf("%v", v)}
		}
	}
	return nil
}
