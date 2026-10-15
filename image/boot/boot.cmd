# Common FAT dispatcher. Slot-specific files come from the native RAUC slot.
setenv devtype mmc
setenv devnum 0
test -n "${nabos_ovl_addr_r}" || setenv nabos_ovl_addr_r ${pxefile_addr_r}
if load mmc 0:1 ${scriptaddr} boot.env; then env import -t ${scriptaddr} ${filesize} nabos_target nabos_dtb nabos_root_a_part nabos_root_b_part nabos_boot_attempts; fi

# Both normal boot and rollback must validate the same complete slot contract.
# iminfo checks the script CRCs; the type byte requires a legacy script image.
setenv nabos_load 'setenv nabos_valid 0; setenv nabos_init; setenv nabos_kernel_params; setenv nabos_ramdisk; if size ${devtype} ${devnum}:${nabos_part} /boot/boot.scr && itest ${filesize} -le 10000 && load ${devtype} ${devnum}:${nabos_part} ${scriptaddr} /boot/boot.scr && iminfo ${scriptaddr}; then setexpr nabos_type_addr ${scriptaddr} + 1e; if itest.b *${nabos_type_addr} == 6 && load ${devtype} ${devnum}:${nabos_part} ${pxefile_addr_r} /boot/init && env import -t ${pxefile_addr_r} ${filesize} nabos_init nabos_kernel_params && test -n "${nabos_init}" && test -n "${nabos_kernel_params}" && load ${devtype} ${devnum}:${nabos_part} ${ramdisk_addr_r} /boot/initrd; then setenv nabos_ramdisk ${ramdisk_addr_r}:${filesize}; if load ${devtype} ${devnum}:${nabos_part} ${kernel_addr_r} /boot/kernel && itest ${filesize} -ge 40 && load ${devtype} ${devnum}:${nabos_part} ${fdt_addr_r} /boot/dtb/${nabos_dtb} && fdt addr ${fdt_addr_r}; then if test "${nabos_target}" = zero2-arm64; then setexpr nabos_magic_addr ${kernel_addr_r} + 38; if itest.l *${nabos_magic_addr} == 644d5241; then setenv nabos_valid 1; fi; elif test "${nabos_target}" = zero-armv6; then setexpr nabos_magic_addr ${kernel_addr_r} + 24; if itest.l *${nabos_magic_addr} == 016f2818; then setenv nabos_valid 1; fi; fi; fi; fi; fi'

# A failed overlay invalidates the blob: reload the plain DTB for health rollback.
setenv nabos_apply 'if load ${devtype} ${devnum}:${nabos_part} ${nabos_ovl_addr_r} /boot/overlays/tagtagtag-sound.dtbo && fdt apply ${nabos_ovl_addr_r}; then echo "nabos: overlay tagtagtag-sound applied"; else setenv nabos_ovl_ok 0; fi'
setenv nabos_overlays 'fdt addr ${fdt_addr_r}; fdt resize 0x10000; setenv nabos_ovl_ok 1; run nabos_apply; if test "${nabos_ovl_ok}" = 0; then echo "nabos: overlay failed, using the plain DTB"; load ${devtype} ${devnum}:${nabos_part} ${fdt_addr_r} /boot/dtb/${nabos_dtb}; fi; fdt addr ${fdt_addr_r}'

# Firmware board revision is absent from slot DTBs; Linux needs it in /system.
setenv nabos_fixup 'fdt addr ${fdt_addr_r}; fdt resize 0x1000; if test -n "${board_revision}"; then fdt get value nabos_rev /system linux,revision || fdt mknode / system; fdt set /system linux,revision <${board_revision}>; fdt get value nabos_rev /system linux,revision; echo "nabos: board revision ${nabos_rev}"; fi'

