package victoriametrics

import (
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/dbarena/benchctl/internal/engine"
)

// toPrometheusLines reads engine.StructuredMetrics from the metrics map and
// returns Prometheus text exposition lines ready to POST to VictoriaMetrics.
//
// baseLabels (run_id, scenario, user-defined) are merged onto every line.
// infoLabels collects text metadata (pg_version, …) for the benchctl_run_info gauge.
// If no StructuredMetrics is present the returned slices are empty.
func toPrometheusLines(metrics engine.Metrics, baseLabels map[string]string, tsMs int64) (lines []string, infoLabels map[string]string) {
	infoLabels = make(map[string]string)

	sm, ok := metrics[engine.StructuredKey].(engine.StructuredMetrics)
	if !ok {
		return nil, infoLabels
	}

	maps.Copy(infoLabels, sm.InfoLabels)

	for _, pt := range sm.Points {
		lbls := make(map[string]string, len(baseLabels)+len(pt.Labels))
		maps.Copy(lbls, baseLabels)
		maps.Copy(lbls, pt.Labels)
		lines = append(lines, formatLine(pt.Family, lbls, pt.Value, tsMs))
	}

	return lines, infoLabels
}

// buildInfoMetric produces the benchctl_run_info gauge line that carries all
// text metadata (pg_version, extension_version, …) as label values.
func buildInfoMetric(baseLabels, infoLabels map[string]string, tsMs int64) string {
	lbls := make(map[string]string, len(baseLabels)+len(infoLabels))
	maps.Copy(lbls, baseLabels)
	maps.Copy(lbls, infoLabels)
	return formatLine("benchctl_run_info", lbls, 1.0, tsMs)
}

// formatLine builds a single Prometheus text exposition line with a millisecond timestamp.
func formatLine(name string, labels map[string]string, value float64, tsMs int64) string {
	if len(labels) == 0 {
		return fmt.Sprintf("%s %g %d", name, value, tsMs)
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+`="`+escapeLabel(labels[k])+`"`)
	}
	return fmt.Sprintf("%s{%s} %g %d", name, strings.Join(parts, ","), value, tsMs)
}

// escapeLabel escapes backslash, double-quote, and newline in Prometheus label values.
func escapeLabel(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}
