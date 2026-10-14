#!/bin/sh
# Optional initramfs reset/failure indicator; the caller repeats bounded blinks.
set -efu
PATH=/bin:/sbin
export PATH
case ${1:-} in red|redblink) ;; *) exit 1 ;; esac

red_frame() {
    for index in 0 1 2 3 4; do
        led="/sys/class/leds/multi:indicator-$index"
        [ "$(cat "$led/max_brightness")" = 255 ] || return 1
        colors=$(cat "$led/multi_index") || return 1
        intensity=
        seen=
        count=0
        for color in $colors; do
            case $color in
                red) value=255 ;;
                green|blue) value=0 ;;
                *) return 1 ;;
            esac
            case " $seen " in *" $color "*) return 1 ;; esac
            seen="$seen $color"
            intensity="$intensity $value"
            count=$((count + 1))
        done
        # Exactly RGB once each; the kernel owns the wire encoding.
        [ "$count" -eq 3 ] || return 1
        printf '%s\n' "${intensity# }" > "$led/multi_intensity" || return 1
        printf '%s\n' "$1" > "$led/brightness" || return 1
    done
    if [ -w /sys/class/leds/multi:indicator-0/sync ]; then
        printf '1\n' > /sys/class/leds/multi:indicator-0/sync || return 1
    fi
}

[ "$1" != red ] || { red_frame 200; exit; }
pulse=0
while [ "$pulse" -lt 3 ]; do
    red_frame 200
    sleep 0.2
    red_frame 0
    sleep 0.2
    pulse=$((pulse + 1))
done
