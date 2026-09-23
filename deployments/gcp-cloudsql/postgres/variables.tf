variable "project_id" {
  description = "GCP project id. Empty (default) lets the google provider resolve it from the GOOGLE_CLOUD_PROJECT env var (or gcloud's own config) -- the same convention the aws provider follows for RDS (region/credentials come from the shell, never hardcoded in Go or Terraform). Only set this to override that resolution, e.g. for a one-off run against a different project."
  type        = string
  default     = ""
}

variable "wait_for_peering_propagation" {
  description = "Whether to wait 180s after creating the shared VPC peering connection for route propagation across Google's network fabric. Always set explicitly by benchctl's Go orchestration layer: false when the peering connection was adopted from a prior run (already propagated), true when it had to be created fresh this run. Default true only so `tofu validate`/manual applies don't require it."
  type        = bool
  default     = true
}

variable "db_instance_type" {
  # Explicit N4-series custom machine type (db-custom-N4-<vCPU>-<RAM_MB>)
  #
  # Note that N4's minimum memory ratio is 2 GB/vCPU (vs. the bare/legacy
  # family's 0.9 GB/vCPU ratio with a 3.75 GB floor), which
  # puts N4's floor at 2vCPU x 2GB/vCPU = 4GB.
  description = "Cloud SQL machine type, e.g. db-custom-N4-2-4096."
  type        = string
  default     = "db-custom-N4-2-4096"
}

variable "engine_version" {
  # Unlike other providers Cloud SQL's API only accepts a major-version enum
  # and GCP manages minor/patch versions itself. Valid values are documented as the
  # SqlDatabaseVersion enum: https://cloud.google.com/sql/docs/postgres/db-versions
  # or retrievable via `gcloud sql instances create --help` (see the
  # --database-version flag's accepted values, which is the authoritative source)
  description = "Cloud SQL Postgres major version enum, e.g. POSTGRES_17."
  type        = string
  default     = "POSTGRES_17"
}

variable "region" {
  description = "GCP region to deploy into."
  type        = string
  default     = "us-central1"
}

variable "zone" {
  description = "GCP zone to pin the Cloud SQL instance to, via settings.location_preference.zone. Cloud SQL has no placement-group equivalent, so zone pinning plus co-locating the GCE driver in the same zone/network is the closest available proximity guarantee."
  type        = string
  default     = "us-central1-a"
}

variable "pg_user" {
  description = "Application username created via google_sql_user, analogous to RDS's master pg_user."
  type        = string
  default     = "bench"
}

variable "pg_password" {
  description = "Password for both pg_user and the built-in postgres superuser (set via root_password, since Cloud SQL Postgres always creates that account at instance creation -- an explicit password keeps it in Terraform state instead of Google generating one we can't retrieve)."
  type        = string
  default     = "benchbench"
  sensitive   = true

  validation {
    condition     = length(var.pg_password) >= 8
    error_message = "pg_password must be at least 8 characters."
  }
}

variable "disk_type" {
  description = "Cloud SQL disk type. Only \"HYPERDISK_BALANCED\" is valid here as it is required by the N4 machine series."
  type        = string
  default     = "HYPERDISK_BALANCED"
}

variable "disk_size_gb" {
  description = "Disk size (GB). Disk performance is provisioned independently via disk_provisioned_iops/disk_provisioned_throughput_mibps below (HYPERDISK_BALANCED decouples IOPS/throughput from capacity); this only changes capacity."
  type        = number
}

variable "disk_provisioned_iops" {
  description = "HYPERDISK_BALANCED provisioned IOPS. Cloud SQL's HYPERDISK_BALANCED disk prices and provisions IOPS as an independent input rather than deriving it from disk size the way PD_SSD did."
  type        = number
}

variable "disk_provisioned_throughput_mibps" {
  description = "HYPERDISK_BALANCED provisioned throughput (MiB/s)."
  type        = number
}

variable "backup_retention_period" {
  description = "Days of automated backups / PITR log retention (transaction_log_retention_days, 1-7)."
  type        = number
  default     = 7
}

variable "tags" {
  description = "Labels applied to the Cloud SQL instance (settings.user_labels) and driver VM. Named `tags` rather than GCP's own `labels` term purely for symmetry with the RDS/EC2 scenario vars. Do not confuse with GCP \"network tags\" (a separate, unrelated string-list mechanism used for firewall targeting)."
  type        = map(string)
  default     = {}
}

variable "vector_sink_victoriametrics_endpoint" {
  description = "VictoriaMetrics base URL. Injected automatically from BENCHCTL_METRICS_ENDPOINT by benchctl. Unused by this module (no Vector agent runs on a managed database instance) — declared only to accept the value without an 'undeclared variable' warning."
  type        = string
  default     = ""
}

variable "vector_sink_victoriametrics_token" {
  description = "Bearer token for VictoriaMetrics remote write. Injected automatically from BENCHCTL_METRICS_TOKEN by benchctl. Unused by this module (no Vector agent runs on a managed database instance) — declared only to accept the value without an 'undeclared variable' warning."
  type        = string
  default     = ""
  sensitive   = true
}
