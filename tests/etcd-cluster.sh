#!/usr/bin/env bash
# Failover test for a three-node etcd cluster under the coordinator.
#
#   1. Start three etcd members on this machine and a coordinator and agent against them.
#   2. Run a job before any failure.
#   3. Kill the leader. The cluster should elect a new one and keep serving writes.
#   4. Kill a second member. Two of three are gone, so there is no quorum: writes must fail.
#   5. Restart both members. The cluster should recover and every earlier job should still be there.
#
#   sudo ./tests/etcd-cluster.sh
#
# Needs: etcd and etcdctl, bin/mbd, bin/mbagent, target/release/minibox, and a rootfs at $ROOTFS.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$HERE/.."
BIN="${BIN:-$ROOT/target/release/minibox}"
MBD="${MBD:-$ROOT/bin/mbd}"
MBAGENT="${MBAGENT:-$ROOT/bin/mbagent}"
ROOTFS="${ROOTFS:-/opt/minibox-rootfs}"
TOKEN="etcd-$$"
PORT=18791
URL="http://127.0.0.1:$PORT"
WORK="$(mktemp -d /tmp/mb-etcd.XXXXXX)"
EP=("http://127.0.0.1:23791" "http://127.0.0.1:23792" "http://127.0.0.1:23793")
PEER=("http://127.0.0.1:23801" "http://127.0.0.1:23802" "http://127.0.0.1:23803")
EPS="$(IFS=,; echo "${EP[*]}")"
INITIAL="m1=${PEER[0]},m2=${PEER[1]},m3=${PEER[2]}"
ETCD_PID=("" "" "")
MBD_PID=""
AGENT_PID=""
pass=0
fail=0

if [ "$(id -u)" -ne 0 ]; then
    echo "run as root (sudo)"
    exit 2
fi
for f in "$BIN" "$MBD" "$MBAGENT"; do
    [ -x "$f" ] || { echo "missing $f (build it first)"; exit 2; }
done
command -v etcd >/dev/null || { echo "etcd not installed"; exit 2; }
command -v etcdctl >/dev/null || { echo "etcdctl not installed"; exit 2; }

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

start_member() {
    # start_member INDEX (0, 1 or 2)
    local i="$1"
    etcd --name "m$((i + 1))" \
        --data-dir "$WORK/m$((i + 1))" \
        --listen-client-urls "${EP[$i]}" --advertise-client-urls "${EP[$i]}" \
        --listen-peer-urls "${PEER[$i]}" --initial-advertise-peer-urls "${PEER[$i]}" \
        --initial-cluster "$INITIAL" --initial-cluster-token mbtest --initial-cluster-state new \
        >"$WORK/etcd-m$((i + 1)).log" 2>&1 &
    ETCD_PID[$i]=$!
}

etcdctl_at() {
    ETCDCTL_API=3 etcdctl --endpoints="$1" "${@:2}"
}

wait_healthy() {
    # wait_healthy ENDPOINT... : wait until every listed endpoint answers
    local ep ok
    for _ in $(seq 100); do
        ok=yes
        for ep in "$@"; do
            etcdctl_at "$ep" endpoint health >/dev/null 2>&1 || ok=no
        done
        [ "$ok" = yes ] && return 0
        sleep 0.2
    done
    return 1
}

leader_index() {
    # Prints the index (0, 1, 2) of the current leader, or nothing if there is none.
    local leader
    leader=$(etcdctl_at "$EPS" endpoint status -w table 2>/dev/null | awk -F'|' '/true/ {gsub(/ /, "", $2); print $2; exit}')
    for i in 0 1 2; do
        [ "$leader" = "${EP[$i]}" ] && { echo "$i"; return; }
    done
}

cleanup() {
    [ -n "$AGENT_PID" ] && kill "$AGENT_PID" 2>/dev/null
    [ -n "$MBD_PID" ] && kill "$MBD_PID" 2>/dev/null
    for i in 0 1 2; do
        [ -n "${ETCD_PID[$i]}" ] && kill "${ETCD_PID[$i]}" 2>/dev/null
    done
    wait 2>/dev/null
    rm -rf "$WORK"
}
trap cleanup EXIT

# ---- 1. the cluster ----

for i in 0 1 2; do start_member "$i"; done
if wait_healthy "${EP[@]}"; then
    report "cluster: three members start and are healthy" yes
else
    report "cluster: three members start and are healthy" no
    exit 1
fi
members=$(etcdctl_at "$EPS" member list 2>/dev/null | wc -l)
report "cluster: all three members are in the membership list (got $members)" "$([ "$members" -eq 3 ] && echo yes || echo no)"

