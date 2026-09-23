package runstate

import "testing"

func TestValidateRunID(t *testing.T) {
	cases := []struct {
		id      string
		wantErr bool
	}{
		{"postgres-tpcc-20260817-101500-ab12", false},
		{"simple", false},
		{"UPPER_lower.123-mixed", false},
		{"", true},
		{".", true},
		{"..", true},
		{"../escape", true},
		{"nested/path", true},
		{"leading/../traversal", true},
		{"has spaces", true},
	}
	for _, tc := range cases {
		err := ValidateRunID(tc.id)
		if (err != nil) != tc.wantErr {
			t.Errorf("ValidateRunID(%q) error = %v, wantErr %v", tc.id, err, tc.wantErr)
		}
	}
}
