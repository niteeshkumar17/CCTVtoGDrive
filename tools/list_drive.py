import json
import pathlib
import time
import urllib.parse
import urllib.request

cfg = json.loads((pathlib.Path(__file__).with_name("gdrive.json")).read_text(encoding="utf-8"))
data = urllib.parse.urlencode(
    {
        "client_id": cfg["client_id"],
        "client_secret": cfg["client_secret"],
        "refresh_token": cfg["refresh_token"],
        "grant_type": "refresh_token",
    }
).encode()
with urllib.request.urlopen(urllib.request.Request("https://oauth2.googleapis.com/token", data=data)) as r:
    access_token = json.loads(r.read().decode())["access_token"]


def gget(url):
    for _ in range(120):
        try:
            req = urllib.request.Request(url, headers={"Authorization": "Bearer " + access_token})
            with urllib.request.urlopen(req, timeout=10) as r:
                return json.loads(r.read().decode())
        except Exception:
            time.sleep(0.5)
    print("WARN: gget exhausted retries for", url)
    return {}


def walk(parent_id, indent=""):
    q = urllib.parse.quote(f"'{parent_id}' in parents and trashed = false")
    res = gget(f"https://www.googleapis.com/drive/v3/files?pageSize=50&fields=files(id,name,mimeType,size)&q={q}")
    files = sorted(res.get("files", []), key=lambda x: x["name"])
    for f in files:
        if f["mimeType"] == "application/vnd.google-apps.folder":
            print(f"{indent}[Folder] {f['name']}/  (id={f['id']})")
            walk(f["id"], indent + "  ")
        else:
            mb = int(f.get("size", 0)) / 1048576
            print(f"{indent}{f['name']:36s}  {mb:6.2f} MB  (id={f['id']})")


print("Google Drive tree under CCTV/:")
walk(cfg["folder_id"], "  ")
