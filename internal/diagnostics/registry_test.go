package diagnostics

import "testing"

// TestVendor pins how a run's cloud is recognised: read from the declaration
// its target made, never guessed. The cases below are the reason it has to be a
// declaration.
func TestVendor(t *testing.T) {
	tests := []struct {
		name    string
		outputs map[string]string
		want    string
	}{
		{
			name:    "rds declares aws",
			outputs: map[string]string{"host": "x.rds.amazonaws.com", "dbi_resource_id": "db-ABC123", VendorOutputKey: VendorAWS},
			want:    VendorAWS,
		},
		{
			name:    "cloud sql declares gcp",
			outputs: map[string]string{"host": "10.1.2.3", "instance_name": "bench-pg-01", VendorOutputKey: VendorGCP},
			want:    VendorGCP,
		},
		{
			// Set by the Supabase provider in Go, so it holds for the
			// supabase and orioledb candidates alike even though their
			// scenario `engine` labels differ.
			name:    "supabase declares itself",
			outputs: map[string]string{"project_ref": "abcdef", VendorOutputKey: VendorSupabase},
			want:    VendorSupabase,
		},
		{
			// Self-hosted Postgres on EC2 is not a managed database, but it is
			// still on AWS: its instance and EBS metrics come from the same
			// CloudWatch API as an RDS instance's. Declaring the vendor rather
			// than the service is what lets one collector serve both.
			name:    "self-hosted ec2 postgres declares aws",
			outputs: map[string]string{"host": "10.0.1.5", "public_ip": "3.1.2.3", VendorOutputKey: VendorAWS},
			want:    VendorAWS,
		},
		{
			name:    "docker-compose declares nothing",
			outputs: map[string]string{"host": "localhost", "port": "5432"},
			want:    "",
		},
		{
			name:    "no outputs at all",
			outputs: nil,
			want:    "",
		},
		{
			// Returned as-is so the registry can say "unsupported collector"
			// rather than silently skipping a module that expects support.
			name:    "unknown vendor is passed through",
			outputs: map[string]string{VendorOutputKey: "azure"},
			want:    "azure",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Vendor(tt.outputs); got != tt.want {
				t.Errorf("Vendor(%v) = %q, want %q", tt.outputs, got, tt.want)
			}
		})
	}
}
