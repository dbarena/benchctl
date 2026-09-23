// Package tags builds the tags map that benchctl injects into every
// provisioned cloud resource.
package tags

import "github.com/dbarena/benchctl/internal/config"

// Inject merges cfg.Tags into vars["tags"] (creating it if absent), then
// forces created-by=benchctl and run-id=runID last, so neither the fixed
// created-by value nor the run's own run-id can be overridden by an entry in
// the user's local config. vars is returned for chaining.
func Inject(vars map[string]any, runID string, cfg *config.Config) map[string]any {
	if vars == nil {
		vars = make(map[string]any)
	}
	tags, _ := vars["tags"].(map[string]any)
	if tags == nil {
		tags = make(map[string]any, len(cfg.Tags)+2)
	}
	for k, v := range cfg.Tags {
		tags[k] = v
	}
	tags["created-by"] = "benchctl"
	tags["run-id"] = runID
	vars["tags"] = tags
	return vars
}
