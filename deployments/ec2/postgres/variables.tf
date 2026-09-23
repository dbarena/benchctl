variable "instance_type" {
  description = "EC2 instance type for the Postgres target. Must have a local NVMe instance store (c*d / r*d / m*d family). Default matches the scenario YAML."
  type        = string
  default     = "c8gd.xlarge"
}

variable "tags" {
  description = "Tags applied to all AWS resources."
  type        = map(string)
  default     = {}
}

variable "vector_enabled" {
  description = "Whether to install and start the Vector metrics agent on boot. When false, Vector is not installed at all."
  type        = bool
  default     = true
}

variable "vector_sink" {
  description = "Metrics sink backend for Vector. 'victoriametrics' sends via prometheus_remote_write (requires vector_sink_victoriametrics_endpoint); any other value writes JSON to a local file."
  type        = string
  default     = "stdout"
}

variable "vector_sink_victoriametrics_endpoint" {
  description = "VictoriaMetrics base URL. Injected automatically from BENCHCTL_METRICS_ENDPOINT by benchctl. When empty while vector_sink='victoriametrics', Vector falls back to the file sink."
  type        = string
  default     = ""
}

variable "vector_sink_victoriametrics_token" {
  description = "Bearer token for VictoriaMetrics remote write. Injected automatically from BENCHCTL_METRICS_TOKEN by benchctl."
  type        = string
  default     = ""
  sensitive   = true
}

variable "vector_host_metrics_scrape_interval_secs" {
  description = "Scrape interval for host (system) metrics in seconds."
  type        = number
  default     = 5
}

variable "vector_postgres_metrics_scrape_interval_secs" {
  description = "Scrape interval for PostgreSQL metrics in seconds."
  type        = number
  default     = 15
}

variable "pg_user" {
  description = "PostgreSQL username. Used both as POSTGRES_USER for the database container and to build the Vector postgresql_metrics connection string."
  type        = string
  default     = "bench"
}

variable "pg_password" {
  description = "PostgreSQL password. Used both as POSTGRES_PASSWORD for the database container and to build the Vector postgresql_metrics connection string."
  type        = string
  default     = "bench"
  sensitive   = true
}

variable "region" {
  description = "AWS region to deploy into."
  type        = string
  default     = "eu-central-1"
}

variable "ami" {
  # Pinned per region to Ubuntu 24.04 LTS arm64 hvm:ebs-gp3.
  # eu-central-1, build 20260904
  # us-east-1, build 20260904
  # Update via the update-ami workflow or query manually:
  #   aws ec2 describe-images --region <region> --owners 099720109477 \
  #     --filters "Name=name,Values=ubuntu/images/hvm-ssd*/ubuntu-noble-24.04-arm64-server-*" \
  #               "Name=virtualization-type,Values=hvm" "Name=architecture,Values=arm64" \
  #     --query 'sort_by(Images,&CreationDate)[-1].ImageId' --output text
  description = "AMI ID per region. Pinned to a specific Ubuntu 24.04 LTS ARM64 build; update via the update-ami workflow."
  type        = map(string)
  default = {
    "eu-central-1" = "ami-0e79e661e73ddfac9"
    "us-east-1" = "ami-0246d714afcc1d494"
  }
}

