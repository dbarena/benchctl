package gce

import "testing"

func TestArchFromInstanceType(t *testing.T) {
	tests := []struct {
		instanceType string
		want         string
	}{
		{"c4a-standard-4", "arm64"},
		{"t2a-standard-4", "arm64"},
		{"c4-standard-4", "amd64"},
		{"n2-standard-4", "amd64"},
		{"n4-standard-4", "amd64"},
		{"c3-standard-4", "amd64"},
	}

	for _, tt := range tests {
		t.Run(tt.instanceType, func(t *testing.T) {
			if got := archFromInstanceType(tt.instanceType); got != tt.want {
				t.Errorf("archFromInstanceType(%q) = %q, want %q", tt.instanceType, got, tt.want)
			}
		})
	}
}
