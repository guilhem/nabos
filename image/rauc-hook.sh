#!/bin/sh
# Runs from the signed bundle, before RAUC writes either inactive slot.
set -eu
reject() { echo "NabOS update rejected: $*" >&2; exit 10; }
[ "${1:-}" = install-check ] || reject "unexpected hook"
# An install-check replaces RAUC's built-in compatible check.
case ${RAUC_SYSTEM_COMPATIBLE:-}:${RAUC_MF_COMPATIBLE:-} in
    nabos-zero-armv6:nabos-zero-armv6|nabos-nixos-zero-armv6:nabos-zero-armv6|\
    nabos-zero2-arm64:nabos-zero2-arm64|nabos-nixos-zero2-arm64:nabos-zero2-arm64) ;;
    *) reject "wrong target" ;;
esac

# Reject the earlier single-FAT layout; only /data may have grown.
partition() {
    [ "$(cat "/sys/class/block/mmcblk0p$1/start")" = "$2" ] &&
        [ "$(cat "/sys/class/block/mmcblk0p$1/size")" = "$3" ] ||
        reject "partition $1 has an incompatible layout"
}
boot_start=$(cat /sys/class/block/mmcblk0p1/start)
case $boot_start in 8192|532480) ;; *) reject "incompatible boot region" ;; esac
partition 1 "$boot_start" 524288
partition 2 1056768 12582912
partition 3 13639680 12582912
[ "$(cat /sys/class/block/mmcblk0p4/start)" = 26222592 ] &&
    [ "$(cat /sys/class/block/mmcblk0p4/size)" -ge 2097152 ] || reject "incompatible data partition"
[ "$(findmnt -n -o SOURCE,FSTYPE --mountpoint /data)" = '/dev/mmcblk0p4 ext4' ] &&
    [ ! -e /data/.volatile ] || reject "data partition is not persistent"

certificate=/data/rauc/ca.cert.pem
if [ -e "$certificate" ] || [ -L "$certificate" ]; then
    [ -f "$certificate" ] && [ -s "$certificate" ] && [ ! -L "$certificate" ] ||
        reject "invalid persistent trust anchor"
else
    [ -s /etc/rauc/ca.cert.pem ] || reject "missing existing trust anchor"
    install -d -m 0700 /data/rauc
    temporary=$(mktemp /data/rauc/.ca.cert.XXXXXX)
    trap 'rm -f "$temporary"' EXIT
    install -m 0600 /etc/rauc/ca.cert.pem "$temporary"
    sync -f "$temporary"
    # Link atomically without replacing an authority published in the meantime.
    ln "$temporary" "$certificate"
    rm -f "$temporary"
fi
# Also complete durability when retrying after an interrupted publication.
sync -f /data/rauc
