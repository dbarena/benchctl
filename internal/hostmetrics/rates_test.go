package hostmetrics

import (
	"testing"
	"time"
)

func sample(name string, tags map[string]string, ts time.Time, value float64) Sample {
	return Sample{Name: name, Tags: tags, Timestamp: ts, Value: value}
}

func TestCPUUtilization_TwoIntervals(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(2 * time.Second)
	t2 := t0.Add(4 * time.Second)

	// Single core. [t0,t1]: idle +1s, user +1s over 2s elapsed -> 50% util.
	// [t1,t2]: idle +0s, user +2s over 2s elapsed -> 100% util.
	samples := []Sample{
		sample(metricCPUSecondsTotal, map[string]string{"cpu": "0", "mode": "idle"}, t0, 0),
		sample(metricCPUSecondsTotal, map[string]string{"cpu": "0", "mode": "user"}, t0, 0),
		sample(metricCPUSecondsTotal, map[string]string{"cpu": "0", "mode": "idle"}, t1, 1),
		sample(metricCPUSecondsTotal, map[string]string{"cpu": "0", "mode": "user"}, t1, 1),
		sample(metricCPUSecondsTotal, map[string]string{"cpu": "0", "mode": "idle"}, t2, 1),
		sample(metricCPUSecondsTotal, map[string]string{"cpu": "0", "mode": "user"}, t2, 3),
	}

	got := CPUUtilization(samples)
	want := []float64{0.5, 1.0}
	if len(got) != len(want) {
		t.Fatalf("CPUUtilization = %v, want %v", got, want)
	}
	for i := range want {
		if diff := got[i] - want[i]; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("CPUUtilization[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestNetworkThroughput_SumsAcrossDevices(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(1 * time.Second)

	samples := []Sample{
		sample(metricNetworkReceiveBytesTotal, map[string]string{"device": "eth0"}, t0, 0),
		sample(metricNetworkReceiveBytesTotal, map[string]string{"device": "lo0"}, t0, 0),
		sample(metricNetworkReceiveBytesTotal, map[string]string{"device": "eth0"}, t1, 100),
		sample(metricNetworkReceiveBytesTotal, map[string]string{"device": "lo0"}, t1, 50),
		sample(metricNetworkTransmitBytesTotal, map[string]string{"device": "eth0"}, t0, 0),
		sample(metricNetworkTransmitBytesTotal, map[string]string{"device": "eth0"}, t1, 10),
	}

	rx, tx := NetworkThroughput(samples)
	if len(rx) != 1 || rx[0] != 150 {
		t.Errorf("rx = %v, want [150]", rx)
	}
	if len(tx) != 1 || tx[0] != 10 {
		t.Errorf("tx = %v, want [10]", tx)
	}
}

func TestDeltaRates_SkipsZeroDurationAndCounterReset(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	points := []timeValue{
		{t0, 100},
		{t0, 100},                 // zero-duration interval vs previous: skipped
		{t0.Add(time.Second), 50}, // decreasing value (reset) vs previous: skipped
		{t0.Add(2 * time.Second), 80},
	}

	got := deltaRates(points)
	want := []float64{30}
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("deltaRates = %v, want %v", got, want)
	}
}

func TestDeltaRates_FewerThanTwoPointsReturnsNil(t *testing.T) {
	if got := deltaRates(nil); got != nil {
		t.Errorf("deltaRates(nil) = %v, want nil", got)
	}
	if got := deltaRates([]timeValue{{time.Now(), 1}}); got != nil {
		t.Errorf("deltaRates(one point) = %v, want nil", got)
	}
}
