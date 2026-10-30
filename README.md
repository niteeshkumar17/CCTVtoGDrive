# CCTV to Google Drive

A CP Plus Wi-Fi camera records to its SD card, then uploads that footage to Google Drive by itself. There is no PC, phone, or other box left running at home. Later changes are done over the home Wi-Fi.

The camera is a CP Plus CP-E31A (Tuya / ezykam style). The board is an Ingenic T31L running Linux. Stock firmware records fragmented MP4 on the SD card and talks to the vendor cloud for the phone app. It cannot talk to Google Drive on its own. This project adds a small uploader on the camera without replacing the recording app.

## What we wanted

- Footage stored in the owner's Google Drive, not a paid camera-cloud plan.
- The camera uploads by itself. No always-on PC, tablet, or extra device at home.
- The phone app keeps working: live view, SD playback, and the existing timeline.
- If the camera is unplugged or taken, Drive already has the recent clips, not only the last finished multi-hour file.
- Old footage is deleted from Drive after `retention_hours`. Each person sets that to fit their own Drive plan.

## What did not work

**Pulling the stream from outside the house.**  
The camera serves a local RTSP stream (`port 5543`, path contains a device token) and ONVIF on port 8000. A PC on the same Wi-Fi can play it. The home connection is behind CGNAT, so a cloud VM cannot open a connection inbound to the camera. GitHub Actions was also the wrong tool: job time limits, minute quotas, and it is not meant to be a 24/7 video relay.

**Pointing the camera at Drive with stock settings.**  
Google Drive only accepts files through its HTTPS API with OAuth. There is no FTP, RTSP, or WebDAV address to type into the camera. The stock firmware only uploads to the vendor cloud.

**Recovering Tuya cloud keys from the Android app.**  
The phone app was run in an emulator and inspected with Frida. Signing material is decrypted briefly inside native libraries and is not a stable key sitting in memory. The emulator was also too slow (ARM app on an x86 PC). This path was abandoned. The useful footage is already on the SD card, so the cloud API was never required.

**Logging in over serial with the stock password.**  
UART pads on the board are labeled TX, RX, and GND (115200 8N1). The UART adapter does not power the board; the camera uses its own USB supply, and the adapter's power pin stays disconnected. TX and RX are crossed. The stock root password hash did not match common camera passwords, and `telnetd` is commented out in the stock boot script. An SD-card auto-run script was also ignored by this firmware.

**Stopping U-Boot from the keyboard.**  
U-Boot prints a 1-second "hit any key" line, but the RX wire contact was unreliable, so keystrokes often never arrived.

## What worked

**Boot a temporary U-Boot from the SD card.**  
Holding the board `BOOT` pad to GND at power-on made the SoC load U-Boot from the SD card instead of the locked stock bootloader. That gave a root shell with no password and a full dump of the flash chip onto the SD card (U-Boot, kernel, rootfs, app, and config).

**Patch only the root filesystem, then boot stock again.**  
The stock app, kernel, and config were left as they were. The rootfs squashfs was rebuilt with three changes:

- a chosen root password
- `telnetd` started on boot (port 23)
- a hook that runs `/conf/custom.sh` after `/conf` is mounted

`/conf` is a small JFFS2 partition (384 KB). It holds `custom.sh`, `gdrive.json`, and the upload ledger. The uploader binary does not fit there, so it lives on the SD card.

**Run the uploader on the camera.**  
`tools/cam_gdrive_sync.go` is a Go program built for `linux/mipsle` soft-float. It is packed with a CA certificate bundle into `tools.sqsh`. On boot, `custom.sh` loop-mounts that file at `/tmp/tools` and restarts the uploader if it exits.

After the patched rootfs is flashed, the serial wires are not needed. Telnet on the home Wi-Fi is enough to update the binary or the config.

## How it works

The stock app keeps recording 5-minute clips under:

```text
/mnt/sdcard/Media/YYYYMM/DDd/HHh/
```

The file being recorded right now is `/mnt/sdcard/Media/.cache.mp4`. The uploader never writes, deletes, or renames those files. The phone app's SD playback is unchanged.

Each poll the uploader:

