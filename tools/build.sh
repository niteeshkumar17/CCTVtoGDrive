#!/bin/bash
# Cross-build the uploader for the camera (Ingenic T31, mipsle soft-float)
# and pack it into a squashfs the camera loop-mounts from the SD card.
#
# Run from WSL with Go and squashfs-tools installed:
#   sudo bash tools/build.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
STAGE="${STAGE:-/tmp/cctv-sqtools}"
mkdir -p "$STAGE"

if [[ ! -f "$STAGE/cacert.pem" ]]; then
  curl -fsSL -o "$STAGE/cacert.pem" https://curl.se/ca/cacert.pem
fi
cp "$ROOT/tools/cam_gdrive_sync.go" "$STAGE/"

(
  cd "$STAGE"
  CGO_ENABLED=0 GOOS=linux GOARCH=mipsle GOMIPS=softfloat \
    go build -ldflags='-s -w' -o "$STAGE/cam_gdrive_sync" cam_gdrive_sync.go
)
chmod +x "$STAGE/cam_gdrive_sync"

rm -f "$ROOT/tools/tools.sqsh"
mksquashfs "$STAGE/cam_gdrive_sync" "$STAGE/cacert.pem" \
  "$ROOT/tools/tools.sqsh" -comp xz -b 65536 -noappend -quiet
ls -lh "$ROOT/tools/tools.sqsh"
echo "Built tools/tools.sqsh. Deploy with CAM_PASS set: python tools/deploy_clean.py"
