# deployments/gcp-cloudsql/postgres

OpenTofu module that provisions a Google Cloud SQL for PostgreSQL instance
(Enterprise edition, N4 machine series, HYPERDISK_BALANCED, single-zone,
Private IP only).

## Prerequisites

- `gcloud auth application-default login` (or `GOOGLE_APPLICATION_CREDENTIALS` set)
- `GOOGLE_CLOUD_PROJECT` set in the shell, or pass `project_id` explicitly
- Cloud SQL Admin API, Compute Engine API, and Service Networking API enabled on the target project:
  `gcloud services enable sqladmin.googleapis.com compute.googleapis.com servicenetworking.googleapis.com`

## Networking

Private IP requires peering this module's VPC with Google's managed
service-producer network (`google_service_networking_connection`). The module
also creates a subnetwork for the co-located GCE driver (see
`deployments/gce/loaddriver_gotpc`) in the same VPC and region. Cloud SQL's own
private IP lives in the separately reserved peering range, not in that
subnetwork.

## Inputs

| Variable | Description |
|----------|-------------|
| `project_id` | GCP project id. Empty (default) resolves from `GOOGLE_CLOUD_PROJECT`/gcloud config |
| `wait_for_peering_propagation` | Whether to wait 180s after creating the VPC peering connection for route propagation; benchctl's Go orchestration layer sets it |
| `db_instance_type` | Cloud SQL machine type, e.g. `db-custom-N4-2-4096` (N4-series custom type; `db-custom-N4-<vCPU>-<RAM_MB>`) |
| `engine_version` | Cloud SQL Postgres major-version enum, e.g. `POSTGRES_17` (no minor-version pinning is possible) |
| `region` | GCP region (default `us-central1`) |
| `zone` | GCP zone the instance is pinned to (default `us-central1-a`) |
| `pg_user` / `pg_password` | Application account credentials |
| `disk_type` | Cloud SQL disk type; only `HYPERDISK_BALANCED` is valid (required by the N4 machine series) |
| `disk_size_gb` | Disk capacity (GB) |
| `disk_provisioned_iops` | HYPERDISK_BALANCED provisioned IOPS |
| `disk_provisioned_throughput_mibps` | HYPERDISK_BALANCED provisioned throughput (MiB/s) |
| `backup_retention_period` | PITR log retention in days (1-7) |
| `tags` | Labels applied to the instance |

## Outputs

| Output | Description |
|--------|-------------|
| `host` | Private IP address |
| `port` | Always `5432` |
| `db` / `user` / `password` | Connection credentials |
| `network` / `subnetwork` / `zone` | Inputs to the GCE loaddriver module for co-location |
| `instance_name` | Cloud SQL instance name |
| `sslmode` | Always `require` |