tar -C "$ROOTFS" -cf "$WORK/ci.tar" .
MINIBOX_IMAGE_DIR="$WORK/images" MINIBOX_STATE_DIR="$WORK/state" "$BIN" image import ci "$WORK/ci.tar" >/dev/null

AGENT_TOKEN="agent-$$"
echo "etcd-host $AGENT_TOKEN" > "$WORK/agents.txt"
"$MBD" --addr "127.0.0.1:$PORT" --token "$TOKEN" --agent-tokens "$WORK/agents.txt" --etcd "$EPS" --prefix "/etcd-$$" \
    >"$WORK/mbd.log" 2>&1 &
MBD_PID=$!
for _ in $(seq 50); do
    curl -s -o /dev/null "$URL/metrics" && break
    sleep 0.1
done

MINIBOX_IMAGE_DIR="$WORK/images" MINIBOX_STATE_DIR="$WORK/state" "$MBAGENT" \
    --server "$URL" --token "$AGENT_TOKEN" --name etcd-host --slots 2 --minibox "$BIN" --interval 500ms \
    >"$WORK/agent.log" 2>&1 &
AGENT_PID=$!

submit() {
    curl -s -w '\n%{http_code}' -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
        -d "$1" "$URL/v1/jobs" --max-time 15
}

submit_id() {
    submit "$1" | grep -o '"id": *"[^"]*"' | head -n1 | sed 's/.*"\([^"]*\)"$/\1/'
}

job_status() {
    curl -s -H "Authorization: Bearer $TOKEN" --max-time 10 "$URL/v1/jobs/$1" \
        | grep -o '"status": *"[^"]*"' | head -n1 | sed 's/.*"\([^"]*\)"$/\1/'
}

wait_status() {
    # wait_status JOB STATUS SECONDS
    local i
    for ((i = 0; i < $3 * 5; i++)); do
        [ "$(job_status "$1")" = "$2" ] && return 0
        sleep 0.2
    done
    return 1
}

JOB='{"image":"ci","cmd":["sh","-c","echo job-run"],"net":"isolated"}'

# ---- 2. a job before any failure ----

before=$(submit_id "$JOB")
if wait_status "$before" succeeded 60; then
    report "before failure: a job runs to completion on the three-node cluster" yes
else
    report "before failure: a job runs to completion on the three-node cluster" no
fi

# ---- 3. kill the leader ----

L=$(leader_index)
if [ -z "$L" ]; then
    report "failover: a leader exists before the failure" no
    exit 1
fi
kill -9 "${ETCD_PID[$L]}"
ETCD_PID[$L]=""
echo "killed leader m$((L + 1)) (${EP[$L]})"

# Give the survivors time to elect a new leader.
sleep 5
NEW_L=$(leader_index)
report "failover: a new leader is elected after the old one dies (leader now m$((${NEW_L:-0} + 1)))" \
    "$([ -n "$NEW_L" ] && [ "$NEW_L" != "$L" ] && echo yes || echo no)"

during=$(submit_id "$JOB")
if [ -n "$during" ] && wait_status "$during" succeeded 60; then
    report "failover: writes keep working with two of three members" yes
else
    report "failover: writes keep working with two of three members" no
fi

# ---- 4. kill a second member: no quorum ----

SECOND=""
for i in 0 1 2; do
    if [ "$i" != "$L" ] && [ -n "${ETCD_PID[$i]}" ]; then
        SECOND=$i
        break
    fi
done
kill -9 "${ETCD_PID[$SECOND]}"
ETCD_PID[$SECOND]=""
echo "killed member m$((SECOND + 1)) (${EP[$SECOND]}), one member left"
sleep 3

code=$(submit "$JOB" | tail -n1)
report "no quorum: a submit is refused with 503, not accepted (got HTTP $code)" \
    "$([ "$code" = "503" ] && echo yes || echo no)"

# ---- 5. bring both members back ----

start_member "$L"
start_member "$SECOND"
if wait_healthy "${EP[@]}"; then
    report "recovery: all three members are healthy again" yes
else
    report "recovery: all three members are healthy again" no
fi

after=$(submit_id "$JOB")
if [ -n "$after" ] && wait_status "$after" succeeded 60; then
    report "recovery: new jobs run again after the cluster recovers" yes
else
    report "recovery: new jobs run again after the cluster recovers" no
fi

for id in "$before" "$during"; do
    [ -n "$id" ] || continue
    st=$(job_status "$id")
    report "durable: job $id from before the failures is still there (status=$st)" \
        "$([ "$st" = "succeeded" ] && echo yes || echo no)"
done

echo
echo "passed: $pass  failed: $fail"
[ "$fail" -eq 0 ]
