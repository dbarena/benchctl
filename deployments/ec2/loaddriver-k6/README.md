# deployments/ec2/loaddriver-k6

OpenTofu module that provisions a k6 load driver instance on EC2 (Graviton4 ARM64,
`c8gd.xlarge` by default). The module places the instance in the same subnet
as the target, so benchmark traffic stays within the VPC.

## Inputs

| Variable | Description |
|----------|-------------|
| `instance_type` | EC2 instance type (e.g. `c8gd.xlarge`) |

## Outputs

| Output | Description |
|--------|-------------|
| `public_ip` | Public IP of the driver instance (used by benchctl for SSH/SCP bootstrap) |
| `ssh_private_key_path` | Absolute path to the generated SSH private key (written into `./tofu-state/<runID>/loaddriver-k6/`) |

## User data

Installs k6 (pre-built binary from GitHub releases) at `/usr/local/bin/k6`.

## Script upload

The EC2 driver provider's bootstrap step (Go-side SCP) uploads the script
directories, not this module. It copies every subdirectory next to the
scenario YAML into `~/benchctl/` on the instance, where `benchctl resume`
resolves step commands relative to the scenario file path.

## benchctl binary delivery

The bootstrap (`benchctl run --async`) copies the scenario YAML over and SSHes
in to start `benchctl resume`, but it delivers no benchctl binary. Put the
binary at `~/benchctl/benchctl` on the instance before provisioning completes.

Options (pick one):

1. **Cross-compile locally** and point the scenario at it:
   ```bash
   GOOS=linux GOARCH=arm64 go build -o ./bin/benchctl-linux-arm64 ./cmd/benchctl
   ```
   Then uncomment `benchctl_binary` in `scenarios/k6-edge-ec2.yaml`.

2. **Build on the instance**: SSH in with the generated key and either clone
   the repo (if you have SSH-agent forwarding or a deploy key wired up) or
   copy the source, then `go build ./cmd/benchctl`.

3. **Manual SCP** of a pre-built arm64 binary from wherever CI produces it.
