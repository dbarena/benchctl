variable "vector_enabled" {
  description = "Whether to install and start the Vector metrics agent on boot. When false, Vector is not installed at all."
  type        = bool
  default     = true
}

variable "vector_sink" {
  description = "Metrics sink backend for Vector. 'victoriametrics' sends via prometheus_remote_write; any other value writes JSON to a local file."
  type        = string
  default     = "stdout"
}

variable "vector_sink_victoriametrics_endpoint" {
  description = "VictoriaMetrics base URL. Injected automatically from BENCHCTL_METRICS_ENDPOINT by benchctl."
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

variable "instance_type" {
  description = "EC2 instance type for the load generator. Default matches the scenario YAML."
  type        = string
  default     = "c8gd.xlarge"
}

variable "subnet_id" {
  description = "Subnet ID where the driver will be placed. Pass target.outputs.subnet_id to co-locate with the postgres module. When null, the module creates its own VPC and subnet."
  type        = string
  default     = null
}

variable "placement_group_name" {
  description = "Name of the cluster placement group. Pass target.outputs.placement_group_name to share hardware with the target. When null, the module creates its own placement group."
  type        = string
  default     = null
}

variable "region" {
  description = "AWS region to deploy into."
  type        = string
  default     = "eu-central-1"
}

variable "ami" {
  # Pinned per region to Ubuntu 24.04 LTS arm64 hvm:ebs-gp3.
  # eu-central-1, build 20261004
  # us-east-1, build 20261004
  # Update via the update-ami workflow or query manually:
  #   aws ec2 describe-images --region <region> --owners 099720109477 \
  #     --filters "Name=name,Values=ubuntu/images/hvm-ssd*/ubuntu-noble-24.04-arm64-server-*" \
  #               "Name=virtualization-type,Values=hvm" "Name=architecture,Values=arm64" \
  #     --query 'sort_by(Images,&CreationDate)[-1].ImageId' --output text
  description = "AMI ID per region. Pinned to a specific Ubuntu 24.04 LTS ARM64 build; update via the update-ami workflow."
  type        = map(string)
  default = {
    "eu-central-1" = "ami-0acc733af7bf42eeb"
    "us-east-1" = "ami-0e1ab5c876cc030e8"
  }
}

variable "tags" {
  description = "Tags applied to all AWS resources."
  type        = map(string)
  default     = {}
}

variable "availability_zone" {
  description = "AZ for the self-provisioned subnet (standalone mode only, i.e. subnet_id == null). Empty picks the first available AZ, preserving the previous behaviour. Ignored when subnet_id is set, since the target module then owns the subnet and its AZ."
  type        = string
  default     = ""
}
