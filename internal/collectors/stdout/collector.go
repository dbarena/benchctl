// Package stdout implements a CollectorProvider that prints benchmark results
// to standard output.
package stdout

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/dbarena/benchctl/internal/engine"
)

// Collector implements engine.CollectorProvider by writing metrics to an
// io.Writer (defaults to os.Stdout).
type Collector struct {
	w io.Writer
}

// New returns a Collector that writes to os.Stdout.
func New() *Collector {
	return &Collector{w: os.Stdout}
}

// newWithWriter returns a Collector that writes to w, for testing.
func newWithWriter(w io.Writer) *Collector {
	return &Collector{w: w}
}

// Collect prints metrics to the writer. StructuredMetrics are rendered with
// grouped, dot-padded lines; all other metrics are printed as key-value pairs.
// "raw_output", "_structured", and any engine.IsRawSamplesCSVKey key are
// excluded from the flat key-value section: the first two are large and
// internal, and each step's raw-samples CSV gets its own dedicated
// raw_samples_*.csv file below (one per step, see writeRawSamplesCSV).
//
// If cfg["file_enabled"] is true, a CSV file named results_<benchmark>_<iteration>.csv
// is written to the current working directory and its absolute path is printed.
func (c *Collector) Collect(_ context.Context, cfg map[string]any, metrics engine.Metrics) error {
	var structured *engine.StructuredMetrics
	if sm, ok := metrics[engine.StructuredKey].(engine.StructuredMetrics); ok {
		structured = &sm
	}

	keys := make([]string, 0, len(metrics))
	var rawSamplesKeys []string
	for k := range metrics {
		if _, ok := engine.IsRawSamplesCSVKey(k); ok {
			rawSamplesKeys = append(rawSamplesKeys, k)
			continue
		}
		if k != "raw_output" && k != engine.StructuredKey {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	sort.Strings(rawSamplesKeys)

	fmt.Fprintln(c.w, "=== Results ===")
	for _, k := range keys {
		fmt.Fprintf(c.w, "  %-30s %s\n", k, formatValue(metrics[k]))
	}
	if structured != nil {
		fmt.Fprint(c.w, formatStructured(structured))
	}

	if cfgBool(cfg, "file_enabled") {
		absPath, err := writeJSON(cfg, keys, metrics, structured)
		if err != nil {
			return fmt.Errorf("stdout collector: write json: %w", err)
		}
		fmt.Fprintf(c.w, "Results also written to %s\n", absPath)

		for _, k := range rawSamplesKeys {
			step, _ := engine.IsRawSamplesCSVKey(k)
			raw, _ := metrics[k].(string)
			if raw == "" {
				continue
			}
			rawPath, err := writeRawSamplesCSV(cfg, step, raw)
			if err != nil {
				return fmt.Errorf("stdout collector: write raw samples csv: %w", err)
			}
			fmt.Fprintf(c.w, "Raw samples also written to %s\n", rawPath)
		}
	}

	return nil
}

// cfgBool returns true when cfg[key] is the boolean true or the string "true".
// Template rendering converts bool inputs to their string representation, so
// both forms must be accepted.
func cfgBool(cfg map[string]any, key string) bool {
	switch v := cfg[key].(type) {
	case bool:
		return v
	case string:
		return v == "true"
	}
	return false
}

// artifactNaming derives the benchmark name, iteration, and sorted fixture
// value parts a per-run artifact filename needs to stay unique across
// iterations and fixture combinations. writeJSON and writeRawSamplesCSV share
// it, so results_*.json and raw_samples_*.csv always agree on how a run's
// artifacts are named.
func artifactNaming(cfg map[string]any) (benchmarkName string, iteration int, fixtureParts []string) {
	if labels, ok := cfg["labels"].(map[string]any); ok {
		benchmarkName, _ = labels["benchmark"].(string)
	}
	if benchmarkName == "" {
		benchmarkName, _ = cfg["scenario"].(string)
	}

	if iter, ok := cfg["iteration"].(int); ok {
		iteration = iter
	}

	// Build fixture suffix from fixture_* labels, sorted for determinism.
	if labels, ok := cfg["labels"].(map[string]any); ok {
		var fkeys []string
		for k := range labels {
			if strings.HasPrefix(k, "fixture_") {
				fkeys = append(fkeys, k)
			}
		}
		sort.Strings(fkeys)
		for _, k := range fkeys {
			fixtureParts = append(fixtureParts, sanitizeFilePart(fmt.Sprintf("%v", labels[k])))
		}
	}
	return benchmarkName, iteration, fixtureParts
}

// artifactFilename builds "<prefix>_<benchmark>_<iteration>[_<fixture_values>].<ext>".
func artifactFilename(prefix, benchmarkName string, iteration int, fixtureParts []string, ext string) string {
	filename := fmt.Sprintf("%s_%s_%d", prefix, sanitizeFilePart(benchmarkName), iteration)
	if len(fixtureParts) > 0 {
		filename += "_" + strings.Join(fixtureParts, "_")
	}
	return filename + "." + ext
}

// writeRawSamplesCSV writes a step's raw per-tick samples verbatim to
// raw_samples_<benchmark>_<iteration>[_<fixture_values>]_<step>.csv in the
// current working directory and returns its absolute path. Suffixing by step
// keeps a warm-up step's samples separate from a subsequent measured step's.
func writeRawSamplesCSV(cfg map[string]any, step, csvContent string) (string, error) {
	benchmarkName, iteration, fixtureParts := artifactNaming(cfg)
	parts := append(fixtureParts, sanitizeFilePart(step))
	filename := artifactFilename("raw_samples", benchmarkName, iteration, parts, "csv")
	if err := os.WriteFile(filename, []byte(csvContent), 0o644); err != nil {
		return "", err
	}
	return filepath.Abs(filename)
}

// writeJSON creates results_<benchmark>_<iteration>[_<fixture_values>].json in
// the current working directory and returns its absolute path. Fixture values
// (injected as "fixture_*" labels) are appended to the filename so it stays
// unique across all fixture combinations.
func writeJSON(cfg map[string]any, sortedKeys []string, metrics engine.Metrics, structured *engine.StructuredMetrics) (string, error) {
	benchmarkName, iteration, fixtureParts := artifactNaming(cfg)
	cfgLabels, _ := cfg["labels"].(map[string]any)

	var entries []map[string]any
	for _, k := range sortedKeys {
		entry := map[string]any{
			"benchmark": benchmarkName,
			"iteration": iteration,
			"name":      k,
			"value":     metrics[k],
		}
		for lk, lv := range cfgLabels {
			if _, exists := entry[lk]; !exists {
				entry[lk] = lv
			}
		}
		entries = append(entries, entry)
	}
	if structured != nil {
		for _, p := range structured.Points {
			entry := map[string]any{
				"benchmark": benchmarkName,
				"iteration": iteration,
				"name":      p.Family,
				"value":     p.Value,
			}
			for lk, lv := range cfgLabels {
				if _, exists := entry[lk]; !exists {
					entry[lk] = lv
				}
			}
			for k, v := range p.Labels {
				entry[k] = v
			}
			entries = append(entries, entry)
		}
	}

	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')

	filename := artifactFilename("results", benchmarkName, iteration, fixtureParts, "json")
	if err := os.WriteFile(filename, data, 0o644); err != nil {
		return "", err
	}

	absPath, err := filepath.Abs(filename)
	if err != nil {
		return "", err
	}
	return absPath, nil
}

// sanitizeFilePart replaces characters that are unsafe in filenames with underscores.
func sanitizeFilePart(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
			sb.WriteRune(r)
		} else {
			sb.WriteRune('_')
		}
	}
	return sb.String()
}

