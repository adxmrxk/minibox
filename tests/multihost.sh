#!/usr/bin/env bash
# Multi-host test. Simulates a second machine with a network namespace that has its own
# IP address, its own agent, and its own state and image directories. The coordinator
# and the first agent run in the main namespace.
#
# This proves the agent protocol works across a real network hop and that jobs move to
# another host when one dies. It does NOT prove anything about a separate physical
# computer, which still needs a real second machine (see the README).
#
#   sudo ./tests/multihost.sh
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$HERE/.."
BIN="${BIN:-$ROOT/target/release/minibox}"
MBD="${MBD:-$ROOT/bin/mbd}"
MBAGENT="${MBAGENT:-$ROOT/bin/mbagent}"
ROOTFS="${ROOTFS:-/opt/minibox-rootfs}"
NS=mbhostb
PORT=18788
TOKEN="multi-$$"
WORK="$(mktemp -d /tmp/mb-multi.XXXXXX)"
. "$HERE/tls.sh"
CA="$WORK/tls.crt"; KEY="$WORK/tls.key"; make_tls_cert "$WORK"
CLIENTS="$WORK/clients.txt"; printf 'tester %s\nbob %s\n' "$(token_hash "$TOKEN")" "$(token_hash "bob-$$")" > "$CLIENTS"
URL="https://10.99.0.1:$PORT"
pass=0
fail=0

if [ "$(id -u)" -ne 0 ]; then
    echo "run as root (sudo)"
    exit 2
fi
for f in "$BIN" "$MBD" "$MBAGENT"; do
    [ -x "$f" ] || { echo "missing $f (build it first)"; exit 2; }
done

report() {
    local name="$1" ok="$2"
    if [ "$ok" = "yes" ]; then
        echo "PASS  $name"
        pass=$((pass + 1))
    else
        echo "FAIL  $name"
        fail=$((fail + 1))
    fi
}

cleanup() {
    [ -n "${AGENT_A:-}" ] && kill "$AGENT_A" 2>/dev/null
    [ -n "${AGENT_B:-}" ] && kill "$AGENT_B" 2>/dev/null
    [ -n "${MBD_PID:-}" ] && kill "$MBD_PID" 2>/dev/null
    wait 2>/dev/null
    ETCDCTL_API=3 etcdctl --endpoints="$ETCD_URL" del --prefix "$ETCD_PREFIX" >/dev/null 2>&1
    ip netns del "$NS" 2>/dev/null
    ip link del mbveth0 2>/dev/null
    rm -rf "$WORK"
}
trap cleanup EXIT

# ---- network: host B lives in its own namespace with its own address ----

ip netns del "$NS" 2>/dev/null
ip netns add "$NS"
ip link add mbveth0 type veth peer name mbveth1
ip link set mbveth1 netns "$NS"
ip addr add 10.99.0.1/24 dev mbveth0
ip link set mbveth0 up
ip netns exec "$NS" ip addr add 10.99.0.2/24 dev mbveth1
ip netns exec "$NS" ip link set mbveth1 up
ip netns exec "$NS" ip link set lo up
if ip netns exec "$NS" ping -c1 -W2 10.99.0.1 >/dev/null 2>&1; then
    report "network: host B can reach the coordinator's address" yes
else
    report "network: host B can reach the coordinator's address" no
    exit 1
fi

# ---- each host has its own image store and state ----

tar -C "$ROOTFS" -cf "$WORK/ci.tar" .
MINIBOX_IMAGE_DIR="$WORK/images-a" MINIBOX_STATE_DIR="$WORK/state-a" "$BIN" image import ci "$WORK/ci.tar" >/dev/null
MINIBOX_IMAGE_DIR="$WORK/images-b" MINIBOX_STATE_DIR="$WORK/state-b" "$BIN" image import ci "$WORK/ci.tar" >/dev/null

# ---- coordinator ----

ETCD_URL="${ETCD_URL:-http://127.0.0.1:2379}"
ETCD_PREFIX="/multi-$$"
TOKEN_A="agent-a-$$"
TOKEN_B="agent-b-$$"
printf 'host-a %s\nhost-b %s\n' "$(token_hash "$TOKEN_A")" "$(token_hash "$TOKEN_B")" > "$WORK/agents.txt"
"$MBD" --addr "0.0.0.0:$PORT" --tls-cert "$CA" --tls-key "$KEY" --client-tokens "$CLIENTS" --agent-tokens "$WORK/agents.txt" \
    --etcd "$ETCD_URL" --prefix "$ETCD_PREFIX" >"$WORK/mbd.log" 2>&1 &
