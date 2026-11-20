#!/usr/bin/env bash
# Smoke tests for minibox. Run as root inside Linux (WSL2 works).
#   sudo ./tests/smoke.sh
#   BIN=/path/to/minibox ROOTFS=/path/to/rootfs sudo ./tests/smoke.sh
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
BIN="${BIN:-$HERE/../target/release/minibox}"
ROOTFS="${ROOTFS:-/opt/minibox-rootfs}"
STATE="$(mktemp -d /tmp/mb-state.XXXXXX)"
export MINIBOX_STATE_DIR="$STATE"
pass=0
fail=0

if [ "$(id -u)" -ne 0 ]; then
    echo "run as root (sudo)"
    exit 2
fi
if [ ! -x "$BIN" ]; then
    echo "binary not found at $BIN (run: cargo build --release)"
    exit 2
fi

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

cleanup() { rm -rf "$STATE" /tmp/mb-*.out; }
trap cleanup EXIT

# ---- milestone 1: namespaces, exit codes, cgroup limits ----

# PID 1 is minibox's init; the command is its child, so it is PID 2.
out=$("$BIN" run -- sh -c 'echo $$')
report "command runs as PID 2 under the init (got: $out)" "$([ "$out" = "2" ] && echo yes || echo no)"

out=$("$BIN" run -- sh -c 'tr "\0" " " < /proc/1/cmdline')
report "PID 1 is the minibox init process" "$(case "$out" in *minibox*) echo yes;; *) echo no;; esac)"

for ns in pid mnt uts ipc; do
    host=$(readlink "/proc/self/ns/$ns")
    inside=$("$BIN" run -- readlink "/proc/self/ns/$ns")
    report "$ns namespace is isolated" "$([ "$host" != "$inside" ] && echo yes || echo no)"
done

"$BIN" run -- sh -c 'exit 7'
report "exit code passes through (7)" "$([ $? -eq 7 ] && echo yes || echo no)"

"$BIN" run --mem 32 -- sh -c 'x=$(head -c 128M /dev/zero | tr "\0" a); echo ok' > /tmp/mb-mem.out 2>&1
rc=$?
report "memory cap kills over-allocation (exit $rc)" \
    "$([ $rc -ne 0 ] && ! grep -q '^ok$' /tmp/mb-mem.out && echo yes || echo no)"

# Bash is used here because dash exits when a background fork fails.
"$BIN" run --pids 6 -- bash -c 'for i in $(seq 20); do sleep 4 & done; sleep 4' > /dev/null 2>&1 &
sb_pid=$!
sleep 1
cg=$(ls -d /sys/fs/cgroup/minibox-* 2>/dev/null | head -n1)
if [ -n "$cg" ]; then
    current=$(cat "$cg/pids.current")
    refused=$(awk '$1=="max"{print $2}' "$cg/pids.events")
    report "pid cap enforced (current=$current, refusals=${refused:-0})" \
        "$([ "$current" -le 6 ] && [ "${refused:-0}" -gt 0 ] && echo yes || echo no)"
else
    report "pid cap enforced (no sandbox cgroup seen)" "no"
fi
wait "$sb_pid" 2>/dev/null

leftover=$(ls -d /sys/fs/cgroup/minibox-* 2>/dev/null | wc -l)
report "sandbox cgroups removed afterwards" "$([ "$leftover" -eq 0 ] && echo yes || echo no)"

# ---- milestone 2: root filesystem ----

if [ -x "$ROOTFS/bin/sh" ]; then
    out=$("$BIN" run --rootfs "$ROOTFS" -- sh -c 'cat /etc/hostname')
    report "rootfs: reads its own /etc/hostname" "$([ "$out" = "minibox-rootfs" ] && echo yes || echo no)"

    out=$("$BIN" run --rootfs "$ROOTFS" -- sh -c 'test -d /home && echo visible || echo hidden')
    report "rootfs: host /home is hidden" "$([ "$out" = "hidden" ] && echo yes || echo no)"

    out=$("$BIN" run --rootfs "$ROOTFS" -- sh -c 'test -e /dev/null && echo ok')
    report "rootfs: /dev/null is available" "$([ "$out" = "ok" ] && echo yes || echo no)"
else
    echo "SKIP  rootfs tests (no rootfs at $ROOTFS; run tests/make-rootfs.sh)"
