package hostmetrics

import (
	"sort"
	"time"
)

const (
	metricCPUSecondsTotal           = "cpu_seconds_total"
	metricNetworkReceiveBytesTotal  = "network_receive_bytes_total"
	metricNetworkTransmitBytesTotal = "network_transmit_bytes_total"
)

type timeValue struct {
	t time.Time
	v float64
}

// sumByTimestamp aggregates the value of every sample named name (and, if
// keep is non-nil, whose tags satisfy keep) across all its tags, grouped by
// scrape timestamp, and returns the result sorted by time. Grouping by
// timestamp rather than by an individual tag (a cpu core, a network device)
// is what aggregates a multi-core/multi-device host down to one overall
// series.
func sumByTimestamp(samples []Sample, name string, keep func(tags map[string]string) bool) []timeValue {
	sums := make(map[time.Time]float64)
	for _, s := range samples {
		if s.Name != name {
			continue
		}
		if keep != nil && !keep(s.Tags) {
			continue
		}
		sums[s.Timestamp] += s.Value
	}
	out := make([]timeValue, 0, len(sums))
	for t, v := range sums {
		out = append(out, timeValue{t, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].t.Before(out[j].t) })
	return out
}

// deltaRates returns the per-interval rate of change (value per second)
// between each consecutive pair of points. Intervals with non-positive
// duration or a decreasing value (a counter reset) are skipped, since
// host_metrics counters are cumulative and can only be turned into a rate by
// differencing consecutive scrapes.
func deltaRates(points []timeValue) []float64 {
	if len(points) < 2 {
		return nil
	}
	rates := make([]float64, 0, len(points)-1)
	for i := 1; i < len(points); i++ {
		dt := points[i].t.Sub(points[i-1].t).Seconds()
		dv := points[i].v - points[i-1].v
		if dt <= 0 || dv < 0 {
			continue
		}
		rates = append(rates, dv/dt)
	}
	return rates
}

// CPUUtilization returns overall CPU utilization (0-1), aggregated across
// all cores, at each scrape interval, derived from cumulative
// cpu_seconds_total counters (utilization = 1 - Δidle/Δtotal).
func CPUUtilization(samples []Sample) []float64 {
	total := sumByTimestamp(samples, metricCPUSecondsTotal, nil)
	idle := sumByTimestamp(samples, metricCPUSecondsTotal, func(tags map[string]string) bool {
		return tags["mode"] == "idle"
	})
	idleByTS := make(map[time.Time]float64, len(idle))
	for _, tv := range idle {
		idleByTS[tv.t] = tv.v
	}

	var out []float64
	for i := 1; i < len(total); i++ {
		dt := total[i].t.Sub(total[i-1].t).Seconds()
		dTotal := total[i].v - total[i-1].v
		if dt <= 0 || dTotal <= 0 {
			continue
		}
		prevIdle, okPrev := idleByTS[total[i-1].t]
		curIdle, okCur := idleByTS[total[i].t]
		if !okPrev || !okCur {
			continue
		}
		util := 1 - (curIdle-prevIdle)/dTotal
		switch {
		case util < 0:
			util = 0
		case util > 1:
			util = 1
		}
		out = append(out, util)
	}
	return out
}

// NetworkThroughput returns per-interval receive and transmit throughput in
// bytes/sec, aggregated across all network devices.
func NetworkThroughput(samples []Sample) (receiveBps, transmitBps []float64) {
	receiveBps = deltaRates(sumByTimestamp(samples, metricNetworkReceiveBytesTotal, nil))
	transmitBps = deltaRates(sumByTimestamp(samples, metricNetworkTransmitBytesTotal, nil))
	return
}