MBD_PID=$!
for _ in $(seq 50); do
    curl -s --cacert "$CA" -o /dev/null "https://127.0.0.1:$PORT/metrics" && break
    sleep 0.1
done

start_b() {
    ip netns exec "$NS" env MINIBOX_IMAGE_DIR="$WORK/images-b" MINIBOX_STATE_DIR="$WORK/state-b" \
        "$MBAGENT" --ca "$CA" --server "$URL" --token "$TOKEN_B" --name host-b --slots 2 \
        --minibox "$BIN" --interval 500ms >"$WORK/agent-b.log" 2>&1 &
    AGENT_B=$!
}

start_a() {
    env MINIBOX_IMAGE_DIR="$WORK/images-a" MINIBOX_STATE_DIR="$WORK/state-a" \
        "$MBAGENT" --ca "$CA" --server "https://127.0.0.1:$PORT" --token "$TOKEN_A" --name host-a --slots 2 \
        --minibox "$BIN" --interval 500ms >"$WORK/agent-a.log" 2>&1 &
    AGENT_A=$!
}

submit() {
    curl -s --cacert "$CA" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
        -d "$1" "https://127.0.0.1:$PORT/v1/jobs" | grep -o '"id": *"[^"]*"' | head -n1 | sed 's/.*"\([^"]*\)"$/\1/'
}

job_field() {
    curl -s --cacert "$CA" -H "Authorization: Bearer $TOKEN" "https://127.0.0.1:$PORT/v1/jobs/$1" \
        | grep -o "\"$2\": *\"[^\"]*\"" | head -n1 | sed 's/.*: *"\([^"]*\)"$/\1/'
}

wait_for() {
    # wait_for JOB FIELD VALUE TIMEOUT_SECONDS
    local id="$1" field="$2" want="$3" limit="$4" i
    for ((i = 0; i < limit * 5; i++)); do
        [ "$(job_field "$id" "$field")" = "$want" ] && return 0
        sleep 0.2
    done
    return 1
}

# ---- 1. a job runs on host B, which lives across the network ----

start_b
long_id=$(submit '{"image":"ci","cmd":["sh","-c","sleep 15; echo finished-after-failover"],"net":"isolated"}')
if wait_for "$long_id" status assigned 20; then
    holder=$(job_field "$long_id" agent)
    report "multihost: job is assigned to host-b across the network (agent=$holder)" "$([ "$holder" = "host-b" ] && echo yes || echo no)"
else
    report "multihost: job is assigned to host-b across the network" no
fi

# ---- 2. host B dies mid-job. Its job must move to host A. ----

kill -9 "$AGENT_B" 2>/dev/null
wait "$AGENT_B" 2>/dev/null
AGENT_B=""
# Start A only after host B's silence has passed the stale window, so the requeue is what hands
# the job over. This is the 45 second StaleAfter in the coordinator.
sleep 50
start_a

wait_for "$long_id" status succeeded 120
final=$(job_field "$long_id" status)
new_agent=$(job_field "$long_id" agent)
report "failover: the job from the dead host finishes on host-a (status=$final agent=$new_agent)" \
    "$([ "$final" = "succeeded" ] && [ "$new_agent" = "host-a" ] && echo yes || echo no)"

out=$(curl -s --cacert "$CA" -H "Authorization: Bearer $TOKEN" "https://127.0.0.1:$PORT/v1/jobs/$long_id")
report "failover: the output comes from the second host" \
    "$(case "$out" in *finished-after-failover*) echo yes;; *) echo no;; esac)"

# ---- 3. both hosts run work when both are up ----

start_b
ids=()
for i in 1 2 3 4 5 6; do
    ids+=("$(submit '{"image":"ci","cmd":["sleep","2"],"net":"isolated"}')")
done
for id in "${ids[@]}"; do wait_for "$id" status succeeded 120 >/dev/null; done
ran_a=0; ran_b=0
for id in "${ids[@]}"; do
    case "$(job_field "$id" agent)" in
        host-a) ran_a=$((ran_a + 1)) ;;
        host-b) ran_b=$((ran_b + 1)) ;;
    esac
done
report "spread: work is shared across both hosts (host-a=$ran_a host-b=$ran_b)" \
    "$([ "$ran_a" -gt 0 ] && [ "$ran_b" -gt 0 ] && echo yes || echo no)"

metrics=$(curl -s --cacert "$CA" "https://127.0.0.1:$PORT/metrics")
report "metrics: two agents are counted online" \
    "$(case "$metrics" in *'minibox_agents_online 2'*) echo yes;; *) echo no;; esac)"

echo
echo "passed: $pass  failed: $fail"
[ "$fail" -eq 0 ]
