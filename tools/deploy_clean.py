import hashlib
import http.server
import os
import pathlib
import secrets
import socket
import sys
import threading
import time

DIR = pathlib.Path(__file__).resolve().parent
HOST = os.environ.get("CAM_HOST", "192.168.1.35")
USER = os.environ.get("CAM_USER", "root")
PASS = os.environ.get("CAM_PASS", "")
DEPLOY_PORT = int(os.environ.get("DEPLOY_PORT", "8765"))

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
        tail -n 100 /tmp/gdrive_err.log >/tmp/gdrive_err.log.tmp 2>/dev/null && mv /tmp/gdrive_err.log.tmp /tmp/gdrive_err.log
        /tmp/tools/cam_gdrive_sync --log /tmp/gdrive.log /conf/gdrive.json >>/tmp/gdrive_err.log 2>&1
    fi
    sleep 20
done
"""


def make_deploy_handler(token: str, tools_bytes: bytes, gdrive_bytes: bytes):
    files = {
        f"/{token}/tools.sqsh": ("application/octet-stream", tools_bytes),
        f"/{token}/gdrive.json": ("application/json", gdrive_bytes),
    }

    class DeployHandler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            item = files.get(self.path)
            if item is None:
                self.send_error(404, "Not Found")
                return
            ctype, data = item
            self.send_response(200)
            self.send_header("Content-Type", ctype)
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def log_message(self, format, *args):
            pass

    return DeployHandler


def read_until(sock: socket.socket, markers: tuple[bytes, ...], timeout: float) -> bytes:
    buf = b""
    deadline = time.time() + timeout
    while time.time() < deadline:
        remaining = max(0.05, deadline - time.time())
        sock.settimeout(remaining)
        try:
            chunk = sock.recv(4096)
            if not chunk:
                break
            buf += chunk
            lower = buf.lower()
            if any(m in lower for m in markers):
                break
        except socket.timeout:
            break
    return buf


def main() -> int:
    if not PASS:
        raise SystemExit("Set CAM_PASS to the camera root password.")

    tools_sqsh = DIR / "tools.sqsh"
    gdrive_json = DIR / "gdrive.json"
    if not tools_sqsh.is_file():
        raise SystemExit(f"Missing {tools_sqsh}. Build it first with: bash tools/build.sh")
    if not gdrive_json.is_file():
        raise SystemExit(f"Missing {gdrive_json}. Copy tools/gdrive.json.example and fill in your credentials.")

    tools_bytes = tools_sqsh.read_bytes()
    gdrive_bytes = gdrive_json.read_bytes()
    pc_md5 = hashlib.md5(tools_bytes).hexdigest()
    print("Expected tools.sqsh MD5:", pc_md5)

    token = secrets.token_urlsafe(16)
    handler = make_deploy_handler(token, tools_bytes, gdrive_bytes)
    httpd = http.server.ThreadingHTTPServer(("0.0.0.0", DEPLOY_PORT), handler)
    actual_port = httpd.server_address[1]
    t = threading.Thread(target=httpd.serve_forever, daemon=True)
    t.start()

    s0 = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    try:
        s0.connect((HOST, 80))
        pc_ip = s0.getsockname()[0]
    finally:
        s0.close()

    sock = None
    try:
        sock = socket.create_connection((HOST, 23), timeout=10)
        read_until(sock, (b"login:",), 3.0)
        sock.sendall((USER + "\r\n").encode())
        read_until(sock, (b"password:",), 3.0)
        sock.sendall((PASS + "\r\n").encode())
        auth_out = read_until(sock, (b"# ", b"$ ", b"login incorrect"), 3.0)
        if b"login incorrect" in auth_out.lower():
            print("Telnet login failed: Login incorrect", file=sys.stderr)
            return 1

        base_url = f"http://{pc_ip}:{actual_port}/{token}"
        cmds = (
            "for p in $(ps | grep -E 'custom\\.sh|cam_gdrive_sync' | grep -v grep | sed -E 's/^ *([0-9]+).*/\\1/'); do kill -9 $p 2>/dev/null; done\n"
            "killall -9 cam_gdrive_sync 2>/dev/null || true\n"
            "sleep 1\n"
            "umount /tmp/tools 2>/dev/null || true\n"
            "umount /tmp/tools 2>/dev/null || true\n"
            "losetup -d /dev/loop0 /dev/loop1 /dev/loop2 2>/dev/null || true\n"
            "GDRIVE_OK=0\n"
            "SQSH_OK=0\n"
            f"if wget -O /tmp/gdrive.json.new {base_url}/gdrive.json && [ -s /tmp/gdrive.json.new ]; then\n"
            "  mv /tmp/gdrive.json.new /conf/gdrive.json\n"
            "  GDRIVE_OK=1\n"
            "else\n"
            "  rm -f /tmp/gdrive.json.new\n"
            "  echo DEPLOY_GDRIVE_FAIL\n"
            "fi\n"
            f"if wget -O /tmp/tools.sqsh.new {base_url}/tools.sqsh && md5sum /tmp/tools.sqsh.new | grep -q \"^{pc_md5} \"; then\n"
            "  mkdir -p /mnt/sdcard/bin\n"
            "  cp /tmp/tools.sqsh.new /mnt/sdcard/bin/tools.sqsh\n"
            "  rm -f /tmp/tools.sqsh.new\n"
            "  SQSH_OK=1\n"
            "  echo DEPLOY_MD5_OK\n"
            "else\n"
            "  rm -f /tmp/tools.sqsh.new\n"
            "  echo DEPLOY_MD5_FAIL\n"
            "fi\n"
            "if [ \"$GDRIVE_OK\" = \"1\" ] && [ \"$SQSH_OK\" = \"1\" ]; then\n"
            "  md5sum /mnt/sdcard/bin/tools.sqsh\n"
            "  mkdir -p /tmp/tools\n"
            "  mount -t squashfs -o loop /mnt/sdcard/bin/tools.sqsh /tmp/tools\n"
            "  /tmp/tools/cam_gdrive_sync --selftest /tmp/tools/cacert.pem\n"
            "  cat << 'EOF' > /conf/custom.sh\n"
            + CUSTOM_SH
            + "EOF\n"
            "  chmod +x /conf/custom.sh\n"
            "  grep retention_hours /conf/gdrive.json\n"
            "elif [ -f /mnt/sdcard/bin/tools.sqsh ]; then\n"
            "  mkdir -p /tmp/tools\n"
            "  mount -t squashfs -o loop /mnt/sdcard/bin/tools.sqsh /tmp/tools 2>/dev/null || true\n"
            "fi\n"
            "rm -rf /tmp/custom_sh.lock\n"
            "sync\n"
            "if [ -f /conf/custom.sh ]; then\n"
            "  setsid /bin/sh /conf/custom.sh </dev/null >/dev/null 2>&1 &\n"
            "fi\n"
            "sleep 2\n"
            "ps | grep -E 'custom|cam_gdrive' | grep -v grep\n"
            "exit\n"
        )
        sock.sendall(cmds.encode())
        sock.settimeout(45.0)
        out = b""
        deadline = time.time() + 45.0
        while time.time() < deadline:
            try:
                c = sock.recv(4096)
                if not c:
                    break
                out += c
            except Exception:
                break
        text = out.decode("utf-8", "replace")
        print(text)

        lines = [ln.strip() for ln in text.splitlines()]
        if "DEPLOY_GDRIVE_FAIL" in lines or "DEPLOY_MD5_FAIL" in lines:
            print("Deploy failed: download or MD5 verification failed.", file=sys.stderr)
            return 1
        if "DEPLOY_MD5_OK" not in lines or not any(ln.startswith("SELFTEST_OK ") for ln in lines):
            print("Deploy failed: missing DEPLOY_MD5_OK or SELFTEST_OK.", file=sys.stderr)
            return 1
        return 0
    finally:
        if sock is not None:
            sock.close()
        httpd.shutdown()
        httpd.server_close()


if __name__ == "__main__":
    sys.exit(main())
