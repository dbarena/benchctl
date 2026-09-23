# Performance analysis

CPU profiling infrastructure for benchctl EC2 targets. Built on Linux `perf` and the [FlameGraph](https://github.com/brendangregg/FlameGraph) toolkit.

## How it works

Terraform installs two files when it provisions an EC2 postgres target:

- **`/usr/local/bin/perf_benchctl.sh`**: the recording script
- **`perf-benchctl.service`**: a systemd unit that runs it

Target creation enables the unit but leaves it stopped. Starting it kicks off `perf record` on all postgres processes for up to 30 minutes (configurable). The unit sets `Restart=always`, so the service restarts after an iteration reboot and records a fresh `.data` file.

### Recording script

The script waits up to 120 seconds for the postgres postmaster to appear, then resolves PIDs and launches `perf record`.

Environment variables that control its behavior:

| Variable | Default | Description |
|---|---|---|
| `PERF_TARGETS` | `all` | `all` = every postgres process; any other string = `pgrep -f` pattern |
| `PERF_FREQ` | `199` | Sample frequency in Hz |
| `PERF_CALL_GRAPH` | `fp` | Unwinding method: `fp` (frame pointer) or `dwarf` |
| `PERF_DURATION` | `1800` | Recording window in seconds |
| `PERF_START_OFFSET` | `0` | Seconds to wait after PIDs are found before recording starts |

Output files on the target:

```
/var/log/perf_benchctl_<TS>.data   raw perf samples
/var/log/perf_benchctl_<TS>.pids   one "<pid> <cmdline>" line per recorded PID
/var/log/perf_benchctl.log         event log
```

The `.pids` file records the full command line for each PID at recording time, so you can filter a flamegraph by process type (e.g. `"background writer"`) long after teardown.

---

## bench-perf skill

The `bench-perf` Claude skill orchestrates the full profiling workflow.

```
/bench-perf <subcommand> <run-id> [options]
```

### Subcommands

| Subcommand | Usage | Description |
|---|---|---|
| `start` | `start <run-id>` | Start the pre-installed `perf-benchctl` service |
| `stop` | `stop <run-id>` | Stop the service, finalize the current `.data` file |
| `status` | `status <run-id>` | List `.data` files with sizes and sample counts |
| `pull` | `pull <run-id> [--iter N \| --ts TS]` | Download a `.data` + `.pids` pair locally |
| `flamegraph` | `flamegraph <run-id> [--iter N] [--from S] [--to S] [--pid <pattern>]` | Generate SVG flamegraph |
| `diff` | `diff <run-id> --iter1 N --iter2 M [--from S] [--to S]` | Differential SVG (red=more in iter2, blue=more in iter1) |
| `lock` | `lock <run-id> [--duration 30]` | bpftrace LWLock wait and hold latency histograms |
| `stat` | `stat <run-id> [--duration 60] [--interval 10]` | `perf stat` hardware counters, one sample per `--interval` seconds |
| `offcpu` | `offcpu <run-id> [--duration 30] [--min-ms 1]` | bpftrace off-CPU flamegraph |
| `auto` | `auto <run-id> --at S --duration S` | Record one window starting `--at` seconds after postgres appears |

### Common workflow

```bash
# 1. Start profiling (service is already installed, just start it)
/bench-perf start my-run-id

# 2. Run your benchmark workload

# 3. Stop and check what was captured
/bench-perf stop my-run-id
/bench-perf status my-run-id

# 4. Download the recording
/bench-perf pull my-run-id --iter 2

# 5. Generate a flamegraph for all processes
/bench-perf flamegraph my-run-id --iter 2

# 6. Filter to a specific process type
/bench-perf flamegraph my-run-id --iter 2 --pid "background writer"
/bench-perf flamegraph my-run-id --iter 2 --pid "checkpointer"

# 7. Compare two iterations (red = more CPU in iter2)
/bench-perf diff my-run-id --iter1 1 --iter2 2
```

### Flamegraph time-window filtering

`--from` and `--to` are offsets in seconds from the start of the recording. Useful for isolating a crash or spike:

```bash
# Show only the 60-second window starting 300s into the recording
/bench-perf flamegraph my-run-id --from 300 --to 360
```

### Recording only a specific window (`auto`)

Use `auto` when you want exactly one window captured at a known point in the benchmark:

```bash
# Record 120 seconds starting 600 seconds after postgres appears
/bench-perf auto my-run-id --at 600 --duration 120
```

To restore always-on recording: `bench-perf start my-run-id` (removes the override and restarts).

---

## Advanced analysis

### Off-CPU flamegraph

Shows where postgres processes spend time blocked (waiting on I/O, locks, sleep). Requires no debug symbols.

```bash
/bench-perf offcpu my-run-id --duration 30 --min-ms 1
```

### Lock contention

bpftrace uprobe tracing of `LWLockAcquire` / `LWLockRelease`. Reports wait and hold latency histograms per call stack.

```bash
/bench-perf lock my-run-id --duration 30
```

Requires that the postgres binary retains function symbols. The nightly builds in this project retain them. When symbols are missing, fall back to `flamegraph`.

### Hardware counters (`perf stat`)

Attaches to live postgres PIDs and reads PMU counters at a fixed interval (default 10s). It measures four process groups in parallel, each producing its own CSV:

| Group | Processes |
|---|---|
| `checkpointer` | checkpoint writer |
| `bg_writer` | background buffer writer |
| `wal_writer` | WAL writer |
| `workers` | all connection handler processes |

Events captured: `cycles`, `instructions`, `cache-misses`, `cache-references`, `branch-misses`, `branches`, `context-switches`.

Key metrics:

- **IPC** (instructions/cycles): low IPC during a throughput drop suggests stall cycles from lock spin or I/O waits
- **Cache miss rate** (`cache-misses / cache-references`): high misses indicate the working set exceeds L2/L3
- **Context switches**: a high rate in the workers group signals lock contention

```bash
/bench-perf stat my-run-id --duration 60
/bench-perf stat my-run-id --duration 1800 --interval 10   # cover a full iteration
```

Each group's output lands in `results/<run-id>/perf_stat_<group>_<ts>.csv`. The first CSV column holds elapsed seconds from measurement start; add the benchmark start epoch to align it against the throughput timeseries.

**Notes on multiplexing.** AWS HVM guests typically expose fewer programmable PMU counter slots than the 5 programmable events requested. Perf handles this by time-multiplexing between event groups and scaling the reported counts by `total_time / enabled_time`. Expect ~40-60% scaling factors on programmable events; the reported values are already corrected. `<not counted>` (0% scaling) on maintenance processes (checkpointer, bg_writer, wal_writer) is normal: those processes sleep most of the time, so the multiplexer gets no scheduling opportunities. The workers group still counts normally.

---

## Override PERF_TARGETS via drop-in

To record a specific process set instead of all postgres, create a systemd drop-in before starting:

```bash
sudo mkdir -p /etc/systemd/system/perf-benchctl.service.d
printf '[Service]\nEnvironment=PERF_TARGETS=postgres background\n' \
  | sudo tee /etc/systemd/system/perf-benchctl.service.d/target.conf
sudo systemctl daemon-reload
sudo systemctl start perf-benchctl
```

Remove the drop-in to revert to all-process recording.
