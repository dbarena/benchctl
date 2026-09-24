package hostmetrics

import "testing"

func TestPercentile_NearestRank(t *testing.T) {
	values := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	tests := []struct {
		q    float64
		want float64
	}{
		{0, 1},
		{0.5, 5},
		{0.99, 10},
		{1.0, 10},
	}
	for _, tt := range tests {
		if got := percentile(values, tt.q); got != tt.want {
			t.Errorf("percentile(values, %v) = %v, want %v", tt.q, got, tt.want)
		}
	}
}

func TestPercentile_EmptyReturnsZero(t *testing.T) {
	if got := percentile(nil, 0.99); got != 0 {
		t.Errorf("percentile(nil, 0.99) = %v, want 0", got)
	}
}

func TestPercentile_DoesNotMutateInput(t *testing.T) {
	values := []float64{3, 1, 2}
	percentile(values, 0.5)
	if values[0] != 3 || values[1] != 1 || values[2] != 2 {
		t.Errorf("percentile mutated input: %v", values)
	}
}
