# deployments/ec2/postgres

OpenTofu module that provisions a Postgres instance on EC2 as a benchmark target.

## Inputs

| Variable | Description |
|----------|-------------|
| `instance_type` | EC2 instance type (e.g. `r6i.xlarge`) |
| `ami` | AMI ID for the Postgres image |

## Outputs

| Output | Description |
|--------|-------------|
| `host` | Private IP of the Postgres instance (used by the driver for ready_check and go-tpc within the VPC) |
| `public_ip` | Public IP of the Postgres instance (for ad-hoc SSH access) |
| `port` | Postgres port |
| `subnet_id` | Subnet holding the instance (used by the load driver) |
| `ssh_private_key_path` | Absolute path to the SSH private key written into the per-run tofu working directory (`./tofu-state/<runID>/postgres`) |
