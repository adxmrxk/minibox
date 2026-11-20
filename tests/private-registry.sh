#!/usr/bin/env bash
# Pulls from a private registry that needs a login over HTTPS, and checks the failure paths.
#   sudo ./tests/private-registry.sh
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
BIN="${BIN:-$HERE/../target/release/minibox}"
WORK="$(mktemp -d /tmp/mb-private.XXXXXX)"
PORT=15443
REF="localhost:$PORT/team/fake:latest"
export MINIBOX_IMAGE_DIR="$WORK/images"
export MINIBOX_STATE_DIR="$WORK/state"
pass=0
fail=0
REG_PID=""

if [ "$(id -u)" -ne 0 ]; then
    echo "run as root (sudo)"
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

cleanup() {
    [ -n "$REG_PID" ] && kill "$REG_PID" 2>/dev/null
    wait 2>/dev/null
    rm -rf "$WORK"
}
trap cleanup EXIT

# A one-file layer, and a certificate for localhost.
mkdir -p "$WORK/layer/etc" "$WORK/layer/bin"
echo "fake-1.0" > "$WORK/layer/etc/fake-release"
cp "$(command -v busybox)" "$WORK/layer/bin/busybox"
tar -C "$WORK/layer" -cf "$WORK/layer.tar" .
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -keyout "$WORK/reg.key" -out "$WORK/reg.crt" \
    -subj "/CN=localhost" -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" >/dev/null 2>&1

start_registry() {
    python3 "$HERE/fake-registry.py" --port "$PORT" --cert "$WORK/reg.crt" --key "$WORK/reg.key" \
        --layer "$WORK/layer.tar" --user alice --password s3cret "$@" >"$WORK/registry.log" 2>&1 &
    REG_PID=$!
    for _ in $(seq 50); do
        curl -s --cacert "$WORK/reg.crt" -o /dev/null "https://localhost:$PORT/v2/" && return 0
        sleep 0.1
    done
    return 1
}

stop_registry() {
    kill "$REG_PID" 2>/dev/null
    wait "$REG_PID" 2>/dev/null
    REG_PID=""
}

export MINIBOX_REGISTRY_CA="$WORK/reg.crt"

# ---- correct credentials ----
start_registry || { echo "registry did not start"; exit 1; }

unset MINIBOX_REGISTRY_USER MINIBOX_REGISTRY_PASSWORD
"$BIN" image pull-registry fake "$REF" >/dev/null 2>&1
report "login: a pull without credentials is refused" "$([ ! -d "$MINIBOX_IMAGE_DIR/fake" ] && echo yes || echo no)"

MINIBOX_REGISTRY_USER=alice MINIBOX_REGISTRY_PASSWORD=wrong "$BIN" image pull-registry fake "$REF" >/dev/null 2>&1
report "login: a wrong password is refused" "$([ ! -d "$MINIBOX_IMAGE_DIR/fake" ] && echo yes || echo no)"

MINIBOX_REGISTRY_USER=alice MINIBOX_REGISTRY_PASSWORD=s3cret "$BIN" image pull-registry fake "$REF" >/dev/null 2>&1
report "login: the right credentials pull the image" "$([ -d "$MINIBOX_IMAGE_DIR/fake" ] && echo yes || echo no)"

out=$("$BIN" run --image fake -- /bin/busybox cat /etc/fake-release 2>/dev/null)
report "run: the private image runs and has its file (got $out)" "$([ "$out" = "fake-1.0" ] && echo yes || echo no)"
stop_registry

# ---- a registry that serves a layer that does not match its digest ----
start_registry --corrupt || { echo "corrupt registry did not start"; exit 1; }
MINIBOX_REGISTRY_USER=alice MINIBOX_REGISTRY_PASSWORD=s3cret "$BIN" image pull-registry bad "$REF" >/dev/null 2>&1
report "integrity: a layer that fails its digest is never installed" "$([ ! -e "$MINIBOX_IMAGE_DIR/bad" ] && echo yes || echo no)"
report "integrity: nothing is left in the image store after the failure" \
    "$(ls -a "$MINIBOX_IMAGE_DIR" 2>/dev/null | grep -q '^\.pull-' && echo no || echo yes)"
stop_registry

echo
echo "passed: $pass  failed: $fail"
[ "$fail" -eq 0 ]
