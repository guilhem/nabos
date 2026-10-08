#!/bin/sh
# Shared data-partition operations for the initrd and boot health check.
DISK=${NABOS_DISK:-/dev/mmcblk0}
SYSFS=${NABOS_SYSFS:-/sys}
DATA=${DISK}p4
# Used by the initrd and health scripts sourcing this library.
# shellcheck disable=SC2034
PERSIST="/etc/NetworkManager/system-connections /var/lib/NetworkManager /var/lib/systemd/timesync /var/lib/tagtagtag-sound /var/lib/nabos"
GROWN=/data/system/.data-grown
KMSG=${NABOS_KMSG:-/dev/kmsg}

log() { echo "nabos-persist: $*" >> "$KMSG" 2>/dev/null || echo "nabos-persist: $*" >&2; }

free_after_data() {
	name=${DISK##*/}
	total=$(cat "$SYSFS/class/block/$name/size") &&
		start=$(cat "$SYSFS/class/block/${name}p4/start") &&
		size=$(cat "$SYSFS/class/block/${name}p4/size") &&
		echo $((total - start - size))
}

grow_partition() {
	free=$(free_after_data) || return 1
	[ "$free" -gt 32768 ] || return 0
	log "extending the data partition by $free sectors"
	echo ',+' | sfdisk --no-reread --no-tell-kernel -q -N 4 "$DISK"
}

# Retry interrupted growth on the next boot; only mark a successful resize.
grow_data() {
	if grow_partition && partx -u --nr 4 "$DISK" && resize2fs "$DATA" && : > "$GROWN"; then
		log "data filesystem uses the whole card"
	else
		log "data growth failed, retrying on next boot"
	fi
}

mount_data() {
	e2fsck -p "$DATA"
	rc=$?
	if [ "$rc" -ge 4 ]; then
		log "e2fsck -p returned $rc, repairing"
		e2fsck -fy "$DATA"
		rc=$?
	fi
	[ "$rc" -lt 4 ] || return 1
	mount -t ext4 -o noatime "$DATA" /data
}

media_permissions() {
	[ ! -d /data/nabos/media/sounds ] ||
		find /data/nabos/media/sounds -type f -exec chmod 0640 '{}' +
}
