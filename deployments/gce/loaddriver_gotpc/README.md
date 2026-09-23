# deployments/gce/loaddriver_gotpc

OpenTofu module that provisions a load driver instance on GCE (Axion ARM64,
`c4a-standard-4` by default, matching the architecture of the EC2 driver's
Graviton4 default). Given `network`, `subnetwork` and `zone`, the module places
the instance in the same VPC, subnetwork and zone as the target, so benchmark
traffic stays within one zone.

## Prerequisites

Same as `deployments/gcp-cloudsql/postgres`: `gcloud auth application-default
login`, `GOOGLE_CLOUD_PROJECT` set, and the Compute Engine API enabled.

## Boot script

The `user-data` metadata key (cloud-init) delivers this script, not GCE's
native `startup-script` key. `internal/providers/sshdriver`'s async bootstrap
runs `cloud-init status --wait` on every cloud, so cloud-init must execute this
script. Ubuntu's GCE images ship cloud-init configured to read `user-data`, same
as AWS's `user_data`.

This module and every EC2 module share the systemd-disable "reduce
variability" step via `deployments/shared/reduce-variability.sh`. See that file
for what it disables and why.

## Inputs

| Variable | Description |
|----------|-------------|
| `project_id` | GCP project id. Empty (default) resolves from `GOOGLE_CLOUD_PROJECT`/gcloud config |
| `instance_type` | GCE machine type (default `c4a-standard-4`) |
| `network` / `subnetwork` | Must match `target.outputs.network` / `target.outputs.subnetwork` for co-location. When null, the module creates its own VPC/subnetwork |
| `zone` | Must match `target.outputs.zone` for co-location |

## Outputs

| Output | Description |
|--------|-------------|
| `public_ip` | Ephemeral external IP of the driver instance (used by benchctl for SSH/SCP bootstrap) |
| `ssh_private_key_path` | Absolute path to the generated SSH private key (written into `./tofu-state/<runID>/loaddriver_gotpc/`) |
