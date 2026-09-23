package engine

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"
)

func fullConnOutputs() Outputs {
	return Outputs{
		"host":     "db.example.com",
		"port":     "5433",
		"user":     "alice",
		"password": "secret",
		"db":       "mydb",
	}
}

func TestBuildConnString_SslmodeDefaultsToDisable(t *testing.T) {
	connStr, err := buildConnString(fullConnOutputs())
	if err != nil {
		t.Fatalf("buildConnString: %v", err)
	}
	u, err := url.Parse(connStr)
	if err != nil {
		t.Fatalf("parse conn string: %v", err)
	}
	if got := u.Query().Get("sslmode"); got != "disable" {
		t.Errorf("sslmode = %q, want disable", got)
	}
	if u.Query().Has("sslnegotiation") {
		t.Errorf("sslnegotiation should be absent, got %q", u.Query().Get("sslnegotiation"))
	}
}

func TestBuildConnString_SslmodeAndNegotiationFromOutputs(t *testing.T) {
	outputs := fullConnOutputs()
	outputs["sslmode"] = "require"
	outputs["sslnegotiation"] = "direct"

	connStr, err := buildConnString(outputs)
	if err != nil {
		t.Fatalf("buildConnString: %v", err)
	}
	u, err := url.Parse(connStr)
	if err != nil {
		t.Fatalf("parse conn string: %v", err)
	}
	if got := u.Query().Get("sslmode"); got != "require" {
		t.Errorf("sslmode = %q, want require", got)
	}
	if got := u.Query().Get("sslnegotiation"); got != "direct" {
		t.Errorf("sslnegotiation = %q, want direct", got)
	}
}

func TestBuildConnString_MissingConnectionParamsErrors(t *testing.T) {
	for _, k := range []string{"host", "port", "user", "password", "db"} {
		outputs := fullConnOutputs()
		delete(outputs, k)
		if _, err := buildConnString(outputs); err == nil {
			t.Errorf("expected error for missing %s", k)
		}
	}
}

func TestFormatRTT_ReportsMicrosecondsNotRoundedMillis(t *testing.T) {
	// Sub-millisecond samples must survive: the whole point of the probe is
	// resolving differences the old integer-millisecond check could not.
	samples := []time.Duration{
		120 * time.Microsecond,
		140 * time.Microsecond,
		135 * time.Microsecond,
		310 * time.Microsecond,
	}
	got := formatRTT(samples)
	want := "min_us=120, median_us=135, p99_us=310, max_us=310, samples=4"
	if got != want {
		t.Errorf("formatRTT = %q, want %q", got, want)
	}
}

func TestFormatRTT_DoesNotMutateInput(t *testing.T) {
	samples := []time.Duration{300 * time.Microsecond, 100 * time.Microsecond}
	formatRTT(samples)
	if samples[0] != 300*time.Microsecond {
		t.Errorf("input reordered: samples[0] = %v, want 300µs", samples[0])
	}
}

func TestFormatRTT_NoSamples(t *testing.T) {
	if got := formatRTT(nil); got != "samples=0" {
		t.Errorf("formatRTT(nil) = %q, want samples=0", got)
	}
}

func TestPercentile_NearestRank(t *testing.T) {
	sorted := []time.Duration{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	for _, tc := range []struct {
		q    float64
		want time.Duration
	}{
		{0.0, 1},
		{0.1, 1},
		{0.5, 5},
		{0.99, 10},
		{1.0, 10},
		{1.5, 10},
	} {
		if got := percentile(sorted, tc.q); got != tc.want {
			t.Errorf("percentile(q=%v) = %v, want %v", tc.q, got, tc.want)
		}
	}
}

func TestCollectMetadata_UnknownCommandErrors(t *testing.T) {
	_, err := collectMetadata(context.Background(), nil, fullConnOutputs(), "traceroute", "probe", "")
	if err == nil {
		t.Fatal("expected an error for an unknown metadata command")
	}
	if !strings.Contains(err.Error(), "traceroute") {
		t.Errorf("error should name the bad command, got: %v", err)
	}
}