# A reset authorization lives in the raw environment, outside the formatted p4.
# Recovery loads exclusively from FAT into RAM, even if slot boot files are broken.
setenv nabos_recovery_boot 'nabos_led red; if load ${devtype} ${devnum}:1 ${kernel_addr_r} /recovery/kernel && load ${devtype} ${devnum}:1 ${ramdisk_addr_r} /recovery/initrd; then setenv nabos_ramdisk ${ramdisk_addr_r}:${filesize}; if load ${devtype} ${devnum}:1 ${fdt_addr_r} /recovery/${nabos_dtb}; then run nabos_fixup; setenv bootargs "rdinit=/init watchdog.open_timeout=300 panic=10 nabos.recovery=reset"; echo "nabos: starting independent recovery for slot ${NABOS_RESET_SLOT}"; if nabos_led off && wdt dev watchdog@7e100000 && wdt start 15999; then if test "${nabos_target}" = zero2-arm64; then booti ${kernel_addr_r} ${nabos_ramdisk} ${fdt_addr_r}; else bootz ${kernel_addr_r} ${nabos_ramdisk} ${fdt_addr_r}; fi; wdt stop; fi; fi; fi; echo "nabos: recovery failed, normal boot blocked"; nabos_led fail; reset'
if test "${BOOT_ORDER}" = "B A" || test "${BOOT_ORDER}" = B; then
    setenv nabos_preferred B
else
    setenv nabos_preferred A
fi

if test -n "${NABOS_RESET_SLOT}"; then
    setenv nabos_recovery_action
    if test "${NABOS_RESET_SLOT}" = A || test "${NABOS_RESET_SLOT}" = B; then
        run nabos_recovery_boot
    else
        echo "nabos: invalid pending reset, normal boot blocked"
        nabos_led fail
        reset
    fi
fi

# Reset authorizes only the selected version; the recovery image owns erasure.
if test "${nabos_recovery_action}" = reset; then
    setenv nabos_recovery_action
    setenv NABOS_RESET_SLOT ${nabos_preferred}
    if saveenv && env load && test "${NABOS_RESET_SLOT}" = "${nabos_preferred}"; then
        run nabos_recovery_boot
    else
        echo "nabos: reset authorization failed, normal boot blocked"
        nabos_led fail
        reset
    fi
elif test "${nabos_recovery_action}" = rollback; then
    setenv nabos_recovery_action
    setenv nabos_valid 0
    if test "${NABOS_ROLLBACK_SLOT}" != "${nabos_preferred}"; then
        if test "${NABOS_ROLLBACK_SLOT}" = A; then
            setenv nabos_part ${nabos_root_a_part}
            run nabos_load
        elif test "${NABOS_ROLLBACK_SLOT}" = B; then
            setenv nabos_part ${nabos_root_b_part}
            run nabos_load
        fi
    fi
    if test "${nabos_valid}" = 1; then
        if test "${NABOS_ROLLBACK_SLOT}" = A; then setenv BOOT_ORDER "A B"; setenv BOOT_A_LEFT ${nabos_boot_attempts}; else setenv BOOT_ORDER "B A"; setenv BOOT_B_LEFT ${nabos_boot_attempts}; fi
        setenv nabos_saved 0
        if saveenv && env load; then
            if test "${NABOS_ROLLBACK_SLOT}" = A && test "${BOOT_ORDER}" = "A B" && test "${BOOT_A_LEFT}" = "${nabos_boot_attempts}"; then setenv nabos_saved 1; fi
            if test "${NABOS_ROLLBACK_SLOT}" = B && test "${BOOT_ORDER}" = "B A" && test "${BOOT_B_LEFT}" = "${nabos_boot_attempts}"; then setenv nabos_saved 1; fi
        fi
        if test "${nabos_saved}" = 1; then
            echo "nabos: rollback selected slot ${NABOS_ROLLBACK_SLOT}"
            nabos_led green
        else
            echo "nabos: rollback persistence failed"
            nabos_led fail
            reset
        fi
    else
        echo "nabos: rollback cancelled, no different loadable recorded slot"
        nabos_led fail
    fi
else
    setenv nabos_recovery_action
fi


# Reject unknown slot names before the pinned native get_slot() lookup.
# Pending reset above must remain independent of this normal-boot validation.
if test -n "${BOOT_ORDER}"; then
    if test "${BOOT_ORDER}" != "A B" && test "${BOOT_ORDER}" != "B A" && test "${BOOT_ORDER}" != A && test "${BOOT_ORDER}" != B; then
        echo "nabos: invalid boot order, normal boot blocked"
        nabos_led fail
        reset
    fi
fi

# No rescan or local fallback: native RAUC owns attempt selection and persistence.
if setenv boot_targets mmc0 && bootmeth order rauc; then bootflow scan -b; fi
echo "nabos: bootflow returned, rebooting"
nabos_led fail
reset
