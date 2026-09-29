output "host" {
  description = "Cloud SQL IP address. Private/VPC-only."
  value       = google_sql_database_instance.this.private_ip_address
}

output "port" {
  description = "PostgreSQL port. Cloud SQL always uses 5432 for Postgres; there is no per-instance port setting to read back."
  value       = 5432
}

output "db" {
  description = "Database name created for benchmarking."
  value       = google_sql_database.tpcc.name
}

output "user" {
  description = "Database username for the benchmark."
  value       = google_sql_user.bench.name
}

output "password" {
  description = "Database user's password."
  value       = var.pg_password
  # Sensitive only hides this from plan/apply CLI output but this is a throwaway benchmark-credential.
  sensitive   = true
}

output "network" {
  description = "Self-link of the VPC the Cloud SQL instance is peered with. Pass to the GCE loaddriver module so it attaches to the same network."
  value       = local.network_self_link
}

output "subnetwork" {
  description = "Self-link of the subnetwork in `zone`'s region. Pass to the GCE loaddriver module for same-region placement."
  value       = local.subnetwork_self_link
}

output "zone" {
  description = "Zone the Cloud SQL instance is pinned to (settings.location_preference.zone). Pass to the GCE loaddriver module so both land in the same zone -- the co-location handoff equivalent of RDS's subnet_id output."
  value       = var.zone
}

output "instance_name" {
  description = "Cloud SQL instance name, e.g. for `gcloud logging read` filtered to this instance's resource."
  value       = google_sql_database_instance.this.name
}

output "sslmode" {
  description = "Forces TLS on all connections."
  value       = "require"
}

output "project_id" {
  description = "GCP project the instance lives in. Used e.g. for fetching diagnostics."
  value       = google_sql_database_instance.this.project
}

output "vendor" {
  description = "Vendor this target runs on. Used e.g. for fetching diagnostics."
  value       = "gcp"
}
