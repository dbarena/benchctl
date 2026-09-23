# Metrics

benchctl pushes benchmark metrics (TPM, latency percentiles, run info) to VictoriaMetrics, and Grafana displays them.

## Configuration

### VictoriaMetrics

Set the config keys in `~/.benchctl/config.yaml` with `benchctl config set`, e.g. `benchctl config set metrics.endpoint <url>`. The two secrets have no config key and come only from the environment.

| Config key | Env var | Purpose |
|---|---|---|
| `metrics.endpoint` | `BENCHCTL_METRICS_ENDPOINT` | VictoriaMetrics base URL (e.g. `http://localhost:8428`) |
| - | `BENCHCTL_METRICS_TOKEN` | Bearer token (secret); takes precedence over username/password |
| `metrics.username` | `BENCHCTL_METRICS_USERNAME` | Basic Auth username |
| - | `BENCHCTL_METRICS_PASSWORD` | Basic Auth password (secret) |

Each `BENCHCTL_METRICS_*` environment variable overrides the corresponding config key when set.

The `victoriametrics` collector checks the endpoint URL and credentials against the import endpoint before a run starts, so a missing or invalid token fails immediately instead of after the benchmark.

### Grafana

| Variable | Purpose |
|---|---|
| `BENCHCTL_GRAFANA_URL` | Grafana base URL (e.g. `https://grafana.example.com`) |
| `BENCHCTL_GRAFANA_SERVICE_TOKEN` | Service account token with Editor role |

## Local observability stack

For development, run a local VictoriaMetrics and Grafana stack:

```bash
mise run observability-up     # start VM + Grafana (http://localhost:3000)
mise run observability-down   # stop containers, preserve data
mise run observability-reset  # stop and wipe all data
```

The local stack needs no authentication. Set `BENCHCTL_METRICS_ENDPOINT=http://127.0.0.1:8428`.

Open http://localhost:3000 in a browser to access Grafana (user: `admin`, password: `admin`).

**macOS note:** use the literal `127.0.0.1`, not `localhost`. On macOS `localhost` resolves to `::1`, and the Docker-hosted VictoriaMetrics resets IPv6 connections. Grafana on port 3000 works with either address.

```bash
export BENCHCTL_METRICS_ENDPOINT=http://127.0.0.1:8428
```

## Running with metrics

Pass `--set collector=victoriametrics` to push metrics instead of printing them:

```bash
BENCHCTL_METRICS_ENDPOINT=https://victoria.example.com \
BENCHCTL_METRICS_TOKEN=<token> \
  ./benchctl run --async my-scenario.yaml --set collector=victoriametrics
```
