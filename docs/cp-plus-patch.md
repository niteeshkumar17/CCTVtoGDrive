# Patch a matching CP Plus camera

This is the procedure that worked on a CP Plus CP-E31A with an Ingenic T31L. It leaves the stock recorder in place and adds a Drive uploader. Use it only after a dump of your own chip matches the layout below. A different CP Plus model can have a different chip, different pads, or different partition sizes. Stop if it does not match. Do not flash anybody else's image.

Your Google refresh token, root password, Wi-Fi password, and ONVIF password stay on your machine. Copy `tools/gdrive.json.example` to `tools/gdrive.json` and fill in your own OAuth client, refresh token, and Drive folder id. That file is gitignored.

## Flash layout on the camera this was built for

An 8 MB dump splits like this:

| Offset | Size | Contents |
| --- | --- | --- |
| `0x000000` | 256 KB (`0x40000`) | U-Boot |
| `0x040000` | 1,792 KB (`0x1C0000`) | Kernel |
| `0x200000` | 960 KB (`0xF0000`) | Rootfs squashfs. This is the only slice you patch. |
| `0x2F0000` | 4,800 KB (`0x4B0000`) | App. Do not write this. |
| `0x7A0000` | 384 KB (`0x60000`) | Config JFFS2, mounted at `/conf`. Do not erase this. |

Split your dump and check those sizes before changing anything. The patched rootfs must stay at or under 983,040 bytes. `tools/patch_rootfs.sh` exits if it grows past that.

## 1. Get a shell without the stock password

The stock root password was not cracked. Telnet is commented out in stock `rcS`. What worked:

1. Power the camera from its own USB supply. A UART adapter does not power the board, and its positive pin stays disconnected.
2. Connect three wires only: camera TX to adapter RX, camera RX to adapter TX, ground to ground. The console is 115200 8N1.
3. Put a T31 SD-card U-Boot on the card (a Thingino MSC build was used here). Hold the `BOOT` pad to ground while applying power so the SoC loads that U-Boot instead of the locked stock bootloader.
4. From that shell, read the whole 8 MB flash onto the SD card. Keep that file. It is your restore image. Do not commit it.

If `BOOT` is not the same pad, or the console is not 115200, this is not the same board. Stop.

## 2. Patch your rootfs

On a Linux machine with `unsquashfs`, `mksquashfs`, and `openssl`:

```bash
cp your-rootfs-slice /root/2_rootfs.bin
export NEWPW='choose-a-password-you-do-not-reuse'
sudo -E bash tools/patch_rootfs.sh
```

The script only does three things:

- Sets the root password from `NEWPW`.
- Changes `#telnetd &` to `telnetd &` so port 23 asks for that password.
- Runs `/conf/custom.sh` after `/conf` is mounted.

It does not open a passwordless shell. Do not add one. Anyone on your Wi-Fi could use it.

Write the new image back to offset `0x200000` only, length `0xF0000`. Read that range back and compare it with the file before you reboot. Do not erase the app or config slices. If the compare fails, write your original rootfs slice back to the same offset.

After a normal boot, telnet to the camera on port 23 as `root` with `NEWPW`. The serial wires can come off. Later updates are over the home Wi-Fi. The camera still has only a private address. This patch does not open it to the internet. The phone app keeps working, because the stock app was not replaced.

## 3. Build and install the uploader

The uploader does not fit in the 384 KB config partition, so it lives on the SD card.

```bash
sudo bash tools/build.sh
copy tools\gdrive.json.example tools\gdrive.json
```

Edit `tools/gdrive.json` with your client id, client secret, refresh token, and the Drive folder id. Set `retention_hours` from your own free space. The camera records about 4.5–5 GB a day, so `hours = (free GB / 5) × 24`. The `2160` in the example is 90 days and only fits a large plan. A 15 GB free account should use about `72`. Use your own Google Cloud OAuth client. A shared public client gets rate-limited.

From the PC on the same LAN:

```bash
set CAM_HOST=192.168.1.35
set CAM_PASS=the-password-you-chose
python tools/deploy_clean.py
```

That copies `tools.sqsh` to `/mnt/sdcard/bin/tools.sqsh`, copies your config to `/conf/gdrive.json`, and installs `/conf/custom.sh`. The script loop-mounts the squashfs at `/tmp/tools` and keeps one uploader running.

`/mnt/sdcard/bin/` is outside `/mnt/sdcard/Media`. When the card fills up, the stock app deletes old day folders under `Media/` only. Formatting the card from the app does delete `tools.sqsh`. Telnet still works after a format, so `deploy_clean.py` can copy it back.

## Memory

This camera has about 32 MB of RAM, and recording already uses most of it. The uploader is built so a video is never held in memory:

- The Go heap is capped at 5 MB, and garbage collection runs early.
- Uploads reuse one 512 KB buffer. The file is read from the SD card, not slurped into RAM.
- Linux page cache is dropped about every 2 MB of reading, so the video does not sit in cache either.
- A finished 3-hour block is merged while it is uploaded. There is no second copy on the SD card and no re-encode.
- The directory walk looks at the last 72 hours only, so a full 128 GB card is not loaded into the process.
- `/conf/uploaded_v2.log` is trimmed when it passes 8 KB, so the small config partition does not fill up.
- `custom.sh` takes a lock, so a second copy exits instead of stacking. If the uploader quits, the loop starts it again after 20 seconds.

Watch it with `free` over telnet while a clip uploads. `cam_gdrive_sync` should stay near that 5 MB cap, and the camera should keep recording. If your board has less free RAM than this one, do not raise the heap limit to "make uploads faster."

## What you still do yourself

- Confirm the chip and the five partition sizes.
- Dump and restore your own flash. This repo does not contain a firmware image.
- Create your own Google token. This repo does not contain one.
- Choose your own root password and keep it out of git.
