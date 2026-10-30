import functools
import hashlib
import http.server
import os
import pathlib
import socket
import threading
import time

DIR = str(pathlib.Path(__file__).resolve().parent)
HOST = os.environ.get("CAM_HOST", "192.168.1.35")
PASS = os.environ.get("CAM_PASS", "")
if not PASS:
    raise SystemExit("Set CAM_PASS to the camera root password.")
pc_md5 = hashlib.md5(open(DIR + r"\tools.sqsh", "rb").read()).hexdigest()
print("Expected tools.sqsh MD5:", pc_md5)

handler = functools.partial(http.server.SimpleHTTPRequestHandler, directory=DIR)
httpd = http.server.ThreadingHTTPServer(("0.0.0.0", 8765), handler)
t = threading.Thread(target=httpd.serve_forever, daemon=True)
t.start()

s0 = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s0.connect((HOST, 80))
pc_ip = s0.getsockname()[0]
s0.close()

CUSTOM_SH = """#!/bin/sh
mkdir /tmp/custom_sh.lock 2>/dev/null || exit 0
sleep 30
mkdir -p /tmp/tools
while [ ! -f /mnt/sdcard/bin/tools.sqsh ]; do
    sleep 5
done
mount -t squashfs -o loop /mnt/sdcard/bin/tools.sqsh /tmp/tools 2>/dev/null
while true; do
    if [ -x /tmp/tools/cam_gdrive_sync ] && [ -f /conf/gdrive.json ]; then
        tail -n 200 /tmp/gdrive.log >/tmp/gdrive.log.tmp 2>/dev/null && mv /tmp/gdrive.log.tmp /tmp/gdrive.log
        /tmp/tools/cam_gdrive_sync /conf/gdrive.json >>/tmp/gdrive.log 2>&1
    fi
    sleep 20
done
"""

s = socket.create_connection((HOST, 23), timeout=10)
s.settimeout(1.5)


def drain(wait=0.5):
    time.sleep(wait)
    out = b""
    while True:
        try:
            c = s.recv(4096)
            if not c:
                break
            out += c
        except Exception:
            break
    return out


drain(0.4)
s.sendall(b"root\r\n")
drain(0.4)
s.sendall((PASS + "\r\n").encode())
drain(0.5)

cmds = (
    "for p in $(ps | grep -E 'custom\\.sh|cam_gdrive_sync' | grep -v grep | sed -E 's/^ *([0-9]+).*/\\1/'); do kill -9 $p 2>/dev/null; done\n"
    "killall -9 cam_gdrive_sync 2>/dev/null || true\n"
    "sleep 1\n"
    "umount /tmp/tools 2>/dev/null || true\n"
    "umount /tmp/tools 2>/dev/null || true\n"
    "losetup -d /dev/loop0 /dev/loop1 /dev/loop2 2>/dev/null || true\n"
    f"wget -O /conf/gdrive.json http://{pc_ip}:8765/gdrive.json\n"
    f"wget -O /tmp/tools.sqsh http://{pc_ip}:8765/tools.sqsh\n"
    "mkdir -p /mnt/sdcard/bin\n"
    "cp /tmp/tools.sqsh /mnt/sdcard/bin/tools.sqsh\n"
    "rm -f /tmp/tools.sqsh\n"
    "md5sum /mnt/sdcard/bin/tools.sqsh\n"
    "mkdir -p /tmp/tools\n"
    "mount -t squashfs -o loop /mnt/sdcard/bin/tools.sqsh /tmp/tools\n"
    "/tmp/tools/cam_gdrive_sync --selftest /tmp/tools/cacert.pem\n"
    "grep retention_hours /conf/gdrive.json\n"
    "rm -rf /tmp/custom_sh.lock\n"
    "sync\n"
    "setsid /bin/sh /conf/custom.sh </dev/null >/dev/null 2>&1 &\n"
    "sleep 2\n"
    "ps | grep -E 'custom|cam_gdrive' | grep -v grep\n"
    "exit\n"
)
s.sendall(cmds.encode())
s.settimeout(45.0)
out = b""
deadline = time.time() + 45.0
while time.time() < deadline:
    try:
        c = s.recv(4096)
        if not c:
            break
        out += c
    except Exception:
        break
s.close()
print(out.decode("utf-8", "replace"))
