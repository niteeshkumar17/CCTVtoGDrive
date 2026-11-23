#!/bin/bash
set -euo pipefail

IN="${1:-/root/2_rootfs.bin}"
OUT="${2:-/root/rootfs_new.bin}"
: "${NEWPW:?Set NEWPW to the new root password}"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

unsquashfs -d "$WORK/rootfs" "$IN" >/dev/null
cd "$WORK/rootfs"

# 1. Set the chosen root password using a random MD5-crypt salt.
grep -q '^root:' etc/shadow || { echo "Missing root entry in etc/shadow"; exit 1; }
HASH=$(openssl passwd -1 "$NEWPW")
sed -i "s|^root:[^:]*:|root:${HASH}:|" etc/shadow
grep -Fq "root:${HASH}:" etc/shadow || { echo "Failed to update root password in etc/shadow"; exit 1; }

# 2. Enable telnetd on port 23 only, plus the /conf/custom.sh hook in rcS.
# Do not start a passwordless shell. Anyone on the LAN could use it.
grep -q '^#telnetd &' etc/init.d/rcS || grep -q '^telnetd &' etc/init.d/rcS || { echo "Expected telnetd line not found in etc/init.d/rcS"; exit 1; }
grep -q 'mount -t jffs2 -o sync /dev/mtdblock4 /conf' etc/init.d/rcS || { echo "Expected /conf mount line not found in etc/init.d/rcS"; exit 1; }

sed -i 's|^#telnetd &|telnetd \&|' etc/init.d/rcS
if ! grep -q '/conf/custom.sh' etc/init.d/rcS; then
  sed -i '/mount -t jffs2 -o sync \/dev\/mtdblock4 \/conf/a if [ -f /conf/custom.sh ]; then\n    /bin/sh /conf/custom.sh \&\nfi' etc/init.d/rcS
fi

grep -q '^telnetd &' etc/init.d/rcS || { echo "Failed to enable telnetd in etc/init.d/rcS"; exit 1; }
grep -q '/conf/custom.sh' etc/init.d/rcS || { echo "Failed to add /conf/custom.sh hook in etc/init.d/rcS"; exit 1; }

# Validate shell syntax of rcS
sh -n etc/init.d/rcS
echo "rcS syntax OK and root password updated"
echo "=== patched etc/init.d/rcS ==="
cat etc/init.d/rcS

# 3. Rebuild squashfs with exact original parameters
rm -f "$OUT"
mksquashfs "$WORK/rootfs" "$OUT" -comp xz -b 65536 -noappend -quiet
SIZE=$(stat -c%s "$OUT")
HEXSIZE=$(printf "0x%x" "$SIZE")
echo "size=$SIZE ($HEXSIZE) partition_max=983040 (0xf0000)"
if [ "$SIZE" -gt 983040 ]; then echo "TOO BIG"; exit 1; fi

sha256sum "$OUT"
echo "Patched image: $OUT"
