package hostmetrics

import (
	"math"
	"sort"
)

// percentile returns the q-th percentile of values using the nearest-rank
// method, mirroring the convention already used for RTT reporting in
// internal/engine/info.go. q is clamped to (0, 1]. values is not mutated.
func percentile(values []float64, q float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)

	if q <= 0 {
		return sorted[0]
	}
	if q > 1 {
		q = 1
	}
	rank := int(math.Ceil(q*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	return sorted[rank]
}
