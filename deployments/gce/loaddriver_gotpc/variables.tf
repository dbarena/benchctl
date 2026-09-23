variable "project_id" {
  description = "GCP project id. Empty (default) lets the google provider resolve it from GOOGLE_CLOUD_PROJECT/gcloud config, same as deployments/gcp-cloudsql/postgres. Pass the same value given to the target module."
  type        = string
  default     = ""
}

variable "vector_enabled" {
  description = "Whether to start the Vector metrics agent on boot. When false, Vector is installed but not started."
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
  description = "GCE machine type for the load generator. Default is Axion (C4A, ARM64) for architecture parity with the EC2 driver's Graviton4 default -- see deployments/ec2/loaddriver_gotpc/variables.tf. Override to an N2/C4 (x86) type if C4A hits regional capacity limits; go-tpc's install script already branches on `uname -m`."
  type        = string
  default     = "c4a-standard-4"
}

variable "network" {
  description = "Self-link of the VPC network to attach the driver to. Pass target.outputs.network to co-locate with the gcp-cloudsql/postgres module. When null, the module creates its own VPC network."
  type        = string
  default     = null
}

variable "subnetwork" {
  description = "Self-link of the subnetwork to attach the driver to. Pass target.outputs.subnetwork to co-locate with the gcp-cloudsql/postgres module. When null, the module creates its own subnetwork."
  type        = string
  default     = null
}

variable "zone" {
  description = "GCP zone to place the driver instance in. Pass target.outputs.zone to land in the same zone as the Cloud SQL instance -- the co-location handoff equivalent of RDS's subnet_id output (see deployments/rds/postgres/outputs.tf)."
  type        = string
  default     = "us-central1-a"
}

variable "tags" {
  description = "Labels applied to all GCP resources. Named `tags` for symmetry with the RDS/EC2 scenario vars -- applied as GCP labels, not GCP \"network tags\" (a separate mechanism; see main.tf's own network tag usage for firewall targeting)."
  type        = map(string)
  default     = {}
}
