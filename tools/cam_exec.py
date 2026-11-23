"""Run one shell command on the camera over telnet.

Password and host come from the environment so they are not stored in git.

  set CAM_PASS=your-root-password
  python tools/cam_exec.py "id"
"""
import os
import socket
import sys
import time

HOST = os.environ.get("CAM_HOST", "192.168.1.35")
USER = os.environ.get("CAM_USER", "root")
PASS = os.environ.get("CAM_PASS", "")
cmd = sys.argv[1] if len(sys.argv) > 1 else "id"
timeout = float(sys.argv[2]) if len(sys.argv) > 2 else 30.0

if not PASS:
    raise SystemExit("Set CAM_PASS to the camera root password.")


def read_until(sock: socket.socket, markers: tuple[bytes, ...], wait: float) -> bytes:
    buf = b""
    deadline = time.time() + wait
    while time.time() < deadline:
        sock.settimeout(max(0.05, deadline - time.time()))
        try:
            c = sock.recv(4096)
            if not c:
                break
            buf += c
            lower = buf.lower()
            if any(m in lower for m in markers):
                break
        except socket.timeout:
            break
    return buf


s = socket.create_connection((HOST, 23), timeout=5)
try:
    read_until(s, (b"login:",), 2.5)
    s.sendall((USER + "\r\n").encode())
    read_until(s, (b"password:",), 2.5)
    s.sendall((PASS + "\r\n").encode())
    auth_out = read_until(s, (b"# ", b"$ ", b"login incorrect"), 2.5)
    if b"login incorrect" in auth_out.lower():
        print("Telnet login failed: Login incorrect", file=sys.stderr)
        sys.exit(1)

    s.sendall((cmd + "\nexit\n").encode())
    s.settimeout(timeout)
    out = b""
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            c = s.recv(4096)
            if not c:
                break
            out += c
        except socket.timeout:
            break
        except Exception:
            break
    print(out.decode("utf-8", "replace"))
finally:
    s.close()