fi

# ---- milestone 3: network ----

ifaces=$("$BIN" run -- sh -c 'tail -n +3 /proc/net/dev | cut -d: -f1 | tr -d " "' | tr '\n' ' ' | xargs)
report "network: only loopback inside by default (got: $ifaces)" "$([ "$ifaces" = "lo" ] && echo yes || echo no)"

host_ifaces=$("$BIN" run --net host -- sh -c 'tail -n +3 /proc/net/dev | cut -d: -f1 | tr -d " "' | wc -l)
report "network: --net host shares host interfaces" "$([ "$host_ifaces" -gt 1 ] && echo yes || echo no)"

# ---- milestone 4: hardening ----

out=$("$BIN" run -- sh -c 'grep CapBnd /proc/self/status | awk "{print \$2}"')
report "hardening: capability bounding set is empty (got $out)" "$([ "$out" = "0000000000000000" ] && echo yes || echo no)"

out=$("$BIN" run -- sh -c 'grep NoNewPrivs /proc/self/status | awk "{print \$2}"')
report "hardening: no_new_privs is set (got $out)" "$([ "$out" = "1" ] && echo yes || echo no)"

out=$("$BIN" run -- sh -c 'mount -t tmpfs none /mnt && echo mounted || echo denied' 2>/dev/null)
report "hardening: seccomp denies mount (got $out)" "$([ "$out" = "denied" ] && echo yes || echo no)"

out=$("$BIN" run -- sh -c 'ls /proc/self/status >/dev/null && (unshare --user true 2>/dev/null && echo allowed || echo denied)')
report "hardening: calls off the allow-list fail (unshare gave $out)" "$([ "$out" = "denied" ] && echo yes || echo no)"

out=$("$BIN" run -- sh -c 'echo ok | tr a-z A-Z && sort </dev/null && printf "%s\n" done')
report "hardening: ordinary tools still run under the allow-list (got $out)" "$([ "$out" = "OK
done" ] && echo yes || echo no)"

# ---- milestone 5: state records and signals ----

