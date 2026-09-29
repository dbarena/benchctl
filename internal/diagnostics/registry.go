package diagnostics

// VendorOutputKey is the target output through which a provisioned target
// declares which cloud it runs on.
//
// It is declared, not inferred, because nothing in run state can answer the
// question. `benchctl fetch` never reads the scenario file; target.provider is
// "opentofu" for RDS, Cloud SQL and self-hosted EC2 Postgres alike;
// driver.provider names the cloud the load generator runs in rather than the
// target's (RDS, self-hosted EC2 and Supabase all drive from ec2); and the
// scenario's `engine` label is descriptive free text — the OrioleDB candidate
// labels itself "orioledb" and is a Supabase project.
//
// The value names the vendor rather than the managed service on purpose. It is
// the axis that decides whose APIs and credentials apply, which is what a
// collector is built around, and it stays meaningful for targets that are not
// a managed database at all: the self-hosted EC2 Postgres module declares
// "aws" and its instance metrics come from the same CloudWatch API as an RDS
// instance's. Which of a vendor's services to query is then the collector's
// business, decided from the identifiers it finds in the outputs.
//
// A target that declares nothing has no cloud behind it to query, which is the
// right default for docker-compose and noop.
const VendorOutputKey = "vendor"

// Vendors benchctl can collect provider-side data from, as declared by
// VendorOutputKey, reported by Collector.Name, and recorded in index.json.
const (
	VendorAWS      = "aws"
	VendorGCP      = "gcp"
	VendorSupabase = "supabase"
)

// Vendor returns the cloud a run's target declared, or "" when it declared
// none.
//
// An unrecognised value is returned as-is rather than discarded, so a module
// declaring a vendor benchctl has no collector for produces a clear
// "unsupported diagnostics collector" message instead of silence.
func Vendor(outputs map[string]string) string {
	return outputs[VendorOutputKey]
}
