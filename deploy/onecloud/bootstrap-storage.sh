#!/bin/sh
set -eu

DATA_UUID=5307332f-8a4a-4f5a-a6ef-508b876913c7
DATA_DEVICE=/dev/mmcblk0p1
DATA_MOUNT=/srv/onecloud-data
FSTAB_LINE="UUID=${DATA_UUID} ${DATA_MOUNT} ext4 noatime,nosymfollow,nofail,x-systemd.device-timeout=15s,errors=remount-ro 0 2"

if [ "$(id -u)" -ne 0 ]; then
	printf '%s\n' 'Run as root.' >&2
	exit 1
fi

actual_uuid=$(blkid -s UUID -o value "$DATA_DEVICE")
if [ "$actual_uuid" != "$DATA_UUID" ]; then
	printf 'Unexpected data volume UUID on %s: %s\n' "$DATA_DEVICE" "$actual_uuid" >&2
	exit 1
fi

mkdir -p /srv
if mountpoint -q "$DATA_MOUNT"; then
	mounted_source=$(findmnt -n -o SOURCE --target "$DATA_MOUNT")
	mounted_uuid=$(blkid -s UUID -o value "$mounted_source")
	if [ "$mounted_uuid" != "$DATA_UUID" ]; then
		printf 'Refusing to use unexpected mount at %s: %s\n' "$DATA_MOUNT" "$mounted_source" >&2
		exit 1
	fi
else
	if [ -e "$DATA_MOUNT" ] && [ "$(find "$DATA_MOUNT" -mindepth 1 -maxdepth 1 -print -quit)" ]; then
		printf 'Refusing to mount over a non-empty directory: %s\n' "$DATA_MOUNT" >&2
		exit 1
	fi
	mkdir -p "$DATA_MOUNT"
	entry=$(awk -v target="$DATA_MOUNT" '$2 == target { print; found = 1 } END { if (!found) exit 1 }' /etc/fstab 2>/dev/null || true)
	if [ -z "$entry" ]; then
		printf '%s\n' "$FSTAB_LINE" >> /etc/fstab
	elif [ "$entry" != "$FSTAB_LINE" ]; then
		printf 'Unexpected existing fstab entry for %s:\n%s\n' "$DATA_MOUNT" "$entry" >&2
		exit 1
	fi
	mount "$DATA_MOUNT"
fi

mounted_source=$(findmnt -n -o SOURCE --target "$DATA_MOUNT")
mounted_uuid=$(blkid -s UUID -o value "$mounted_source")
if [ "$mounted_uuid" != "$DATA_UUID" ]; then
	printf 'Data mount verification failed: expected %s, found %s\n' "$DATA_UUID" "$mounted_uuid" >&2
	exit 1
fi

getent group navidrome >/dev/null || groupadd --system navidrome
getent passwd navidrome >/dev/null || useradd --system --no-create-home --home-dir "$DATA_MOUNT/navidrome" --shell /usr/sbin/nologin --gid navidrome navidrome
getent group webdav >/dev/null || groupadd --system webdav
getent passwd webdav >/dev/null || useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin --gid webdav --groups navidrome webdav
usermod -a -G navidrome webdav

install -d -o root -g navidrome -m 2770 "$DATA_MOUNT"
install -d -o navidrome -g navidrome -m 2770 \
	"$DATA_MOUNT/music" \
	"$DATA_MOUNT/.cache" \
	"$DATA_MOUNT/.cache/navidrome" \
	"$DATA_MOUNT/.cache/audio" \
	"$DATA_MOUNT/navidrome" \
	"$DATA_MOUNT/worker-state"
install -d -o root -g root -m 0755 /etc/navidrome /etc/webdav

printf 'Mounted %s from UUID %s\n' "$DATA_MOUNT" "$DATA_UUID"
findmnt -n -o SOURCE,FSTYPE,OPTIONS,TARGET --target "$DATA_MOUNT"
