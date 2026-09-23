variable "db_instance_class" {
  description = "RDS DB instance class, e.g. \"db.t4g.small\" or \"db.m9g.xlarge\"."
  type        = string
  default     = "db.t4g.small"
}

// run the following command to find the most recent version
//
// aws rds describe-db-engine-versions --engine postgres --region us-east-1 --query "DBEngineVersions[?starts_with(EngineVersion,'17')].EngineVersion" --no-cli-pager
variable "engine_version" {
  description = "PostgreSQL engine version."
  type        = string
  default     = "17.11"
}

variable "region" {
  description = "AWS region to deploy into."
  type        = string
  default     = "us-east-1"
}

variable "pg_user" {
  description = "Master username for the RDS instance."
  type        = string
  default     = "bench"
}

variable "pg_password" {
  description = "Master password for the RDS instance."
  type        = string
  // restrictions: 8-128 printable ASCII characters, excluding '/', '\"', '@', and spaces
  default     = "benchbench"
  sensitive   = true

  validation {
    condition     = length(var.pg_password) >= 8 && length(var.pg_password) <= 128
    error_message = "pg_password must be 8-128 characters (RDS master password requirement)."
  }
  validation {
    condition     = !can(regex("[/\"@ ]", var.pg_password))
    error_message = "pg_password must not contain '/', '\"', '@', or spaces (RDS master password requirement)."
  }
}

variable "allocated_storage_gb" {
  description = "gp3 allocated storage (GB). RDS Postgres on gp3 rejects a custom `iops` value entirely below 400 GiB allocated storage (see main.tf's min_storage_gb_for_custom_iops). Set this to >= 400 to make `iops` take effect."
  type        = number
  default     = 20
}

variable "iops" {
  description = "gp3 provisioned IOPS. Only takes effect if allocated_storage_gb is >= 400 GB. Below that threshold, AWS rejects the `iops` argument and it's omitted regardless of this value (see main.tf's min_storage_gb_for_custom_iops)."
  type        = number
  default     = 3000
}

variable "disk_throughput_mibps" {
  description = "gp3 provisioned throughput (MB/s), i.e. the aws_db_instance `storage_throughput` argument. 0 (default) omits the argument, letting AWS apply gp3's own free baseline for the given size/iops Like `iops`, only takes effect if allocated_storage_gb is >= 400 GB."
  type        = number
  default     = 0
}

variable "backup_retention_period" {
  description = "Days automated backups are retained."
  type        = number
  default     = 7
}

variable "tags" {
  description = "Tags applied to all AWS resources."
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
