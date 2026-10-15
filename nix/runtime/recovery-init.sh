#!/bin/sh
# Install as /init with the library at /lib/nabos-recovery.sh and its own closure.
PATH=/bin:/sbin
export PATH
umask 077
watchdog_open=false

recovery_block() {
    trap '' HUP INT TERM
    echo "nabos-recovery: $*; normal boot blocked" >&2
    # Keep feeding the watchdog, especially if completion cleared the pending key
    # and the corrective environment write was not readable or durable.
    while :; do
        if "$watchdog_open"; then printf '.' >&9 || :; fi
        if [ -x /bin/nabos-recovery-led ]; then
            timeout -s KILL 3 /bin/nabos-recovery-led redblink || :
        fi
        sleep 1
    done
}

trap 'recovery_block "recovery interrupted"' HUP INT TERM
[ -r /lib/nabos-layout.sh ] || recovery_block "recovery layout missing"
# shellcheck source-path=SCRIPTDIR
# shellcheck source=../../image/layout.sh
. /lib/nabos-layout.sh || recovery_block "recovery layout invalid"
[ -r /lib/nabos-recovery.sh ] || recovery_block "recovery library missing"
# shellcheck source-path=SCRIPTDIR
# shellcheck source=recovery.sh
. /lib/nabos-recovery.sh || recovery_block "recovery library invalid"

recovery_setup() {
    mkdir -p /proc /sys /dev /run || return 1
    mount -t proc proc /proc && mount -t sysfs sysfs /sys &&
        mount -t devtmpfs devtmpfs /dev &&
        mount -t tmpfs -o mode=0755,nosuid,nodev tmpfs /run || return 1
    exec </dev/console >/dev/console 2>&1
    modprobe bcm2835_wdt || :
    modprobe mmc_block || :
    modprobe ext4 || :
    modprobe bcm2835-ws2812 || :
    tries=0
    while [ ! -c /dev/watchdog0 ] && [ "$tries" -lt 100 ]; do
        sleep 0.1 || return 1
        tries=$((tries + 1))
    done
    [ -c /dev/watchdog0 ] || return 1
    exec 9>/dev/watchdog0 || return 1
    watchdog_open=true
    # PID 1 holds the descriptor open; this child pings throughout mkfs/fsck/env I/O.
    (
        while :; do
            printf '.' >&9 || exit 1
            sleep 1 || exit 1
        done
    ) &
    mkdir -p "$RECOVERY_WORK"
}

recovery_setup || recovery_block "initramfs setup failed"
if recovery_reset; then
    reboot -f
    recovery_block "reboot returned"
fi
recovery_block "factory reset failed"
