# shellcheck shell=sh
set -eu
data=/sysroot/data

check_data() {
    [ ! -e "$data/.volatile" ]
    source=$(findmnt -n -o SOURCE --mountpoint "$data")
    [ "$(readlink -f "$source")" = "$(readlink -f "${NABOS_DISK:-/dev/mmcblk0}p4")" ]
    [ "$(findmnt -n -o FSTYPE --mountpoint "$data")" = ext4 ]
    options=$(findmnt -n -o OPTIONS --mountpoint "$data")
    case ,$options, in *,rw,*) ;; *) return 1 ;; esac
    available=$(stat -f -c '%a' "$data")
    block_size=$(stat -f -c '%S' "$data")
    [ "$((available * block_size))" -ge 67108864 ]
}

case ${1:-collect} in
    check) check_data ;;
    journal)
        check_data
        install -d -m 0755 "$data/system/journal" /sysroot/var/log/journal
        mount --bind "$data/system/journal" /sysroot/var/log/journal
        ;;
    collect)
        check_data
        cmdline=$(cat /proc/cmdline)
        slot=
        version=unknown
        for arg in $cmdline; do
            case $arg in
                rauc.slot=A) slot=A ;;
                rauc.slot=B) slot=B ;;
                init=*) version=${arg#init=} ;;
            esac
        done
        [ -n "$slot" ]
        umask 077
        install -d -m 0700 "$data/system/boot-logs"
        file=$data/system/boot-logs/slot-$slot.initrd.log
        install -m 0600 /dev/null "$file"
        {
            printf 'boot_id=%s\n' "$(cat /proc/sys/kernel/random/boot_id)"
            printf 'cmdline=%s\n' "$cmdline"
            printf 'version=%s\n' "$version"
        } >> "$file"
        # shortcut: forced reboot can lose the last line; add a snapshot only if field evidence requires it.
        exec journalctl --boot=0 --follow --lines=all --no-pager --output=short-monotonic >> "$file"
        ;;
    *) exit 1 ;;
esac