// formatStructured renders StructuredMetrics as human-readable dot-padded lines.
//
// Points are grouped by (Family, non-stat labels). The stat dimension is the
// value of the "stat" label (k6 convention) or the "quantile" label (TPCC
// convention); it becomes the column headers within a group. Families with a
// single group render on one line; families with multiple groups (e.g. one per
// transaction) render with the family name as a header and each group indented.
func formatStructured(sm *engine.StructuredMetrics) string {
	type groupRow struct {
		family   string
		groupKey string // "{k=v,...}" of non-stat labels; empty when no such labels
		stats    map[string]float64
		scalar   float64
		hasStats bool
	}

	groupMap := make(map[string]*groupRow) // key: family+"|"+groupKey
	var rowOrder []string

	familyRows := make(map[string][]string) // family → row keys, insertion order
	var familyOrder []string

	for _, p := range sm.Points {
		// Identify the stat dimension label (the sub-column within a group).
		statLabelName := ""
		statLabelVal := ""
		if v := p.Labels["stat"]; v != "" {
			statLabelName, statLabelVal = "stat", v
		} else if v := p.Labels["quantile"]; v != "" {
			statLabelName, statLabelVal = "quantile", v
		}

		nonStatKeys := make([]string, 0, len(p.Labels))
		for k := range p.Labels {
			if k != statLabelName {
				nonStatKeys = append(nonStatKeys, k)
			}
		}
		sort.Strings(nonStatKeys)
		var gkParts []string
		for _, k := range nonStatKeys {
			gkParts = append(gkParts, k+"="+p.Labels[k])
		}
		groupKey := ""
		if len(gkParts) > 0 {
			groupKey = "{" + strings.Join(gkParts, ",") + "}"
		}

		rowKey := p.Family + "|" + groupKey
		if _, exists := groupMap[rowKey]; !exists {
			groupMap[rowKey] = &groupRow{
				family:   p.Family,
				groupKey: groupKey,
				stats:    make(map[string]float64),
			}
			rowOrder = append(rowOrder, rowKey)
			if _, famExists := familyRows[p.Family]; !famExists {
				familyOrder = append(familyOrder, p.Family)
			}
			familyRows[p.Family] = append(familyRows[p.Family], rowKey)
		}
		row := groupMap[rowKey]
		if statLabelVal != "" {
			row.stats[statLabelVal] = p.Value
			row.hasStats = true
		} else {
			row.scalar = p.Value
		}
	}
	sort.Strings(familyOrder)

	// Compute max display widths for dot-padding across both layout modes.
	maxSingleWidth := 0 // family+groupKey for single-group families
	maxSubWidth := 0    // groupKey for sub-rows in multi-group families
	for _, family := range familyOrder {
		rows := familyRows[family]
		if len(rows) == 1 {
			w := len(family) + len(groupMap[rows[0]].groupKey)
			if w > maxSingleWidth {
				maxSingleWidth = w
			}
		} else {
			for _, rk := range rows {
				if w := len(groupMap[rk].groupKey); w > maxSubWidth {
					maxSubWidth = w
				}
			}
		}
	}

	renderValue := func(row *groupRow) string {
		if !row.hasStats {
			return fmtNum(row.scalar)
		}
		statKeys := make([]string, 0, len(row.stats))
		for k := range row.stats {
			statKeys = append(statKeys, k)
		}
		sort.Strings(statKeys)
		parts := make([]string, 0, len(statKeys))
		for _, k := range statKeys {
			parts = append(parts, fmt.Sprintf("%s=%s", k, fmtNum(row.stats[k])))
		}
		return strings.Join(parts, "  ")
	}

	var sb strings.Builder
	for _, family := range familyOrder {
		rows := familyRows[family]
		sort.Slice(rows, func(i, j int) bool {
			return groupMap[rows[i]].groupKey < groupMap[rows[j]].groupKey
		})

		if len(rows) == 1 {
			row := groupMap[rows[0]]
			displayName := family + row.groupKey
			dots := strings.Repeat(".", maxSingleWidth-len(displayName)+3)
			sb.WriteString(fmt.Sprintf("  %s%s %s\n", displayName, dots, renderValue(row)))
		} else {
			sb.WriteString(fmt.Sprintf("  %s\n", family))
			for _, rk := range rows {
				row := groupMap[rk]
				dots := strings.Repeat(".", maxSubWidth-len(row.groupKey)+3)
				sb.WriteString(fmt.Sprintf("    %s%s %s\n", row.groupKey, dots, renderValue(row)))
			}
		}
	}

	if len(sm.InfoLabels) > 0 {
		sb.WriteString("\n")
		infoKeys := make([]string, 0, len(sm.InfoLabels))
		for k := range sm.InfoLabels {
			infoKeys = append(infoKeys, k)
		}
		sort.Strings(infoKeys)
		for _, k := range infoKeys {
			sb.WriteString(fmt.Sprintf("  %s: %s\n", k, sm.InfoLabels[k]))
		}
	}

	return sb.String()
}

// fmtNum formats a float as an integer when it has no fractional part,
// otherwise with two decimal places.
func fmtNum(f float64) string {
	if f == math.Trunc(f) {
		return strconv.FormatFloat(f, 'f', 0, 64)
	}
	return strconv.FormatFloat(f, 'f', 2, 64)
}

// formatValue formats a flat metric value for display.
func formatValue(v any) string {
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return fmt.Sprintf("%v", v)
}
