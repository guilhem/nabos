#!/bin/sh
# Standalone initramfs library; no paths or tools come from either root slot.
RECOVERY_DISK=$NABOS_DISK
RECOVERY_SYS=/sys/class/block
RECOVERY_PROC=/proc
RECOVERY_WORK=/run/nabos-recovery
RECOVERY_ENV_CONFIG=/etc/fw_env.config
RECOVERY_SLOT=

recovery_log() { echo "nabos-recovery: $*" >&2; }

recovery_block_device() {
    [ -b "$1" ] || return 1
    major=${2%:*}; minor=${2#*:}
    [ "$(stat -c '%t:%T' "$1")" = "$(printf '%x:%x' "$major" "$minor")" ]
}

recovery_wait_devices() {
    tries=0
    while [ "$tries" -lt 100 ]; do
        ready=true
        recovery_block_device "$RECOVERY_DISK" 179:0 || ready=false
        for n in "$NABOS_BOOT_PARTITION" "$NABOS_ROOT_A_PARTITION" "$NABOS_ROOT_B_PARTITION" "$NABOS_DATA_PARTITION"; do
            recovery_block_device "${RECOVERY_DISK}p$n" "179:$n" || ready=false
        done
        "$ready" && return 0
        sleep 0.1 || return 1
        tries=$((tries + 1))
    done
    recovery_log "SD partitions did not appear within 10 seconds"
    return 1
}

recovery_geometry() {
    total=$(cat "$RECOVERY_SYS/mmcblk0/size") || return 1
    case $total in ''|*[!0-9]*|0*) return 1 ;; esac
    [ "$(cat "$RECOVERY_SYS/mmcblk0/dev")" = 179:0 ] &&
        [ "$(cat "$RECOVERY_SYS/mmcblk0/queue/logical_block_size")" = "$NABOS_SECTOR_SIZE" ] || return 1
    sfdisk --dump "$RECOVERY_DISK" > "$RECOVERY_WORK/table" || return 1
    # Reject changed types, extra entries, and sectors not described by the MBR.
    awk -v disk="$RECOVERY_DISK" -v total="$total" -v sector_size="$NABOS_SECTOR_SIZE" \
        -v boot_part="$NABOS_BOOT_PARTITION" -v root_a_part="$NABOS_ROOT_A_PARTITION" \
        -v root_b_part="$NABOS_ROOT_B_PARTITION" -v data_part="$NABOS_DATA_PARTITION" \
        -v boot_start="$((NABOS_BOOT_PRIMARY_OFFSET / NABOS_SECTOR_SIZE))" \
        -v boot_secondary="$((NABOS_BOOT_SECONDARY_OFFSET / NABOS_SECTOR_SIZE))" \
        -v boot_size="$((NABOS_BOOT_SIZE / NABOS_SECTOR_SIZE))" \
        -v root_a_start="$((NABOS_ROOT_A_OFFSET / NABOS_SECTOR_SIZE))" \
        -v root_b_start="$((NABOS_ROOT_B_OFFSET / NABOS_SECTOR_SIZE))" \
        -v root_size="$((NABOS_ROOT_SIZE / NABOS_SECTOR_SIZE))" \
        -v data_start="$((NABOS_DATA_OFFSET / NABOS_SECTOR_SIZE))" \
        -v data_size="$((NABOS_DATA_SIZE / NABOS_SECTOR_SIZE))" \
        -v boot_type="$(printf '%x' "$NABOS_BOOT_PARTITION_TYPE")" \
        -v root_type="$(printf '%x' "$NABOS_ROOT_PARTITION_TYPE")" \
        -v data_type="$(printf '%x' "$NABOS_DATA_PARTITION_TYPE")" '
        /^label: / { if ($2 == "dos") label++; else bad=1; next }
        /^unit: / { if ($2 == "sectors") unit++; else bad=1; next }
        /^sector-size: / { if ($2 == sector_size) sector++; else bad=1; next }
        /^device: / { if ($2 != disk) bad=1; next }
        /^label-id: / || /^[[:space:]]*$/ { next }
        {
            gsub(/[,:=]/, " ")
            n=++count
            if ($1 != disk "p" n || $2 != "start" || $4 != "size" || $6 != "type" ||
                $3 !~ /^[1-9][0-9]*$/ || $5 !~ /^[1-9][0-9]*$/) bad=1
            if (n==boot_part && (($3!=boot_start && $3!=boot_secondary) || $5!=boot_size || $7!=boot_type || NF!=8 || $8!="bootable")) bad=1
            if (n==root_a_part && ($3!=root_a_start || $5!=root_size || $7!=root_type || NF!=7)) bad=1
            if (n==root_b_part && ($3!=root_b_start || $5!=root_size || $7!=root_type || NF!=7)) bad=1
            if (n==data_part && ($3!=data_start || $5<data_size || $7!=data_type || NF!=7)) bad=1
            if ($3+$5 > total) bad=1
            print n, $3, $5
        }
        END { if (bad || count!=4 || label!=1 || unit!=1 || sector!=1) exit 1 }
    ' "$RECOVERY_WORK/table" > "$RECOVERY_WORK/geometry" || return 1
    while read -r n start size; do
        sys="$RECOVERY_SYS/mmcblk0p$n"
        [ "$(cat "$sys/partition")" = "$n" ] &&
            [ "$(cat "$sys/start")" = "$start" ] &&
            [ "$(cat "$sys/size")" = "$size" ] &&
            [ "$(cat "$sys/dev")" = "179:$n" ] &&
            recovery_block_device "${RECOVERY_DISK}p$n" "179:$n" || return 1
    done < "$RECOVERY_WORK/geometry"
    # Major/minor also catches bind mounts and mounts made through aliases.
    awk -v data_dev="179:$NABOS_DATA_PARTITION" '$3 == data_dev { mounted=1 } END { exit mounted }' "$RECOVERY_PROC/self/mountinfo" || return 1
    [ -d "$RECOVERY_SYS/mmcblk0p$NABOS_DATA_PARTITION/holders" ] || return 1
    for holder in "$RECOVERY_SYS/mmcblk0p$NABOS_DATA_PARTITION/holders/"*; do
        [ ! -e "$holder" ] || return 1
    done
}

