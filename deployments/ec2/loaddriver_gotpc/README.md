# deployments/ec2/loaddriver_gotpc

OpenTofu module that provisions a load driver instance on EC2 (Graviton4 ARM64,
`c8gd.xlarge` by default). The module places the instance in the same subnet
as the target, so benchmark traffic stays within the VPC.

## Inputs

| Variable | Description |
|----------|-------------|
| `instance_type` | EC2 instance type (e.g. `c8gd.xlarge`) |
| `subnet_id` | Subnet ID; must match `target.outputs.subnet_id` |

## Outputs

| Output | Description |
|--------|-------------|
| `public_ip` | Public IP of the driver instance (used by benchctl for SSH/SCP bootstrap) |
| `ssh_private_key_path` | Absolute path to the generated SSH private key (written into `./tofu-state/<runID>/loaddriver_gotpc/`) |

