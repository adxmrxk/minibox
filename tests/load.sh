#!/usr/bin/env bash
# Control-plane load test. Starts a coordinator with TLS against an etcd server and drives
# it with simulated agents (see cmd/mbload). Prints throughput, latency percentiles, and the
# coordinator's CPU time and peak memory for each run.
#
#   ./tests/load.sh "10 50 100" 2000
#     first argument: agent counts to test, second: jobs per run
#
# Needs: bin/mbd, bin/mbload, an etcd server at $ETCD_URL (default http://127.0.0.1:2379).
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$HERE/.."
MBD="${MBD:-$ROOT/bin/mbd}"
MBLOAD="${MBLOAD:-$ROOT/bin/mbload}"
ETCD_URL="${ETCD_URL:-http://127.0.0.1:2379}"
AGENT_COUNTS="${1:-10 50 100}"
JOBS="${2:-2000}"
SUBMITTERS="${SUBMITTERS:-16}"
PORT=18800
. "$HERE/tls.sh"

for f in "$MBD" "$MBLOAD"; do
    [ -x "$f" ] || { echo "missing $f"; exit 2; }
done

WORK="$(mktemp -d /tmp/mb-load.XXXXXX)"
trap 'kill "$MBD_PID" 2>/dev/null; wait 2>/dev/null; rm -rf "$WORK"' EXIT
make_tls_cert "$WORK"
CA="$WORK/tls.crt"; KEY="$WORK/tls.key"
MAX=$(echo "$AGENT_COUNTS" | tr ' ' '\n' | sort -n | tail -n1)

# One user, and one agent per simulated slot pool, each with its own hashed token.
RUN="r$$"
USER_TOKEN="load-user-$RUN"
printf 'load %s\n' "$(token_hash "$USER_TOKEN")" > "$WORK/clients.txt"
: > "$WORK/agents.txt"
# Names are three digits (001, 002, ...) to match what cmd/mbload derives, whatever the max.
for n in $(seq 1 "$MAX"); do
    i=$(printf '%03d' "$n")
    printf 'load-%s-%s %s\n' "$RUN" "$i" "$(token_hash "agent-$RUN-$i")" >> "$WORK/agents.txt"
done

# PPROF_OUT=<prefix> captures a 20-second CPU profile of mbd during each run, written to
# <prefix>-<agents>.prof. Read it with: go tool pprof -top <file>.
PPROF_ARGS=()
if [ -n "${PPROF_OUT:-}" ]; then PPROF_ARGS=(--pprof 127.0.0.1:6060); fi
"$MBD" --addr "127.0.0.1:$PORT" --tls-cert "$CA" --tls-key "$KEY" \
    --client-tokens "$WORK/clients.txt" --agent-tokens "$WORK/agents.txt" \
    --etcd "$ETCD_URL" --prefix "/load-$RUN" "${PPROF_ARGS[@]}" >"$WORK/mbd.log" 2>&1 &
MBD_PID=$!
for _ in $(seq 100); do
    curl -s --cacert "$CA" -o /dev/null "https://127.0.0.1:$PORT/metrics" && break
    sleep 0.1
done

# CPU ticks (utime+stime) and peak RSS of the coordinator, read from /proc.
cpu_ticks() { awk '{print $14 + $15}' "/proc/$MBD_PID/stat"; }
peak_rss_kb() { awk '/VmHWM/ {print $2}' "/proc/$MBD_PID/status"; }
CLK=$(getconf CLK_TCK)

for n in $AGENT_COUNTS; do
    if [ -n "${PPROF_OUT:-}" ]; then
        # Start the profile a few seconds in, so it covers steady load rather than startup.
        ( sleep 4; curl -s -o "$PPROF_OUT-$n.prof" "http://127.0.0.1:6060/debug/pprof/profile?seconds=20" ) &
    fi
    before=$(cpu_ticks)
    out=$("$MBLOAD" --server "https://127.0.0.1:$PORT" --ca "$CA" --run "$RUN" \
        --agents "$n" --jobs "$JOBS" --submitters "$SUBMITTERS" 2>"$WORK/load-$n.err")
    after=$(cpu_ticks)
    echo "== agents=$n"
    echo "$out"
    cpu_s=$(awk -v a="$after" -v b="$before" -v c="$CLK" 'BEGIN { printf "%.2f", (a-b)/c }')
    echo "coordinator_cpu_s=$cpu_s peak_rss_mb=$(awk -v k="$(peak_rss_kb)" 'BEGIN { printf "%.1f", k/1024 }')"
    [ -s "$WORK/load-$n.err" ] && head -n 3 "$WORK/load-$n.err"
done

ETCDCTL_API=3 etcdctl --endpoints="$ETCD_URL" del --prefix "/load-$RUN" >/dev/null 2>&1
