# benchctl

A pluggable benchmark orchestrator: it manages isolated benchmark environments in the cloud.

## Prerequisites

Install OpenTofu. On MacOS run `brew install opentofu`. See their [installation instructions](https://opentofu.org/docs/intro/install/) for other platforms.

**Supabase CLI** (required only for the `supabase` target provider):

Install the beta version by following the [getting started guide](https://supabase.com/docs/guides/local-development/cli/getting-started#beta-channel). The binary must be invokable as `supabase` from the shell (npm/npx wrappers are not supported).

Authenticate via `supabase login` or set `BENCHCTL_SUPABASE_ACCESS_TOKEN` to a personal access token.

## Install

### From a release

```bash
curl -fsSL https://raw.githubusercontent.com/dbarena/benchctl/main/scripts/install.sh | sh
```

Installs to `/usr/local/bin/benchctl`. State and cached artifacts live under `~/.benchctl`.

### From source

This requires [mise](https://mise.jdx.dev/):

```bash
git clone git@github.com:dbarena/benchctl.git
cd benchctl
mise trust
mise run build
```

**To use plain `benchctl` from any directory** (optional, but recommended over
`./benchctl`): source the dev shim once per shell session:

```bash
source scripts/use.sh
```

This puts a wrapper on `PATH` ahead of anything else named `benchctl`. The
wrapper runs `mise run build` if needed, so `benchctl` always
reflects your current source, including uncommitted changes. A broken build
fails the invocation instead of falling back to a stale binary.

Caveats:

- **Source the script, do not execute it.** `source scripts/use.sh` or `. scripts/use.sh`.
- The shim affects only your *current* shell session. Put the `source` line in your shell's rc file to keep it across sessions.

## Quick start

Run scenarios locally with Docker Compose for smoke tests and tool validation.

```bash
# Run a scenario (blocking)
benchctl run scenarios/postgres-tpcc-local.yaml

# Override inputs
benchctl run scenarios/postgres-tpcc-local.yaml --set warehouses=10 --set duration=10m

# Keep infrastructure running after the run for inspection, then clean up manually
benchctl run scenarios/postgres-tpcc-local.yaml --no-teardown
benchctl teardown <run-id>

# Validate a scenario file without running it
benchctl validate scenarios/postgres-tpcc-local.yaml

# Inspect a scenario's inputs, fixtures, and benchmark steps without running it
benchctl info scenarios/postgres-tpcc-local.yaml

# Preview the effect of --set overrides (e.g. on fixture combinations) before running
benchctl info scenarios/postgres-tpcc-local.yaml --set warehouses=10 --set duration=10m
```

## EC2 async workflow

The async workflow provisions a target (e.g. Postgres) and a load-driver EC2 instance, transfers benchctl to the driver, and starts `benchctl resume` as a background process. The local process exits after handoff; the run continues autonomously on the driver.

**Prerequisites:**

Your AWS credentials must be active in the shell (e.g. `AWS_PAGER="" aws sts get-caller-identity --profile $MY_AWS_PROFILE` should return without error).

**Start a run:**

```bash
# select the correct AWS profile; consider putting this into .zshrc / .bashrc
export AWS_PROFILE=MY_AWS_PROFILE_HERE
benchctl run --async scenarios/postgres-tpcc-ec2.yaml
```

**Monitor:**

```bash
benchctl status <run-id>   # show current phase
benchctl wait <run-id>     # block until complete or failed

# Connect to the driver and tail the live log
benchctl connect <run-id> driver
tail -f ~/benchctl/resume.log
```

Use `--print` to get the SSH command without executing it:

```bash
benchctl connect <run-id> driver --print
```

**Fetching results:** copies the driver's resume log and any result files to a local directory. Calling it before the run finishes is safe; benchctl skips the files that do not exist yet.

```bash
benchctl fetch <run-id> --dest ./results/<run-id>
```

**Naming the run ID up front:** `run --async` normally generates the run ID and reveals it
only after provisioning and driver handoff finish, which can take a while. Supply your own
ID instead:

```bash
benchctl run --async --run-id my-run-20260817-1 scenarios/postgres-tpcc-ec2.yaml
```

**Teardown:**

```bash
benchctl teardown --purge <run-id>
```

See [docs/metrics.md](docs/metrics.md) for metrics configuration.

## GCP async workflow

Same async model as EC2, but the target is Google Cloud SQL for Postgres and the load-driver is a co-located GCE instance in the same zone/VPC (Private IP only; the Cloud SQL instance has no public endpoint).

**Prerequisites:**

- GCP application-default credentials active in the shell: `gcloud auth application-default login`
- `GOOGLE_CLOUD_PROJECT` set to the target project, e.g. `export GOOGLE_CLOUD_PROJECT=my-gcp-project`. Consider putting this into `.zshrc`/`.bashrc`. The `google` Terraform provider reads this directly; benchctl never hardcodes a project ID.
- The target project must have the Cloud SQL Admin API, Compute Engine API, and Service Networking API enabled (`gcloud services enable sqladmin.googleapis.com compute.googleapis.com servicenetworking.googleapis.com`)
- The Service Networking service agent needs `roles/servicenetworking.serviceAgent` on the project. Google normally grants this when the Service Networking API is enabled. Where that did not happen, Private IP Cloud SQL provisioning fails with a permission error that appears to name *your own* IAM role. The grant it reports missing belongs to the service agent, a separate service-account principal. Check and fix with:
  ```bash
  PROJECT_NUMBER=$(gcloud projects describe "$GOOGLE_CLOUD_PROJECT" --format="value(projectNumber)")
  gcloud projects get-iam-policy "$GOOGLE_CLOUD_PROJECT" \
    --flatten="bindings[].members" \
    --filter="bindings.members:service-${PROJECT_NUMBER}@service-networking.iam.gserviceaccount.com AND bindings.role:roles/servicenetworking.serviceAgent"
  # if that prints nothing, grant it (requires an IAM admin / project owner):
  gcloud projects add-iam-policy-binding "$GOOGLE_CLOUD_PROJECT" \
    --member="serviceAccount:service-${PROJECT_NUMBER}@service-networking.iam.gserviceaccount.com" \
    --role="roles/servicenetworking.serviceAgent"
  ```
This is a one-time, per-project fix, not per-user.

**Start a run:**

```bash
export GOOGLE_CLOUD_PROJECT=my-gcp-project
# uses the GCP scenario from dbarenactl, see https://github.com/dbarena/dbarenactl
benchctl run --async ~/my-projects/dbarenactl/candidates/gcp/cloud-sql-for-postgres/tpcc/scenario.yaml
```

Monitor, connect, and tear down exactly as in the EC2 workflow above (`benchctl status`/`wait`/`connect`/`teardown`).

## Command reference

| Command | Description |
|---|---|
| `benchctl run <scenario>` | Execute a scenario end-to-end (blocking) |
| `benchctl run --async <scenario>` | Provision and hand off to remote driver |
| `benchctl run [--async] --run-id <id> <scenario>` | Same, but use a caller-supplied run ID instead of generating one |
| `benchctl resume <run-id>` | Resume a run on the driver instance |
| `benchctl status` | List active runs |
| `benchctl status --all` | List all runs, including torn-down ones |
| `benchctl status <run-id>` | Show detail and metadata for a specific run |
| `benchctl wait <run-id>` | Block until a run completes or fails |
| `benchctl teardown <run-id>` | Tear down infrastructure for a run |
| `benchctl connect <run-id> driver\|target` | Open an interactive shell on the driver or target |
| `benchctl fetch <run-id> --dest <dir>` | Copy result files and logs from the driver to the local machine |
| `benchctl validate <scenario>` | Validate a scenario file without running it |
| `benchctl info <scenario> [--set key=value]` | Show a scenario's inputs, fixtures, and benchmark steps; `--set` previews the effect of overrides |
| `benchctl results <run-id>` | Render a per-transaction summary table |
| `benchctl auth login` | Authenticate to shared Supabase state store with GitHub SSO (optional) |
| `benchctl auth status` | Verify auth and state store access |
| `benchctl providers list` | List available providers and adapters |
| `benchctl purge <run-id>` | Delete a run from the state store |

## Development

```bash
mise trust          # allow mise to read mise.toml
mise install        # install Go
mise run build      # build ./benchctl
mise run test       # run the test suite
mise run lint       # gofmt + go vet
mise run format     # format Go files
```

Cross-compilation:

```bash
mise run build-linux   # linux/amd64 + linux/arm64 → ./bin/
mise run build-all     # all four platforms → ./bin/
```

Install workload tools locally for ad-hoc use:

```bash
mise run install-go-tpc   # → $GOPATH/bin/go-tpc
mise run install-k6       # → $GOPATH/bin/k6
```

## Further reading

- [docs/concepts.md](docs/concepts.md): scenario structure, domain model, YAML reference, template variables
- [docs/state-store.md](docs/state-store.md): local vs remote store, authentication, CI setup, admin operations
- [docs/metrics.md](docs/metrics.md): VictoriaMetrics, Grafana, local observability stack
- [docs/fixtures.md](docs/fixtures.md): multi-dimensional benchmarking with parameter sweeps
- [docs/performance-analysis.md](docs/performance-analysis.md): CPU profiling, flamegraphs, hardware counters
