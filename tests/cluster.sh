#!/usr/bin/env bash
# End-to-end test for the coordinator (mbd) and an agent (mbagent) on one machine.
#   sudo ./tests/cluster.sh
# Needs: bin/mbd, bin/mbagent, target/release/minibox, a rootfs at $ROOTFS,
# and an etcd server at $ETCD_URL (default http://127.0.0.1:2379).
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$HERE/.."
BIN="${BIN:-$ROOT/target/release/minibox}"
MBD="${MBD:-$ROOT/bin/mbd}"
MBAGENT="${MBAGENT:-$ROOT/bin/mbagent}"
ROOTFS="${ROOTFS:-/opt/minibox-rootfs}"
PORT=18787
TOKEN="test-token-$$"
URL="http://127.0.0.1:$PORT"
WORK="$(mktemp -d /tmp/mb-cluster.XXXXXX)"
pass=0
fail=0

if [ "$(id -u)" -ne 0 ]; then
    echo "run as root (sudo)"
    exit 2
fi
for f in "$BIN" "$MBD" "$MBAGENT"; do
    [ -x "$f" ] || { echo "missing $f (build it first)"; exit 2; }
done
if [ ! -x "$ROOTFS/bin/sh" ]; then
    echo "missing rootfs at $ROOTFS (run tests/make-rootfs.sh)"
    exit 2
fi

export MINIBOX_STATE_DIR="$WORK/state"
export MINIBOX_IMAGE_DIR="$WORK/images"

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
    [ -n "${AGENT_PID:-}" ] && kill "$AGENT_PID" 2>/dev/null
    [ -n "${MBD_PID:-}" ] && kill "$MBD_PID" 2>/dev/null
    wait 2>/dev/null
    ETCDCTL_API=3 etcdctl --endpoints="$ETCD_URL" del --prefix "$ETCD_PREFIX" >/dev/null 2>&1
    rm -rf "$WORK"
}
trap cleanup EXIT

# Pull the rootfs into the image store, the same way a real host would.
tar -C "$ROOTFS" -cf "$WORK/ci.tar" .
"$BIN" image import ci "$WORK/ci.tar" >/dev/null

ETCD_URL="${ETCD_URL:-http://127.0.0.1:2379}"
ETCD_PREFIX="/cluster-$$"
AGENT_TOKEN="agent-$$"
echo "test-host $AGENT_TOKEN" > "$WORK/agents.txt"
"$MBD" --addr "127.0.0.1:$PORT" --token "$TOKEN" --agent-tokens "$WORK/agents.txt" \
    --etcd "$ETCD_URL" --prefix "$ETCD_PREFIX" >"$WORK/mbd.log" 2>&1 &
MBD_PID=$!
for _ in $(seq 50); do
    curl -s -o /dev/null "$URL/metrics" && break
    sleep 0.1
done

"$MBAGENT" --server "$URL" --token "$AGENT_TOKEN" --name test-host --slots 2 \
    --minibox "$BIN" --interval 500ms >"$WORK/agent.log" 2>&1 &
AGENT_PID=$!

# ---- auth ----

code=$(curl -s -o /dev/null -w '%{http_code}' "$URL/v1/jobs")
report "auth: requests without the token get 401 (got $code)" "$([ "$code" = "401" ] && echo yes || echo no)"

code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" "$URL/v1/jobs")
report "auth: the right token gets 200 (got $code)" "$([ "$code" = "200" ] && echo yes || echo no)"

code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $AGENT_TOKEN" "$URL/v1/jobs")
report "auth: an agent token cannot read or submit jobs (got $code)" "$([ "$code" = "401" ] && echo yes || echo no)"

code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $AGENT_TOKEN" \
    -H 'Content-Type: application/json' -d '{"name":"someone-else","slots":1,"running":0}' "$URL/v1/agents/poll")
report "auth: an agent cannot poll under another agent's name (got $code)" "$([ "$code" = "403" ] && echo yes || echo no)"

# ---- validation ----

code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" \
    -H 'Content-Type: application/json' -d '{"cmd":["true"]}' "$URL/v1/jobs")
report "validation: a job without an image is rejected with 400 (got $code)" "$([ "$code" = "400" ] && echo yes || echo no)"

# ---- a job that succeeds ----

submit() {
    curl -s -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
        -d "$1" "$URL/v1/jobs" | grep -o '"id": *"[^"]*"' | head -n1 | sed 's/.*"\([^"]*\)"$/\1/'
}

job_field() {
    curl -s -H "Authorization: Bearer $TOKEN" "$URL/v1/jobs/$1" | grep -o "\"$2\": *\"[^\"]*\"" | head -n1 | sed 's/.*: *"\([^"]*\)"$/\1/'
}

wait_for_final() {
    local id="$1" status=""
    for _ in $(seq 150); do
        status=$(job_field "$id" status)
        case "$status" in succeeded|failed|timed_out) break ;; esac
        sleep 0.2
    done
    echo "$status"
}

ok_id=$(submit '{"image":"ci","cmd":["sh","-c","echo hello-from-cluster"],"net":"isolated"}')
status=$(wait_for_final "$ok_id")
report "job: a normal job runs on the agent and succeeds (got $status)" "$([ "$status" = "succeeded" ] && echo yes || echo no)"

log=$(curl -s -H "Authorization: Bearer $TOKEN" "$URL/v1/jobs/$ok_id")
report "job: the output comes back with the result" "$(case "$log" in *hello-from-cluster*) echo yes;; *) echo no;; esac)"

# ---- a job that goes over its timeout ----

slow_id=$(submit '{"image":"ci","cmd":["sleep","30"],"timeout_secs":2}')
status=$(wait_for_final "$slow_id")
report "job: a job over its timeout is recorded as timed_out (got $status)" "$([ "$status" = "timed_out" ] && echo yes || echo no)"

# ---- a job that fails ----

bad_id=$(submit '{"image":"ci","cmd":["sh","-c","exit 3"]}')
status=$(wait_for_final "$bad_id")
report "job: a failing job is recorded as failed (got $status)" "$([ "$status" = "failed" ] && echo yes || echo no)"

# ---- metrics ----

metrics=$(curl -s "$URL/metrics")
report "metrics: job counts are exported" "$(case "$metrics" in *'minibox_jobs{status="succeeded"} 1'*) echo yes;; *) echo no;; esac)"
report "metrics: the online agent's slots are exported" "$(case "$metrics" in *'minibox_agent_slots 2'*) echo yes;; *) echo no;; esac)"

echo
echo "passed: $pass  failed: $fail"
[ "$fail" -eq 0 ]
