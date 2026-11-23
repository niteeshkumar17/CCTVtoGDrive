import json
import pathlib
import sys
import time
import urllib.error
import urllib.parse
import urllib.request


def fetch_access_token(cfg: dict) -> str:
    data = urllib.parse.urlencode(
        {
            "client_id": cfg["client_id"],
            "client_secret": cfg["client_secret"],
            "refresh_token": cfg["refresh_token"],
            "grant_type": "refresh_token",
        }
    ).encode()
    req = urllib.request.Request("https://oauth2.googleapis.com/token", data=data)
    with urllib.request.urlopen(req, timeout=15) as r:
        return json.loads(r.read().decode())["access_token"]


def gget(url: str, access_token: str) -> dict:
    for attempt in range(8):
        try:
            req = urllib.request.Request(url, headers={"Authorization": "Bearer " + access_token})
            with urllib.request.urlopen(req, timeout=15) as r:
                return json.loads(r.read().decode())
        except urllib.error.HTTPError as e:
            body = e.read(512).decode("utf-8", "replace")
            retryable_403 = e.code == 403 and ("rateLimitExceeded" in body or "userRateLimitExceeded" in body)
            if e.code in (429, 500, 502, 503, 504) or retryable_403:
                time.sleep(min(10.0, 0.5 * (2**attempt)))
                continue
            raise SystemExit(f"Google Drive API HTTP {e.code} for {url}: {body.strip()}")
        except Exception as e:
            if attempt == 7:
                print(f"WARN: gget failed for {url}: {e}", file=sys.stderr)
                return {}
            time.sleep(min(10.0, 0.5 * (2**attempt)))
    print("WARN: gget exhausted retries for", url, file=sys.stderr)
    return {}


def walk(parent_id: str, access_token: str, indent: str = "") -> None:
    q = urllib.parse.quote(f"'{parent_id}' in parents and trashed = false")
    files = []
    page_token = ""
    while True:
        url = (
            "https://www.googleapis.com/drive/v3/files"
            f"?pageSize=100&fields=nextPageToken,files(id,name,mimeType,size)&q={q}"
        )
        if page_token:
            url += "&pageToken=" + urllib.parse.quote(page_token)
        res = gget(url, access_token)
        files.extend(res.get("files", []))
        page_token = res.get("nextPageToken", "")
        if not page_token:
            break

    files.sort(key=lambda x: x["name"])
    for f in files:
        if f["mimeType"] == "application/vnd.google-apps.folder":
            print(f"{indent}[Folder] {f['name']}/  (id={f['id']})")
            walk(f["id"], access_token, indent + "  ")
        else:
            mb = int(f.get("size", 0)) / 1048576
            print(f"{indent}{f['name']:36s}  {mb:6.2f} MB  (id={f['id']})")


def main() -> None:
    cfg = json.loads((pathlib.Path(__file__).with_name("gdrive.json")).read_text(encoding="utf-8"))
    access_token = fetch_access_token(cfg)
    print("Google Drive tree under CCTV/:")
    walk(cfg["folder_id"], access_token, "  ")


if __name__ == "__main__":
    main()
