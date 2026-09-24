package hostmetrics

import (
	"strings"
	"testing"
	"time"
)

func TestParse_ExtractsRelevantSamplesAndSkipsOthers(t *testing.T) {
	input := strings.Join([]string{
		`{"name":"cpu_seconds_total","namespace":"host","tags":{"collector":"cpu","cpu":"0","host":"h","mode":"idle"},"timestamp":"2026-09-24T12:11:02.540933Z","kind":"absolute","counter":{"value":100.0}}`,
		`{"name":"network_receive_bytes_total","namespace":"host","tags":{"collector":"network","device":"eth0","host":"h"},"timestamp":"2026-09-24T12:11:02.540933Z","kind":"absolute","counter":{"value":1000.0}}`,
		`{"name":"memory_total_bytes","namespace":"host","tags":{"host":"h"},"timestamp":"2026-09-24T12:11:02.540933Z","kind":"absolute","gauge":{"value":123.0}}`,
		`{"name":"vector_component_received_events_total","namespace":"vector","tags":{},"timestamp":"2026-09-24T12:11:02.540933Z","kind":"absolute","counter":{"value":5.0}}`,
		``,
		`not json at all`,
	}, "\n")

	samples, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("len(samples) = %d, want 2 (got %+v)", len(samples), samples)
	}

	if samples[0].Name != "cpu_seconds_total" || samples[0].Value != 100.0 {
		t.Errorf("samples[0] = %+v, want cpu_seconds_total=100.0", samples[0])
	}
	if samples[0].Tags["mode"] != "idle" {
		t.Errorf("samples[0].Tags[mode] = %q, want idle", samples[0].Tags["mode"])
	}
	wantTS, err := time.Parse(time.RFC3339Nano, "2026-09-24T12:11:02.540933Z")
	if err != nil {
		t.Fatalf("parse want timestamp: %v", err)
	}
	if !samples[0].Timestamp.Equal(wantTS) {
		t.Errorf("samples[0].Timestamp = %v, want %v", samples[0].Timestamp, wantTS)
	}

	if samples[1].Name != "network_receive_bytes_total" || samples[1].Value != 1000.0 {
		t.Errorf("samples[1] = %+v, want network_receive_bytes_total=1000.0", samples[1])
	}
}

func TestParseFile_MissingFileErrors(t *testing.T) {
	if _, err := ParseFile("/nonexistent/path/metrics.log"); err == nil {
		t.Error("ParseFile(nonexistent path) = nil error, want an error")
	}
}
