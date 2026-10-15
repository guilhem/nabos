#!/bin/sh
# Runs from the signed bundle, before RAUC writes either inactive slot.
set -eu
set -f
reject() { echo "NabOS update rejected: $*" >&2; exit 10; }
[ "${1:-}" = install-check ] || reject "unexpected hook"
# An install-check replaces RAUC's built-in compatible check.
case ${RAUC_SYSTEM_COMPATIBLE:-}:${RAUC_MF_COMPATIBLE:-} in
    nabos-native-rauc-zero-armv6:nabos-native-rauc-zero-armv6|\
    nabos-native-rauc-zero2-arm64:nabos-native-rauc-zero2-arm64) ;;
    *) reject "wrong target" ;;
esac

# Constants travel inside the signed bundle alongside this hook.
# shellcheck source=image/layout.sh
. "${0%/*}/layout.sh"
disk_name=${NABOS_DISK##*/}
# Only /data may have grown.
partition() {
    if [ "$(cat "/sys/class/block/${disk_name}p$1/start")" != "$(( $2 / NABOS_SECTOR_SIZE ))" ] ||
        [ "$(cat "/sys/class/block/${disk_name}p$1/size")" != "$(( $3 / NABOS_SECTOR_SIZE ))" ]; then
        reject "partition $1 has an incompatible layout"
    fi
}
boot_start=$(cat "/sys/class/block/${disk_name}p${NABOS_BOOT_PARTITION}/start")
boot_offset=$((boot_start * NABOS_SECTOR_SIZE))
[ "$boot_offset" = "$NABOS_BOOT_PRIMARY_OFFSET" ] ||
    [ "$boot_offset" = "$NABOS_BOOT_SECONDARY_OFFSET" ] || reject "incompatible boot region"
partition "$NABOS_BOOT_PARTITION" "$boot_offset" "$NABOS_BOOT_SIZE"
partition "$NABOS_ROOT_A_PARTITION" "$NABOS_ROOT_A_OFFSET" "$NABOS_ROOT_SIZE"
partition "$NABOS_ROOT_B_PARTITION" "$NABOS_ROOT_B_OFFSET" "$NABOS_ROOT_SIZE"
[ "$(cat "/sys/class/block/${disk_name}p${NABOS_DATA_PARTITION}/start")" = "$((NABOS_DATA_OFFSET / NABOS_SECTOR_SIZE))" ] || reject "incompatible data partition"
[ "$(cat "/sys/class/block/${disk_name}p${NABOS_DATA_PARTITION}/size")" -ge "$((NABOS_DATA_SIZE / NABOS_SECTOR_SIZE))" ] || reject "incompatible data partition"
[ "$(findmnt -n -o SOURCE,FSTYPE --mountpoint /data)" = "${NABOS_DISK}p${NABOS_DATA_PARTITION} ext4" ] ||
    reject "data partition is not persistent"
[ ! -e /data/.volatile ] || reject "data partition is not persistent"

if [ ! -f /usr/share/nabos/ca.cert.pem ] || [ ! -s /usr/share/nabos/ca.cert.pem ] || \
    [ -L /usr/share/nabos/ca.cert.pem ]; then
    reject "invalid image trust anchor"
fi

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
