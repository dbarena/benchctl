package engine

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/dbarena/benchctl/internal/schema"
)

var tmplRe = regexp.MustCompile(`\{\{\s*(\S+)\s*\}\}`)

// TemplateContext holds all values available for template resolution.
type TemplateContext struct {
	Inputs          schema.ResolvedInputs
	TargetOutputs   Outputs
	ProviderOutputs Outputs
	// Info holds results from workload.info queries, populated by the metadata
	// steps that precede a benchmark entry's workload steps.
	Info map[string]string
	// Fixture holds the current fixture combination values, set by the runner
	// before each fixture iteration. Nil when no fixtures are declared.
	Fixture schema.ResolvedFixture
}

// Resolve replaces {{ expr }} placeholders in s.
//
// Supported expressions:
//
//	inputs.<name>           resolved input value (typed → string)
//	target.outputs.<field>  field from the target provider's Outputs
func Resolve(s string, tc *TemplateContext) (string, error) {
	var resolveErr error
	result := tmplRe.ReplaceAllStringFunc(s, func(match string) string {
		if resolveErr != nil {
			return match
		}
		sub := tmplRe.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		expr := strings.TrimSpace(sub[1])
		parts := strings.SplitN(expr, ".", 3)

		switch parts[0] {
		case "inputs":
			if len(parts) < 2 {
				resolveErr = fmt.Errorf("invalid template %q: expected inputs.<name>", match)
				return match
			}
			v, ok := tc.Inputs[parts[1]]
			if !ok {
				resolveErr = fmt.Errorf("template %q: unknown input %q", match, parts[1])
				return match
			}
			return inputToString(v)

		case "target":
			if len(parts) < 3 || parts[1] != "outputs" {
				resolveErr = fmt.Errorf("invalid template %q: expected target.outputs.<field>", match)
				return match
			}
			v, ok := tc.TargetOutputs[parts[2]]
			if !ok {
				resolveErr = fmt.Errorf("template %q: unknown target output %q", match, parts[2])
				return match
			}
			return v

		case "info":
			if len(parts) < 2 {
				resolveErr = fmt.Errorf("invalid template %q: expected info.<name>", match)
				return match
			}
			v, ok := tc.Info[parts[1]]
			if !ok {
				resolveErr = fmt.Errorf("template %q: unknown info key %q", match, parts[1])
				return match
			}
			return v

		case "provider":
			if len(parts) < 2 {
				resolveErr = fmt.Errorf("invalid template %q: expected provider.<key>", match)
				return match
			}
			key := strings.Join(parts[1:], ".")
			v, ok := tc.ProviderOutputs[key]
			if !ok {
				// Fall back to the last dot-separated component of the expression
				// so that providers returning flat keys (e.g. "host") remain
				// compatible with templates written for namespaced keys
				// (e.g. provider.service.db.host → fallback key "host").
				allParts := strings.Split(expr, ".")
				last := allParts[len(allParts)-1]
				v, ok = tc.ProviderOutputs[last]
			}
			if !ok {
				resolveErr = fmt.Errorf("template %q: unknown provider output %q", match, key)
				return match
			}
			return v

		case "fixture":
			if len(parts) < 2 {
				resolveErr = fmt.Errorf("invalid template %q: expected fixture.<name>", match)
				return match
			}
			if tc.Fixture == nil {
				resolveErr = fmt.Errorf("template %q: no fixture context (no fixtures declared in suite)", match)
				return match
			}
			v, ok := tc.Fixture[parts[1]]
			if !ok {
				resolveErr = fmt.Errorf("template %q: unknown fixture %q", match, parts[1])
				return match
			}
			return v

		default:
			resolveErr = fmt.Errorf("template %q: unknown namespace %q", match, parts[0])
			return match
		}
	})
	if resolveErr != nil {
		return "", resolveErr
	}
	return result, nil
}

// ResolveMap resolves templates in every value of a string map.
func ResolveMap(m map[string]string, tc *TemplateContext) (map[string]string, error) {
	if m == nil {
		return nil, nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		resolved, err := Resolve(v, tc)
		if err != nil {
			return nil, fmt.Errorf("key %s: %w", k, err)
		}
		out[k] = resolved
	}
	return out, nil
}

// resolveConfigMap recursively resolves templates in a map[string]any,
// covering nested maps and slices. Non-string leaves are passed through as-is.
func resolveConfigMap(cfg map[string]any, tc *TemplateContext) (map[string]any, error) {
	if cfg == nil {
		return nil, nil
	}
	result, err := resolveAny(cfg, tc)
	if err != nil {
		return nil, err
	}
	return result.(map[string]any), nil
}

func resolveAny(v any, tc *TemplateContext) (any, error) {
	switch val := v.(type) {
	case string:
		return Resolve(val, tc)
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, child := range val {
			resolved, err := resolveAny(child, tc)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			out[k] = resolved
		}
		return out, nil
	case []any:
		out := make([]any, len(val))
		for i, child := range val {
			resolved, err := resolveAny(child, tc)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			out[i] = resolved
		}
		return out, nil
	default:
		return v, nil
	}
}

func inputToString(v any) string {
	switch val := v.(type) {
	case int64:
		return fmt.Sprintf("%d", val)
	case bool:
		return fmt.Sprintf("%t", val)
	case time.Duration:
		return val.String()
	case string:
		return val
	default:
		return fmt.Sprintf("%v", val)
	}
}
