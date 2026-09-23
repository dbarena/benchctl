package engine

import (
	"testing"
	"time"
)

func TestResolveEffectiveDate(t *testing.T) {
	tests := []struct {
		input string
		check func(t *testing.T, got string)
	}{
		{
			input: "",
			check: func(t *testing.T, got string) {
				if got != "" {
					t.Errorf("empty input: want %q, got %q", "", got)
				}
			},
		},
		{
			input: "20260427",
			check: func(t *testing.T, got string) {
				if got != "2026-04-27 00:00:00" {
					t.Errorf("YYYYMMDD input: want %q, got %q", "2026-04-27 00:00:00", got)
				}
			},
		},
		{
			input: "auto",
			check: func(t *testing.T, got string) {
				now := time.Now().UTC()
				want := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Format("2006-01-02 15:04:05")
				if got != want {
					t.Errorf("auto input: want %q, got %q", want, got)
				}
			},
		},
		{
			input: "notadate",
			check: func(t *testing.T, got string) {
				if got != "notadate" {
					t.Errorf("unrecognised input: want passthrough %q, got %q", "notadate", got)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			tc.check(t, resolveEffectiveDate(tc.input))
		})
	}
}
