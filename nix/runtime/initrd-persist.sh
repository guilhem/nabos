# shellcheck shell=sh
# Runs after the immutable /etc overlay and the separate /var tmpfs exist.
set -eu
# shellcheck disable=SC1090
. "$NABOS_PERSIST_LIB"

tries=0
while [ ! -b "$DATA" ] && [ "$tries" -lt 50 ]; do
    sleep 0.1
    tries=$((tries + 1))
done
mkdir -p /data
data_ready=false
if mount_data && mkdir -p /data/system && touch /data/system/.write-test; then
    rm /data/system/.write-test
    data_ready=true
else
    log "data partition unusable, /data is volatile for this boot"
    if mountpoint -q /data; then umount /data; fi
    mount -t tmpfs -o mode=0755,size=64M,nosuid,nodev tmpfs /data
    : > /data/.volatile
    mkdir -p /data/system
fi
# A shared initrd root cannot move a child mount; bind it into the real root.
mount --bind /data /sysroot/data
if "$data_ready"; then
    timeout --signal=KILL 3s systemctl --no-block start nabos-initrd-log.service || log "initrd log collector unavailable"
    timeout --signal=KILL 3s nabos-initrd-log journal || log "persistent journal unavailable"
    [ -e "$GROWN" ] || grow_data
fi

# Seed only once, atomically; never overwrite a device's profiles or preferences.
for path in $PERSIST; do
    store=/data/system$path
    if [ ! -d "$store" ]; then
        rm -rf "$store.seed"
        mkdir -p "${store%/*}"
        cp -a "$NABOS_STATE_SEEDS$path" "$store.seed"
        mv "$store.seed" "$store"
    fi
    # /etc's mount target is already present in the immutable metadata layer.
    case $path in /var/*) mkdir -p "/sysroot$path" ;; esac
    mount --bind "$store" "/sysroot$path"
done

id=/data/system/machine-id
if ! grep -Eqx '[0-9a-f]{32}' "$id" 2>/dev/null; then
    tr -d '-' < /proc/sys/kernel/random/uuid > "$id.new"
    chmod 0444 "$id.new"
    mv "$id.new" "$id"
fi
mount --bind "$id" /sysroot/etc/machine-id
mount -o remount,bind,ro /sysroot/etc/machine-id
# Keep the initrd's file and the real root's identity coherent at switch-root.
cp "$id" /etc/machine-id
media_permissions
umount /data