1. Reads `/conf/gdrive.json` and refreshes a Google access token.
2. Scans the last 72 hours of clips on the SD card. Older folders on a card that already had months of video are ignored, so inserting a full 128 GB card does not upload that history.
3. For the current 3-hour window, uploads each finished 5-minute clip into `live_HH-HH/` as soon as the camera closes it.
4. For a finished 3-hour window, stream-merges the clips into one `.mp4` and uploads that. The merge only rewrites fragment sequence numbers and decode timestamps. It does not re-encode, and it does not need a second copy on the SD card.
5. Deletes that window's temporary `live_HH-HH/` folder in Drive after the merged file is in place.
6. About once an hour, deletes day folders inside `CCTV/` that are older than `retention_hours`. Nothing outside that folder is deleted.

Drive layout:

```text
CCTV/
└── 2026-10/
    └── 29/
        ├── 06-09.mp4
        ├── 09-12.mp4
        ├── 12-15.mp4
        └── live_12-15/          # only while 12:00–15:00 is still open
            └── 14h_05m41s_auto_300s_hd.mp4
```

The eight blocks each day are `00-03`, `03-06`, `06-09`, `09-12`, `12-15`, `15-18`, `18-21`, and `21-24`.

Clips are fragmented MP4 (`ftyp` + `moov` + `moof`/`mdat`, 1000 Hz timestamps). Google Drive's resumable upload is picky about the final chunk, so the uploader retries that commit and does not create a duplicate folder when Drive rate-limits a folder lookup.

Memory on the camera is about 32 MB. The uploader drops the Linux page cache while streaming and keeps a small heap limit so recording is not pushed into swap.

`retention_hours` is how long files stay in Drive. Set it from your own free space. This camera records about 4.5–5 GB a day, so `hours = (free GB / 5) × 24`. A 15 GB free account can hold about 3 days (`72`). A plan with a few hundred GB free can hold longer. The 72-hour SD-card scan is separate and is not controlled by this number.

## Layout of this repo

| Path | Role |
| --- | --- |
| `tools/cam_gdrive_sync.go` | Uploader that runs on the camera |
| `tools/build.sh` | Cross-compile and pack `tools.sqsh` |
| `tools/deploy_clean.py` | Copy `tools.sqsh` and `gdrive.json` to the camera over telnet |
| `tools/cam_exec.py` | Run one command on the camera |
| `tools/list_drive.py` | Print the `CCTV/` tree in Drive |
| `tools/patch_rootfs.sh` | Rebuild the rootfs with telnet and the `/conf/custom.sh` hook |
| `tools/gdrive.json.example` | Config template with no secrets |

These stay on the machine that deploys the camera and are listed in `.gitignore`:

- `tools/gdrive.json` and `tools/token_success.json` (Google refresh token)
- `tools/tools.sqsh` (built binary image)
- `firmware/` (local flash backup and vendor images)
- old app extracts, Frida scripts, serial logs, and screenshots

## Build and update

From WSL, with Go and `squashfs-tools`:

```bash
sudo bash tools/build.sh
```

Copy `tools/gdrive.json.example` to `tools/gdrive.json` and fill in the OAuth client, refresh token, and Drive folder id. Create those in Google Cloud for a Drive scope of `https://www.googleapis.com/auth/drive`, limited to the account that owns the CCTV folder. Prefer your own OAuth client. A widely shared public client is rate-limited and was the cause of upload retries.

On the PC, from the repo root, with the camera on the same Wi-Fi:

```bash
set CAM_PASS=your-root-password
python tools/deploy_clean.py
python tools/list_drive.py
```

`CAM_HOST` defaults to `192.168.1.35`.

The one-time patch for a matching CP Plus CP-E31A / Ingenic T31L is written out in [docs/cp-plus-patch.md](docs/cp-plus-patch.md). It uses your own root password and your own Google token. Nothing in that guide is this camera's login.

## Will this work on another camera?

No. Not as a download-and-flash package, and not on every CP Plus or Tuya camera.

It works on this unit because that unit is an Ingenic T31L whose flash is laid out like this:

| Image | Size |
| --- | --- |
| U-Boot | 256 KB |
| Kernel | 1,792 KB |
| Rootfs squashfs | 960 KB (must stay under 983,040 bytes after the patch) |
| App squashfs | 4,800 KB |
| Config JFFS2 (`/conf`) | 384 KB |
| Full chip backup | 8 MB |

