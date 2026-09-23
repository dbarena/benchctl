#!/bin/bash
# Deployed to /usr/local/bin/perf_benchctl.sh on the target machine.
# Managed by the perf-benchctl systemd service (Restart=always).
# Survives between-iteration reboots; writes one .data + .pids file per boot.
#
# Environment variables (set via systemd unit or drop-in override):
#   PERF_FREQ         Sample frequency in Hz              (default: 199)
#   PERF_CALL_GRAPH   Unwinding method: fp or dwarf       (default: fp)
#   PERF_DURATION     Recording duration in seconds       (default: 1800)
#   PERF_START_OFFSET Seconds to wait after finding PIDs  (default: 0)
#   PERF_TARGETS      Which processes to record:
#                       "all"              every postgres process (default)
#                       "<pgrep-pattern>"  arbitrary pattern passed to pgrep -f
#
# Output files (append-safe, timestamped, never overwritten):
#   /var/log/perf_benchctl_<TS>.data   raw perf samples
#   /var/log/perf_benchctl_<TS>.pids   one "<pid> <cmdline>" line per recorded PID
#   /var/log/perf_benchctl.log         human-readable event log

LOG=/var/log/perf_benchctl.log
TARGETS="${PERF_TARGETS:-all}"

log() {
    local msg="$(date -u): $*"
    echo "$msg" >> "$LOG"
    echo "$msg"
}

# Wait up to 120 s for the postgres postmaster to appear (confirms the DB is up).
echo "Waiting for postgres postmaster (up to 120s)..."
for i in $(seq 1 120); do
    POSTMASTER=$(pgrep -f "postgres -D" 2>/dev/null | head -1)
    [[ -n "$POSTMASTER" ]] && break
    sleep 1
done

if [[ -z "$POSTMASTER" ]]; then
    log "ERROR: postgres postmaster not found after 120s"
    exit 1
fi
echo "Postmaster found (pid $POSTMASTER)"

# Select PID set based on PERF_TARGETS.
if [[ "$TARGETS" == "all" ]]; then
    PID_LIST=$(pgrep -x postgres 2>/dev/null | paste -sd,)
    if [[ -z "$PID_LIST" ]]; then
        log "ERROR: no postgres processes found"
        exit 1
    fi
else
    # Bracket the first character so pgrep doesn't match its own cmdline.
    # e.g. "checkpointer" → "[c]heckpointer": the pgrep process has the literal
    # string "[c]heckpointer" in its args, which the regex [c]heckpointer
    # (meaning: the char 'c' followed by 'heckpointer') does not match.
    SAFE_TARGETS="[${TARGETS:0:1}]${TARGETS:1}"
    PID_LIST=$(pgrep -f "$SAFE_TARGETS" 2>/dev/null | paste -sd,)
    if [[ -z "$PID_LIST" ]]; then
        log "ERROR: no processes match pgrep pattern '$TARGETS'"
        exit 1
    fi
fi

TS=$(date -u '+%Y%m%dT%H%M%S')
DATA_FILE=/var/log/perf_benchctl_${TS}.data
PIDS_FILE=/var/log/perf_benchctl_${TS}.pids

# Write one "<pid> <cmdline>" line per recorded PID so the flamegraph command
# can filter by pattern (grep) without needing live processes.
echo "Recording PIDs:"
for pid in $(echo "$PID_LIST" | tr ',' '\n'); do
    cmdline="$(ps -p "$pid" -o args= 2>/dev/null || echo unknown)"
    printf '%s %s\n' "$pid" "$cmdline" | tee -a "$PIDS_FILE"
done

log "start  targets=$TARGETS pids=$PID_LIST" \
    "freq=${PERF_FREQ:-199} call_graph=${PERF_CALL_GRAPH:-fp}" \
    "offset=${PERF_START_OFFSET:-0}s duration=${PERF_DURATION:-1800}s" \
    "-> $DATA_FILE"

# Optional delay before recording (used by bench-perf auto --at <S>).
if [[ "${PERF_START_OFFSET:-0}" -gt 0 ]]; then
    echo "Waiting ${PERF_START_OFFSET}s before recording..."
    sleep "${PERF_START_OFFSET}"
fi

echo "Starting perf record (${PERF_DURATION:-1800}s at ${PERF_FREQ:-199}Hz, call-graph=${PERF_CALL_GRAPH:-fp}) -> $DATA_FILE"
exec perf record \
    -F "${PERF_FREQ:-199}" \
    --call-graph "${PERF_CALL_GRAPH:-fp}" \
    -p "$PID_LIST" \
    -o "$DATA_FILE" \
    -- sleep "${PERF_DURATION:-1800}"
