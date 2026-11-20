#!/usr/bin/env bash
# Pulls a real image from Docker Hub and runs it. Needs internet access.
#   sudo ./tests/registry.sh
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
BIN="${BIN:-$HERE/../target/release/minibox}"
WORK="$(mktemp -d /tmp/mb-registry.XXXXXX)"
export MINIBOX_IMAGE_DIR="$WORK/images"
export MINIBOX_STATE_DIR="$WORK/state"
pass=0
fail=0

if [ "$(id -u)" -ne 0 ]; then
    echo "run as root (sudo)"
    exit 2
fi
trap 'rm -rf "$WORK"' EXIT

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

"$BIN" image pull-registry alpine alpine:3.20 >"$WORK/pull.log" 2>&1
report "pull: alpine:3.20 downloads from Docker Hub and passes its digest checks" \
    "$([ -d "$MINIBOX_IMAGE_DIR/alpine" ] && echo yes || echo no)"

out=$("$BIN" run --image alpine -- cat /etc/alpine-release 2>/dev/null)
report "run: the pulled image boots and reports its release (got $out)" \
    "$(case "$out" in 3.20*) echo yes;; *) echo no;; esac)"

out=$("$BIN" run --image alpine -- sh -c 'id -u')
report "run: the job is root inside its own namespace (got $out)" "$([ "$out" = "0" ] && echo yes || echo no)"

"$BIN" image pull-registry alpine alpine:3.20 >/dev/null 2>&1
report "pull: pulling over an existing image is refused" "$([ $? -ne 0 ] && echo yes || echo no)"

"$BIN" image pull-registry nope-$$ no-such-repo-$$/nothing:latest >/dev/null 2>&1
report "pull: a missing image fails cleanly and leaves nothing behind" \
    "$([ ! -e "$MINIBOX_IMAGE_DIR/nope-$$" ] && ! ls -a "$MINIBOX_IMAGE_DIR" | grep -q '^\.pull-' && echo yes || echo no)"

echo
echo "passed: $pass  failed: $fail"
[ "$fail" -eq 0 ]
