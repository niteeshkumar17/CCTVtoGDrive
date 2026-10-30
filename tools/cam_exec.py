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

s = socket.create_connection((HOST, 23), timeout=5)
s.settimeout(1.0)


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
s.sendall((USER + "\r\n").encode())
drain(0.4)
s.sendall((PASS + "\r\n").encode())
drain(0.5)
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
s.close()
print(out.decode("utf-8", "replace"))