Another owner has to dump **their** chip, confirm the same layout, and patch **their** rootfs. Flashing this camera's image into a different board can brick it. The stock app, kernel, and config on this camera were never replaced.

The phone-app and Frida work did not produce a working uploader. Putting that app online would not make the project succeed on another camera. Google Drive still needs each person's own OAuth client and refresh token. Copy `tools/gdrive.json.example` to `tools/gdrive.json` and fill it in locally. That file is gitignored.

## What is not in this public repo

These stay on the machine that owns the camera:

- `tools/gdrive.json` and `tools/token_success.json`. Those are a live Google login. Stripping them is the example file above. There is no safe way to publish the real token.
- The ONVIF password and Tuya user database inside the camera's `/conf` partition. Those are this camera's credentials.
- Stock and patched firmware binaries, kernel, rootfs, the vendor app, kernel modules, and the decoded phone app. Those are the vendor's copyrighted code, and the patched rootfs also contains this camera's root password. The partition sizes and the boot-script change are documented here so the same patch can be repeated on a matching dump.
- Serial logs and one-off password guesses. They were scratch work, and some of them contained secrets. They are not required to build or run the uploader.

The step that actually opens the camera is in `tools/patch_rootfs.sh`: enable the commented-out `telnetd` line in `etc/init.d/rcS`, start `/conf/custom.sh` after `/conf` is mounted, and replace the root hash. Stock `rcS` has telnet disabled:

```sh
# Start telnet daemon
#telnetd &
mount -t squashfs /dev/mtdblock3 /app
mount -t jffs2 -o sync /dev/mtdblock4 /conf
```

If your dumped `etc/shadow` uses the salt `soidjfoi`, it is the same rootfs family this patch was written for. You still choose your own `NEWPW`. Do not reuse a password you use anywhere else, and do not commit it.

## Precautions

**Wi-Fi, not the public internet.**  
The camera only has a private address. Nothing in this project opens a tunnel or a port forward. A host on the internet cannot open telnet to it. The phone app still works from outside, because the camera calls out to the vendor cloud the same way it did before. That path cannot use the telnet shell.

**Any device on this Wi-Fi can reach telnet.**  
Port 23 asks for the root password. The patched boot script also starts `telnetd -l /bin/sh -p 2323`, which is a root shell with no password. That was a recovery shortcut. On a shared network, disable port 2323 and keep only the password login. Do not put the password or `gdrive.json` in git.

**Do not factory-reset the camera, and do not format the SD card from the app.**  
Reset would remove the telnet patch only if the rootfs itself were restored; format would delete `/mnt/sdcard/bin/tools.sqsh`. The boot hook and config are on internal flash, so the binary can be copied back over Wi-Fi, but only while telnet still works.

**The uploader is outside the footage folder.**  
When the card is full, the stock app deletes old day folders with `rm -rf /mnt/sdcard/Media/YYYYMM/DDd`. It does not delete `/mnt/sdcard/bin/`. Leave the SD card inserted. The 128 GB card is the one in use.

**Reserve the camera's DHCP address** (this unit is `192.168.1.35`) so later updates still find it. If the address changes, scan the LAN for port 23.

**A sudden power loss can drop the clip that is still recording.**  
Drive only keeps an upload after the file is fully closed. Finished 5-minute clips are already in `live_HH-HH/`. The open `.cache.mp4` is not. Worst case is about one to five minutes missing at the moment the camera dies, plus whatever was mid-upload. This is not a zero-gap recorder.

**`/conf` is only 384 KB.**  
The ledger `/conf/uploaded_v2.log` is trimmed when it grows past 8 KB. Do not store the binary or long logs there.

**Serial work, if it is ever needed again.**  
Three wires only: camera TX to adapter RX, camera RX to adapter TX, GND to GND. Do not connect the adapter's positive pin. Remove the wires before closing the case so they cannot short. Normal updates do not need the case open.

**OAuth token.**  
`/conf/gdrive.json` on the camera contains the refresh token. Anyone with telnet can read it. Treat the camera like a logged-in Google session.
