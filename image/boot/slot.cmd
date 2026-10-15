# Native BOOTMETH_RAUC supplies devtype, devnum, distro_rootpart and raucargs.
setenv nabos_part ${distro_rootpart}
setenv nabos_slot
if test "${distro_rootpart}" = "${nabos_root_a_part}"; then setenv nabos_slot A; fi
if test "${distro_rootpart}" = "${nabos_root_b_part}"; then setenv nabos_slot B; fi
if test -n "${nabos_slot}" && test "${devtype}" = mmc && test "${devnum}" = 0; then
    run nabos_load
    if test "${nabos_valid}" = 1 && run nabos_overlays && run nabos_fixup; then
        setenv bootargs "${nabos_kernel_params} root=/dev/mmcblk${devnum}p${distro_rootpart} init=${nabos_init} ${raucargs} quiet"
        echo "nabos: booting slot ${nabos_slot}: ${bootargs}"
        # BCM2835 permits just under 16s before Linux's first ping; Linux feeds
        # until systemd opens it (300s), covering rootwait and stuck initrds.
        if nabos_led off && wdt dev watchdog@7e100000 && wdt start 15999; then
            if test "${nabos_target}" = zero2-arm64; then
                booti ${kernel_addr_r} ${nabos_ramdisk} ${fdt_addr_r}
            else
                bootz ${kernel_addr_r} ${nabos_ramdisk} ${fdt_addr_r}
            fi
            wdt stop
        else
            echo "nabos: cannot arm watchdog"
        fi
    fi
fi
echo "nabos: slot ${nabos_slot} did not boot"
