#!/usr/bin/env bash
# The caller mounts the artifact's root and immutable /etc before adding test tools.
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
# Native verifier resolves absolute store paths inside the supplied image root.
env -u SYSTEMD_UNIT_PATH systemd-analyze verify --root="$root" --man=no --generators=no --recursive-errors=yes "${system_units[@]}"
# systemd does not support --user with --root. Give the root verifier the shipped
# user graph first, with system targets supplying its implicit system-mode deps.
env SYSTEMD_UNIT_PATH=/etc/systemd/user:/usr/lib/systemd/user:/etc/systemd/system:/usr/lib/systemd/system \
  systemd-analyze verify --root="$root" --man=no --generators=no --recursive-errors=yes \
  pipewire.service pipewire.socket pipewire-pulse.service pipewire-pulse.socket wireplumber.service
echo 'PASS: native verification of composed image system/user units and user@1004'
