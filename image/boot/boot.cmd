# NabOS A/B boot script for RAUC's U-Boot backend, compiled to boot.scr.
# It lives on the read-only boot partition, so it must keep working for every
# future rootfs: everything slot specific is read from /boot of the chosen slot.
#
# RAUC owns BOOT_ORDER and BOOT_<slot>_LEFT. Each try decrements and saves the
# counter first, so a slot that hangs or fails its health check runs out of
# attempts; a slot whose kernel cannot even be loaded falls through at once.

test -n "${devtype}" || setenv devtype mmc
test -n "${devnum}" || setenv devnum 0
test -n "${nabos_ovl_addr_r}" || setenv nabos_ovl_addr_r ${pxefile_addr_r}

# nabos_target and nabos_dtb (see boot.env.in).
if load ${devtype} ${devnum}:1 ${scriptaddr} boot.env; then env import -t ${scriptaddr} ${filesize} nabos_target nabos_dtb; fi

# Only used if the stored environment is missing or corrupt: same as uboot.env.
test -n "${BOOT_ORDER}" || setenv BOOT_ORDER "A B"
test -n "${BOOT_A_LEFT}" || setenv BOOT_A_LEFT 3
test -n "${BOOT_B_LEFT}" || setenv BOOT_B_LEFT 0

# Hardware overlays, applied before Linux starts. A failed apply leaves the
# blob unusable, so reload the plain DTB: the system then boots without sound,
# fails its health check and rolls back instead of not booting at all.
setenv nabos_apply 'if load ${devtype} ${devnum}:${nabos_part} ${nabos_ovl_addr_r} /boot/overlays/${nabos_ovl}.dtbo && fdt apply ${nabos_ovl_addr_r}; then echo "nabos: overlay ${nabos_ovl} applied"; else setenv nabos_ovl_ok 0; fi'
# Debian rc6 needs its ears overlay; later slots drive the ears in userspace.
setenv nabos_overlays 'fdt addr ${fdt_addr_r}; fdt resize 0x10000; setenv nabos_ovl_ok 1; setenv nabos_ovl tagtagtag-sound; run nabos_apply; if test -e ${devtype} ${devnum}:${nabos_part} /boot/overlays/tagtagtag-ears.dtbo; then setenv nabos_ovl tagtagtag-ears; run nabos_apply; fi; if test ${nabos_ovl_ok} = 0; then echo "nabos: overlay failed, using the plain DTB"; load ${devtype} ${devnum}:${nabos_part} ${fdt_addr_r} /boot/dtb/${nabos_dtb}; fi'

# The firmware fills /system/linux,revision only in its own DTB, and U-Boot's
# board fixups do not copy it; /proc/cpuinfo needs it.
setenv nabos_fixup 'fdt addr ${fdt_addr_r}; fdt resize 0x1000; if test -n "${board_revision}"; then fdt get value nabos_rev /system linux,revision || fdt mknode / system; fdt set /system linux,revision <${board_revision}>; fdt get value nabos_rev /system linux,revision; echo "nabos: board revision ${nabos_rev}"; fi'

# Kernel and DTB always come from the same root filesystem as userspace.
# The BCM2835 watchdog allows just under 16 seconds for Linux's first ping.
# Its built-in driver feeds it until systemd opens it, at most 300 seconds:
# even rootwait or a stuck initrd must eventually consume another attempt.
# If boot returns (invalid kernel), stop it before attempting the other slot.
# Recognize Debian positively; damaged NixOS metadata must never fall back to it.
setenv nabos_boot 'setenv nabos_init; setenv nabos_kernel_params; setenv nabos_ramdisk; if test -e ${devtype} ${devnum}:${nabos_part} /boot/init || test -e ${devtype} ${devnum}:${nabos_part} /boot/initrd; then if load ${devtype} ${devnum}:${nabos_part} ${pxefile_addr_r} /boot/init && env import -t ${pxefile_addr_r} ${filesize} nabos_init nabos_kernel_params && test -n "${nabos_init}" && test -n "${nabos_kernel_params}" && load ${devtype} ${devnum}:${nabos_part} ${ramdisk_addr_r} /boot/initrd; then setenv nabos_ramdisk ${ramdisk_addr_r}:${filesize}; fi; elif test -e ${devtype} ${devnum}:${nabos_part} /usr/lib/nabos/boot-init; then setenv nabos_init /usr/lib/nabos/boot-init; setenv nabos_kernel_params "ro rootwait rootfstype=ext4 fsck.mode=skip watchdog.open_timeout=300 panic=10"; setenv nabos_ramdisk -; fi; if test -n "${nabos_ramdisk}"; then setenv bootargs "${nabos_kernel_params} root=/dev/mmcblk0p${nabos_part} init=${nabos_init} rauc.slot=${nabos_slot} quiet"; if load ${devtype} ${devnum}:${nabos_part} ${kernel_addr_r} /boot/kernel && load ${devtype} ${devnum}:${nabos_part} ${fdt_addr_r} /boot/dtb/${nabos_dtb}; then run nabos_overlays; run nabos_fixup; echo "nabos: booting slot ${nabos_slot}: ${bootargs}"; if wdt dev watchdog@7e100000 && wdt start 15999; then if test "${nabos_target}" = zero2-arm64; then booti ${kernel_addr_r} ${nabos_ramdisk} ${fdt_addr_r}; else bootz ${kernel_addr_r} ${nabos_ramdisk} ${fdt_addr_r}; fi; wdt stop; else echo "nabos: cannot arm watchdog"; fi; fi; fi; echo "nabos: slot ${nabos_slot} did not boot"'

setenv nabos_try 'if test "${nabos_slot}" = A; then setenv nabos_part 2; setenv nabos_left ${BOOT_A_LEFT}; else setenv nabos_part 3; setenv nabos_left ${BOOT_B_LEFT}; fi; if test ${nabos_left} -gt 0; then setexpr nabos_left ${nabos_left} - 1; if test "${nabos_slot}" = A; then setenv BOOT_A_LEFT ${nabos_left}; else setenv BOOT_B_LEFT ${nabos_left}; fi; echo "nabos: trying slot ${nabos_slot}, ${nabos_left} attempts left after this one"; saveenv; run nabos_boot; else echo "nabos: slot ${nabos_slot} has no attempts left"; fi'

if test "${BOOT_ORDER}" = "B A" || test "${BOOT_ORDER}" = "B"; then
	setenv nabos_slot B; run nabos_try
	setenv nabos_slot A; run nabos_try
else
	setenv nabos_slot A; run nabos_try
	setenv nabos_slot B; run nabos_try
fi

# Nothing booted: give both slots their attempts back (same value as RAUC's
# boot-attempts) and start over rather than stopping at a prompt.
echo "nabos: no bootable slot, restoring boot attempts"
setenv BOOT_A_LEFT 3
setenv BOOT_B_LEFT 3
saveenv
reset
