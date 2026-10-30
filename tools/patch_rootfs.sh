#!/bin/bash
set -e
rm -rf /root/rootfs_new
unsquashfs -d /root/rootfs_new /root/2_rootfs.bin >/dev/null
cd /root/rootfs_new

# NEWPW is the root password you choose. Do not commit it.
: "${NEWPW:?Set NEWPW to the new root password}"
HASH=$(openssl passwd -1 -salt soidjfoi "$NEWPW")

# 1. Set the chosen root password (same salt as the stock shadow entry)
sed -i "s|^root:[^:]*:|root:${HASH}:|" etc/shadow

# 2. Enable telnetd (port 23 login + port 2323 direct root shell) and /conf/custom.sh hook in rcS
sed -i 's|^#telnetd &|telnetd \&\ntelnetd -l /bin/sh -p 2323 \&|' etc/init.d/rcS
sed -i '/mount -t jffs2 -o sync \/dev\/mtdblock4 \/conf/a if [ -f /conf/custom.sh ]; then\n    /bin/sh /conf/custom.sh \&\nfi' etc/init.d/rcS

# Validate shell syntax of rcS
sh -n etc/init.d/rcS
echo "rcS syntax OK"

echo "=== patched etc/shadow ==="
cat etc/shadow
echo "=== patched etc/init.d/rcS ==="
cat etc/init.d/rcS

# 3. Rebuild squashfs with exact original parameters
cd /root
rm -f /root/rootfs_new.bin
mksquashfs /root/rootfs_new /root/rootfs_new.bin -comp xz -b 65536 -noappend -quiet
SIZE=$(stat -c%s /root/rootfs_new.bin)
HEXSIZE=$(printf "0x%x" "$SIZE")
echo "size=$SIZE ($HEXSIZE) partition_max=983040 (0xf0000)"
if [ "$SIZE" -gt 983040 ]; then echo "TOO BIG"; exit 1; fi

sha256sum /root/rootfs_new.bin
echo "Patched image: /root/rootfs_new.bin"
