#!/usr/bin/env bash
# Measures how long minibox adds to starting a trivial command.
#   sudo ./tests/bench.sh [RUNS]     (default 50)
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
BIN="${BIN:-$HERE/../target/release/minibox}"
RUNS="${1:-50}"

if [ "$(id -u)" -ne 0 ]; then
    echo "run as root (sudo)"
    exit 2
fi

# Average wall-clock microseconds for RUNS invocations of "$@".
avg_us() {
    local start end i
    start=$(date +%s%N)
    for ((i = 0; i < RUNS; i++)); do
        "$@" >/dev/null 2>&1
    done
    end=$(date +%s%N)
    echo $(( (end - start) / RUNS / 1000 ))
}

ms() { awk -v us="$1" 'BEGIN { printf "%.2f ms", us / 1000 }'; }

echo "runs per case: $RUNS"

host_us=$(avg_us /bin/true)
echo "host /bin/true:                 $(ms "$host_us")"

sandbox_us=$(avg_us "$BIN" run -- /bin/true)
echo "minibox (namespaces + net):     $(ms "$sandbox_us")"

nohard_us=$(avg_us "$BIN" run --no-harden -- /bin/true)
echo "minibox (--no-harden):          $(ms "$nohard_us")"

limits_us=$(avg_us "$BIN" run --mem 64 --pids 32 -- /bin/true)
echo "minibox (+ cgroup limits):      $(ms "$limits_us")"

echo "overhead of full sandbox:       $(ms $((sandbox_us - host_us)))"