"$BIN" run -- sh -c 'exit 3' >/dev/null
latest=$(ls -t "$STATE"/*.json 2>/dev/null | head -n1)
report "state: record written with status exited and exit code 3" \
    "$(grep -q '"status": "exited"' "$latest" 2>/dev/null && grep -q '"exit_code": 3' "$latest" && echo yes || echo no)"

"$BIN" run -- sleep 30 &
sig_pid=$!
sleep 1
kill -TERM "$sig_pid"
start=$(date +%s)
wait "$sig_pid"
rc=$?
elapsed=$(( $(date +%s) - start ))
report "signals: one SIGTERM stops the sandbox command (rc=$rc, ${elapsed}s)" \
    "$([ "$elapsed" -le 5 ] && [ "$rc" -eq 143 ] && echo yes || echo no)"

# ---- fixes: PID 1 reaps orphans; stale cgroups are cleaned up ----

zombies=$("$BIN" run -- sh -c '(sleep 0 &); sleep 1; grep -l "^State:.*Z" /proc/[0-9]*/status 2>/dev/null | wc -l')
report "init: no zombie processes left behind (got $zombies)" "$([ "$zombies" -eq 0 ] && echo yes || echo no)"

mkdir -p /sys/fs/cgroup/minibox-999999 2>/dev/null
"$BIN" run --mem 32 -- true
report "gc: stale cgroup of a dead runner is removed" \
    "$([ ! -d /sys/fs/cgroup/minibox-999999 ] && echo yes || echo no)"

# The job's root is an unprivileged host user, not host root.
"$BIN" run -- sleep 4 &
run_pid=$!
sleep 1
job_uid=$(ps -eo uid,args | awk '$2=="sleep" && $3=="4" {print $1; exit}')
report "userns: the job runs as host uid 100000, not root (got ${job_uid:-none})" \
    "$([ "$job_uid" = "100000" ] && echo yes || echo no)"
wait "$run_pid" 2>/dev/null

# Killing the runner takes its job down with it.
"$BIN" run -- sleep 60 &
runner=$!
sleep 1
kill -9 "$runner"
sleep 1
left=$(ps -eo args | grep -c '^sleep 60$' || true)
report "runner death: the job is killed too (left: $left)" "$([ "$left" -eq 0 ] && echo yes || echo no)"

# ---- milestone 6: bridge networking ----

addr=$("$BIN" run --net bridge -- sh -c 'ip -4 -o addr show eth0 | awk "{print \$4}"')
report "bridge: sandbox gets an address on 10.88.0.0/24 (got $addr)" \
    "$(case "$addr" in 10.88.0.*/24) echo yes;; *) echo no;; esac)"

gw=$("$BIN" run --net bridge -- sh -c 'ip route | awk "/default/{print \$3}"')
report "bridge: default route points at the gateway (got $gw)" "$([ "$gw" = "10.88.0.1" ] && echo yes || echo no)"

net_ok=$(timeout 20 "$BIN" run --net bridge -- bash -c 'exec 3<>/dev/tcp/1.1.1.1/443 && echo connected' 2>/dev/null)
report "bridge: sandbox reaches the internet via NAT (got '$net_ok')" "$([ "$net_ok" = "connected" ] && echo yes || echo no)"

sleep 1
veth_left=$(ip -o link show type veth 2>/dev/null | grep -c "mb[0-9]" || true)
report "bridge: veth pairs are removed after the sandbox exits (left: $veth_left)" "$([ "$veth_left" -eq 0 ] && echo yes || echo no)"

# ---- milestone 7: volumes, environment, working directory ----

vol=$(mktemp -d /tmp/mb-vol.XXXXXX)
chmod 755 "$vol"
echo "from-host" > "$vol/hello.txt"
if [ -x "$ROOTFS/bin/sh" ]; then
    out=$("$BIN" run --rootfs "$ROOTFS" -v "$vol:/data" -- cat /data/hello.txt)
    report "volume: host file is visible in the sandbox" "$([ "$out" = "from-host" ] && echo yes || echo no)"

    "$BIN" run --rootfs "$ROOTFS" -v "$vol:/data:ro" -- sh -c 'echo x > /data/new.txt' 2>/dev/null
    report "volume: :ro mount refuses writes" "$([ ! -e "$vol/new.txt" ] && echo yes || echo no)"

    # Writes from the job arrive as uid 100000 on the host, so a writable volume must be owned by it.
    chown 100000:100000 "$vol"
    "$BIN" run --rootfs "$ROOTFS" -v "$vol:/data" -- sh -c 'echo sandbox > /data/from-sandbox.txt'
    report "volume: writes in a :rw mount reach the host" "$([ -f "$vol/from-sandbox.txt" ] && echo yes || echo no)"
fi
rm -rf "$vol"

# A folder owned by an ordinary host user is still writable by the job's root, and it stays
# owned by that user afterwards.
vol2=$(mktemp -d /tmp/mb-vol2.XXXXXX)
chmod 755 "$vol2"
chown 1000:1000 "$vol2"
echo "from-owner" > "$vol2/owner.txt"
chown 1000:1000 "$vol2/owner.txt"
if [ -x "$ROOTFS/bin/sh" ]; then
    out=$("$BIN" run --rootfs "$ROOTFS" -v "$vol2:/data" -- sh -c 'cat /data/owner.txt && echo job > /data/job.txt && echo done')
    report "volume: a folder owned by uid 1000 is readable and writable by the job (got '$(echo $out | tr '\n' ' ')')" \
        "$([ "$out" = "$(printf 'from-owner\ndone')" ] && [ -f "$vol2/job.txt" ] && echo yes || echo no)"
    owner=$(stat -c %u "$vol2")
    report "volume: the host folder keeps its owner after the job (uid $owner)" "$([ "$owner" = "1000" ] && echo yes || echo no)"
    jobfile_owner=$(stat -c %u "$vol2/job.txt" 2>/dev/null)
    report "volume: files the job creates belong to the folder's owner on the host (uid $jobfile_owner)" \
        "$([ "$jobfile_owner" = "1000" ] && echo yes || echo no)"
fi
rm -rf "$vol2"

out=$("$BIN" run -e GREETING=hello -- sh -c 'echo $GREETING')
report "env: -e variables are set in the sandbox (got $out)" "$([ "$out" = "hello" ] && echo yes || echo no)"

out=$(HOSTSECRET=leak "$BIN" run -- sh -c 'echo ${HOSTSECRET:-clean}')
report "env: host variables do not leak in (got $out)" "$([ "$out" = "clean" ] && echo yes || echo no)"

out=$("$BIN" run -w /tmp -- pwd)
report "workdir: -w sets the starting directory (got $out)" "$([ "$out" = "/tmp" ] && echo yes || echo no)"

# ---- milestone 8: images (overlay), timeouts, cpu limits, logs ----

IMG_DIR="$STATE/images"
if [ -x "$ROOTFS/bin/sh" ]; then
    tarball="$STATE/ci.tar"
    tar -C "$ROOTFS" -cf "$tarball" .
    MINIBOX_IMAGE_DIR="$IMG_DIR" "$BIN" image import ci "$tarball" >/dev/null
    report "image: import unpacks a tarball into the image store" "$([ -d "$IMG_DIR/ci" ] && echo yes || echo no)"

    out=$(MINIBOX_IMAGE_DIR="$IMG_DIR" "$BIN" run --image ci -- sh -c 'cat /etc/hostname')
    report "image: job boots from the imported image (got $out)" "$([ "$out" = "minibox-rootfs" ] && echo yes || echo no)"

    MINIBOX_IMAGE_DIR="$IMG_DIR" "$BIN" run --image ci -- sh -c 'mkdir -p /root && echo built > /root/artifact.txt' >/dev/null
    report "overlay: job writes do not change the image" "$([ ! -e "$IMG_DIR/ci/root/artifact.txt" ] && echo yes || echo no)"

    leftover=$(ls -d "$STATE"/run-* 2>/dev/null | wc -l)
    report "overlay: run directories are removed afterwards (left: $leftover)" "$([ "$leftover" -eq 0 ] && echo yes || echo no)"
else
    echo "SKIP  image tests (no rootfs at $ROOTFS)"
fi

if [ -x "$ROOTFS/bin/sh" ]; then
    good=$(sha256sum "$tarball" | cut -d' ' -f1)
    MINIBOX_IMAGE_DIR="$IMG_DIR" "$BIN" image pull ci-pulled "file://$tarball" --sha256 "$good" >/dev/null 2>&1
    report "pull: image downloads and passes its SHA-256 check" "$([ -d "$IMG_DIR/ci-pulled" ] && echo yes || echo no)"

    MINIBOX_IMAGE_DIR="$IMG_DIR" "$BIN" image pull ci-bad "file://$tarball" --sha256 "0000" >/dev/null 2>&1
    report "pull: a checksum mismatch is refused and nothing is imported" "$([ ! -e "$IMG_DIR/ci-bad" ] && echo yes || echo no)"
fi

start=$(date +%s)
"$BIN" run --timeout 2 -- sleep 30
rc=$?
elapsed=$(( $(date +%s) - start ))
report "timeout: job is killed after 2s with exit 124 (rc=$rc, ${elapsed}s)" \
    "$([ "$rc" -eq 124 ] && [ "$elapsed" -le 6 ] && echo yes || echo no)"

latest=$(grep -l '"timed_out"' "$STATE"/*.json 2>/dev/null | head -n1)
report "timeout: run is recorded as timed_out" "$([ -n "$latest" ] && echo yes || echo no)"

"$BIN" run --cpus 0.5 -- sleep 3 &
cpu_pid=$!
sleep 1
cpu=$(cat /sys/fs/cgroup/minibox-*/cpu.max 2>/dev/null | head -n1)
report "cpu: --cpus 0.5 sets cpu.max to 50000/100000 (got $cpu)" "$([ "$cpu" = "50000 100000" ] && echo yes || echo no)"
wait "$cpu_pid"

"$BIN" run -- sh -c 'echo to-stdout; echo to-stderr >&2' >/dev/null 2>&1
logfile=$(grep -l 'to-stderr' "$STATE"/*.log 2>/dev/null | head -n1)
report "logs: stdout and stderr are saved to the run's log" \
    "$([ -n "$logfile" ] && grep -q to-stdout "$logfile" && echo yes || echo no)"

id=$(basename "$logfile" .log)
out=$(MINIBOX_STATE_DIR="$STATE" "$HERE/../bin/mbctl" logs "$id" 2>/dev/null)
report "logs: mbctl logs prints the saved output" "$(case "$out" in *to-stdout*) echo yes;; *) echo no;; esac)"

echo
echo "passed: $pass  failed: $fail"
[ "$fail" -eq 0 ]
