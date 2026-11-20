#!/usr/bin/env bash
# Build a minimal root filesystem from busybox for minibox --rootfs.
#   sudo ./tests/make-rootfs.sh [DEST]     (default DEST: /opt/minibox-rootfs)
# Requires the busybox-static package (apt install busybox-static).
set -eu

DEST="${1:-/opt/minibox-rootfs}"
BB="$(command -v busybox || true)"

if [ -z "$BB" ]; then
    echo "busybox not found. Install it with: apt install busybox-static"
    exit 2
fi
if [ "$(id -u)" -ne 0 ]; then
    echo "run as root (sudo)"
    exit 2
fi

rm -rf "$DEST"
mkdir -p "$DEST"/{bin,dev,etc,proc,tmp,root,sys}
cp "$BB" "$DEST/bin/busybox"
chmod 755 "$DEST/bin/busybox"

# Create a symlink for every busybox applet (sh, ls, cat, ...).
# Point them at the relative name "busybox": absolute host paths don't exist
# once minibox has pivoted into the rootfs.
"$DEST/bin/busybox" --install -s "$DEST/bin"
for link in "$DEST"/bin/*; do
    if [ -L "$link" ] && [ "$(basename "$link")" != "busybox" ]; then
        ln -sf busybox "$link"
    fi
done

echo "minibox-rootfs" > "$DEST/etc/hostname"
chmod 1777 "$DEST/tmp"

echo "rootfs ready at $DEST"
echo "try: sudo ./target/release/minibox run --rootfs $DEST -- sh -c 'ls /; cat /etc/hostname'"
