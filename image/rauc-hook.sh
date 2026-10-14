#!/bin/sh
# Runs from the signed bundle, before RAUC writes either inactive slot.
set -eu
set -f
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
    if [ "$(cat "/sys/class/block/mmcblk0p$1/start")" != "$2" ] ||
        [ "$(cat "/sys/class/block/mmcblk0p$1/size")" != "$3" ]; then
        reject "partition $1 has an incompatible layout"
    fi
}
boot_start=$(cat /sys/class/block/mmcblk0p1/start)
case $boot_start in 8192|532480) ;; *) reject "incompatible boot region" ;; esac
partition 1 "$boot_start" 524288
partition 2 1056768 12582912
partition 3 13639680 12582912
[ "$(cat /sys/class/block/mmcblk0p4/start)" = 26222592 ] || reject "incompatible data partition"
[ "$(cat /sys/class/block/mmcblk0p4/size)" -ge 2097152 ] || reject "incompatible data partition"
[ "$(findmnt -n -o SOURCE,FSTYPE --mountpoint /data)" = '/dev/mmcblk0p4 ext4' ] ||
    reject "data partition is not persistent"
[ ! -e /data/.volatile ] || reject "data partition is not persistent"

certificate=/data/rauc/ca.cert.pem
if [ -e /usr/share/nabos/ca.cert.pem ] || [ -L /usr/share/nabos/ca.cert.pem ]; then
    [ -f /usr/share/nabos/ca.cert.pem ] && [ -s /usr/share/nabos/ca.cert.pem ] &&
        [ ! -L /usr/share/nabos/ca.cert.pem ] || reject "invalid image trust anchor"
elif [ -e "$certificate" ] || [ -L "$certificate" ]; then
    if [ ! -f "$certificate" ] || [ ! -s "$certificate" ] || [ -L "$certificate" ]; then
        reject "invalid persistent trust anchor"
    fi
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
# Older slots still need their existing authority for rollback before a reset.
if [ -f "$certificate" ]; then sync -f /data/rauc; fi

running=
seen=false
# Kernel arguments are words; globbing is disabled.
# shellcheck disable=SC2013
for parameter in $(cat /proc/cmdline); do
    case $parameter in
        rauc.slot=*)
            ! "$seen" || reject "ambiguous running slot"
            seen=true
            running=${parameter#rauc.slot=}
            case $running in A|B) ;; *) reject "invalid running slot" ;; esac
            ;;
    esac
done
[ -n "$running" ] || reject "missing running slot"
transaction=$(mktemp /run/nabos-rollback.XXXXXX)
trap 'rm -f "$transaction" "$transaction.errors"' EXIT
printf 'NABOS_ROLLBACK_SLOT %s\nnabos_recovery_action\n' "$running" > "$transaction"
fw_setenv -s "$transaction" 2> "$transaction.errors" || reject "cannot record running slot"
# fw_setenv can report fsync errors while returning zero.
if [ -s "$transaction.errors" ]; then
    cat "$transaction.errors" >&2
    reject "running slot write reported an error"
fi
[ "$(fw_printenv -n NABOS_ROLLBACK_SLOT)" = "$running" ] || reject "running slot was not persisted"
