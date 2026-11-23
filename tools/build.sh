#!/bin/bash
# Cross-build the uploader for the camera (Ingenic T31, mipsle soft-float)
# and pack it into a squashfs the camera loop-mounts from the SD card.
#
# Run from WSL with Go and squashfs-tools installed:
#   bash tools/build.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
STAGE="${STAGE:-/tmp/cctv-sqtools}"
mkdir -p "$STAGE"

if [[ ! -f "$STAGE/cacert.pem" ]] || [[ "${REFRESH_CA:-0}" == "1" ]] || [[ -n "$(find "$STAGE/cacert.pem" -mtime +30 2>/dev/null)" ]]; then
  curl -fsSL -o "$STAGE/cacert.pem.tmp" https://curl.se/ca/cacert.pem
  mv "$STAGE/cacert.pem.tmp" "$STAGE/cacert.pem"
fi

(
  cd "$ROOT/tools"
  CGO_ENABLED=0 GOOS=linux GOARCH=mipsle GOMIPS=softfloat \
    go build -ldflags='-s -w' -o "$STAGE/cam_gdrive_sync" .
)
chmod +x "$STAGE/cam_gdrive_sync"

rm -f "$ROOT/tools/tools.sqsh"
mksquashfs "$STAGE/cam_gdrive_sync" "$STAGE/cacert.pem" \
  "$ROOT/tools/tools.sqsh" -comp xz -b 65536 -all-root -noappend -quiet
ls -lh "$ROOT/tools/tools.sqsh"
echo "Built tools/tools.sqsh. Deploy with CAM_PASS set: python tools/deploy_clean.py"