recovery_read_env() {
    # Never accept compiled defaults or partial output from a failed CRC read.
    fw_printenv -c "$RECOVERY_ENV_CONFIG" > "$RECOVERY_WORK/env.new" 2> "$RECOVERY_WORK/env.error" &&
        [ ! -s "$RECOVERY_WORK/env.error" ] &&
        mv "$RECOVERY_WORK/env.new" "$RECOVERY_WORK/env"
}

recovery_env_value() {
    awk -v key="$1" '
        index($0, key "=")==1 { count++; value=substr($0, length(key)+2) }
        END { if (count!=1) exit 1; print value }
    ' "$RECOVERY_WORK/env"
}

recovery_authorize() {
    recovery_read_env || return 1
    RECOVERY_SLOT=$(recovery_env_value NABOS_RESET_SLOT) || return 1
    case $RECOVERY_SLOT in A|B) ;; *) RECOVERY_SLOT=; return 1 ;; esac
}

recovery_verify_finished() {
    recovery_read_env || return 1
    [ "$(recovery_env_value "BOOT_${RECOVERY_SLOT}_LEFT")" = "$NABOS_BOOT_ATTEMPTS" ] &&
        [ "$(recovery_env_value BOOT_ORDER)" = "$order" ] &&
        awk '/^NABOS_RESET_SLOT=/ || /^nabos_recovery_action=/ { found=1 }
            END { exit found }' "$RECOVERY_WORK/env"
}

recovery_write_env() {
    rc=0
    # fw_setenv can report fsync errors while returning zero.
    fw_setenv -c "$RECOVERY_ENV_CONFIG" -s "$1" 2> "$RECOVERY_WORK/env-errors" || rc=$?
    if [ -s "$RECOVERY_WORK/env-errors" ]; then
        cat "$RECOVERY_WORK/env-errors" >&2
        return 1
    fi
    [ "$rc" -eq 0 ]
}

recovery_reauthorize() {
    printf 'NABOS_RESET_SLOT %s\n' "$RECOVERY_SLOT" > "$RECOVERY_WORK/reauthorize" || return 1
    written=true
    recovery_write_env "$RECOVERY_WORK/reauthorize" || written=false
    sync "$RECOVERY_DISK" || written=false
    # A writer can fail after changing the environment; still attempt readback.
    recovery_read_env &&
        [ "$(recovery_env_value NABOS_RESET_SLOT)" = "$RECOVERY_SLOT" ] && "$written"
}

recovery_reset() {
    recovery_wait_devices || return 1
    recovery_authorize || { recovery_log "no valid durable reset authorization"; return 1; }
    recovery_geometry || { recovery_log "unsafe SD geometry or mounted data"; return 1; }
    data=${RECOVERY_DISK}p${NABOS_DATA_PARTITION}
    if [ -x /bin/nabos-recovery-led ]; then
        timeout -s KILL 3 /bin/nabos-recovery-led red || :
    fi
    # A damaged old filesystem is irrelevant after explicit authorization.
    if ! { mkfs.ext4 -F -L nabos-data -E lazy_itable_init=0,lazy_journal_init=0 "$data" &&
        sync "$data" && e2fsck -fn "$data" &&
        [ "$(blkid -p -s TYPE -o value "$data")" = ext4 ] &&
        [ "$(blkid -p -s LABEL -o value "$data")" = nabos-data ] &&
        sync "$data"; }; then
        recovery_log "format or fresh filesystem verification failed; reset remains pending"
        return 1
    fi
    case $RECOVERY_SLOT in A) order='A B' ;; B) order='B A' ;; esac
    # One redundant-environment transaction; a cut before it leaves reset pending.
    printf 'BOOT_%s_LEFT %s\nBOOT_ORDER %s\nNABOS_RESET_SLOT\nnabos_recovery_action\n' \
        "$RECOVERY_SLOT" "$NABOS_BOOT_ATTEMPTS" "$order" > "$RECOVERY_WORK/complete" || return 1
    if recovery_write_env "$RECOVERY_WORK/complete" &&
        sync "$RECOVERY_DISK" && recovery_verify_finished; then
        recovery_log "factory reset complete for slot $RECOVERY_SLOT"
        return 0
    fi
    recovery_log "completion could not be verified; restoring reset authorization"
    if ! recovery_reauthorize; then
        recovery_log "reset reauthorization could not be verified; do not reboot"
    fi
    # Even successful correction does not authorize a reboot after uncertain completion.
    return 1
}
