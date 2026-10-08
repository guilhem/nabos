#!/usr/bin/env bash
# The caller mounts the original image root and immutable /etc without host tools.
set -euo pipefail
[[ $# == 2 && -d $2/etc ]] || {
  echo 'Usage: bash image/test-generated-units.sh TARGET COMPOSED_ROOT' >&2; exit 2;
}
case $1 in zero-armv6|zero2-arm64) ;; *) echo "Unknown target: $1" >&2; exit 2 ;; esac
root=$(realpath "$2")
system_units=(nabos.service nab-hardware.service device-core.service nabos-health.service
  nabos-board-led-off.service nabos-rauc-manual.service tagtagtag-mixerd.service
  user@1004.service linger-users.service NetworkManager.service rauc.service ssh.service sshd-keygen.service)
if [[ $1 == zero2-arm64 ]]; then
  system_units+=(linux-voice-assistant.service)
fi
# --root still opens absolute unit symlinks on the host; chroot confines resolution.
chroot=$(command -v chroot)
env -i SYSTEMD_UNIT_PATH=/etc/systemd/system QEMU_CPU=arm1176 \
  "$chroot" "$root" /run/current-system/systemd/bin/systemd-analyze verify --man=no --generators=no --recursive-errors=yes "${system_units[@]}"
# System targets supply the user graph's implicit system-mode dependencies.
env -i SYSTEMD_UNIT_PATH=/etc/systemd/user:/etc/systemd/system QEMU_CPU=arm1176 \
  "$chroot" "$root" /run/current-system/systemd/bin/systemd-analyze verify --man=no --generators=no --recursive-errors=yes \
  pipewire.service pipewire.socket pipewire-pulse.service pipewire-pulse.socket wireplumber.service
echo 'PASS: shipped verifier checked image system/user units and user@1004'
