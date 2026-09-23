package engine

// MetricPoint is a single numeric observation from a benchmark run.
// The Family and Labels are Prometheus-style; adapters are responsible
// for naming them according to the benchmark standard (e.g. "tpcc_latency_ms").
type MetricPoint struct {
	Family string            // e.g. "tpcc_latency_ms"
	Labels map[string]string // e.g. {"transaction": "NEW_ORDER", "quantile": "p99"}
	Value  float64
}

// StructuredMetrics is the adapter-produced, typed representation of a benchmark run.
// It is embedded in a Metrics map under StructuredKey so that collectors which
// understand structured data can use it, while collectors that operate on the flat
// map (e.g. stdout) continue to work unchanged.
//
// Adapters own metric naming: the Family strings should reflect the benchmark
// standard (TPC-C, TPC-H, and so on), not the tool that ran it.
type StructuredMetrics struct {
	Points     []MetricPoint
	InfoLabels map[string]string // text metadata: pg_version, extension_version, and so on
}

// StructuredKey is the reserved key in a Metrics map that holds StructuredMetrics.
// It starts with "_" to avoid collision with any real metric names.
const StructuredKey = "_structured"
