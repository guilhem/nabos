#!/bin/bash
# Executed inside the target Raspberry Pi OS filesystem, never on the host.
set -euo pipefail
[[ -f /etc/rpi-issue && -d /nabos-build ]] || { echo 'Target chroot required' >&2; exit 1; }
phase=${1:?build-packages, packages, drivers, wheels or finalize}
target=${2:?zero-armv6 or zero2-arm64}
case "$target" in
  zero-armv6) flavour=rpi-v6 dtb=bcm2708-rpi-zero-w.dtb ;;
  zero2-arm64) flavour=rpi-v8 dtb=bcm2710-rpi-zero-2-w.dtb ;;
  *) exit 2 ;;
esac
export DEBIAN_FRONTEND=noninteractive
runtime=(ca-certificates curl dbus dbus-user-session polkitd systemd-timesyncd openssl openssh-server sudo
  pipewire pipewire-pulse pipewire-alsa wireplumber pulseaudio-utils alsa-utils
  libasound2t64 libmpg123-0t64 mpg123
  network-manager wpasupplicant dnsmasq-base nftables avahi-daemon rauc rauc-service u-boot-tools libubootenv-tool
  util-linux fdisk e2fsprogs python3 device-tree-compiler)
development=(build-essential cmake pkg-config libasound2-dev libssl-dev
  bison flex bc python3-dev python3-setuptools python3-pyelftools
  "linux-headers-$flavour")
inputs=/nabos-build/inputs
src=/nabos-build/src
stage=/nabos-build/runtime

case "$phase" in
build-packages|packages)
  # dpkg post-install scripts must not start daemons in the build chroot.
  install -m755 /nabos-build/image/build-config/policy-rc.d /usr/sbin/policy-rc.d
  mkdir -p "$inputs/debs"
  install -m644 /nabos-build/image/build-config/99nabos-build /etc/apt/apt.conf.d/99nabos-build
  if [[ $target == zero-armv6 ]]; then
    # Use the official archive directly: the stock redirector can select a
    # mirror unreachable from standard GitHub runners. Keep its signing key.
    sed -i 's|http://raspbian.raspberrypi.com/raspbian/|https://archive.raspbian.org/raspbian/|' \
      /etc/apt/sources.list.d/raspbian.sources
  fi
  if [[ $target == zero2-arm64 ]]; then
    runtime+=(python3-venv libmpv2 libgomp1)
  fi
  packages=("${runtime[@]}" "linux-image-$flavour")
  if [[ $phase == build-packages ]]; then
    packages+=("${development[@]}")
  else
    # The delivery image uses only the package resolution already built against.
    [[ -f "$inputs/debs/manifest.tsv" ]] || { echo 'Builder package archive required' >&2; exit 1; }
  fi
  if [[ -f "$inputs/debs/manifest.tsv" ]]; then
    # Replay uses the complete archived package set, not a live APT resolution.
    (cd "$inputs/debs" && sha256sum --check SHA256SUMS)
    printf 'deb [trusted=yes] file:%s/debs ./\n' "$inputs" > /run/nabos-apt.list
    mkdir -p /run/nabos-apt-lists/partial
    replay_apt=(-o Dir::Etc::sourcelist=/run/nabos-apt.list -o Dir::Etc::sourceparts=-
      -o Dir::State::lists=/run/nabos-apt-lists)
    apt-get "${replay_apt[@]}" update
    apt-get "${replay_apt[@]}" install --yes --no-install-recommends "${packages[@]}"
    # Development packages may have upgraded runtime libraries in the builder.
    # Reuse those exact archived upgrades without installing development packages.
    apt-get "${replay_apt[@]}" upgrade --yes --with-new-pkgs --no-install-recommends
    if [[ $phase == packages ]]; then
      # Drop inherited build tools and Python GPIO bindings: hardware is in Rust.
      # Our own build dependencies stay in the disposable builder.
      apt-get "${replay_apt[@]}" purge --yes --auto-remove \
        "${development[@]}" gcc g++ cpp make dpkg-dev 'linux-headers-*' \
        python3-rpi-lgpio python3-lgpio liblgpio1
    fi
  else
    apt-get update
    # APT does not retry all TLS EOFs. Retry only fetching, before dpkg changes
    # the image; successful downloads stay cached and remain hash-checked.
    for attempt in 1 2 3; do
      if apt-get install --download-only --yes --no-install-recommends \
        "${packages[@]}"; then break; fi
      (( attempt < 3 )) || exit 1
    done
    apt-get install --no-download --yes --no-install-recommends "${packages[@]}"
    cp /var/cache/apt/archives/*.deb "$inputs/debs/"
    dpkg-query -W -f='${Package}\t${Version}\t${Architecture}\n' > "$inputs/debs/manifest.tsv"
    (cd "$inputs/debs" && dpkg-scanpackages . /dev/null | gzip -n > Packages.gz
      sha256sum ./*.deb Packages.gz > SHA256SUMS)
  fi
  if [[ $phase == packages ]]; then
    # A base/replayed package set must not leave a local broker in production.
    mapfile -t brokers < <(dpkg-query -W -f='${binary:Package}\t${db:Status-Status}\n' |
      awk -F '\t' '$1 ~ /^mosquitto(-clients)?(:.*)?$/ && $2 == "installed" { print $1 }')
    if (( ${#brokers[@]} )); then
      apt-get "${replay_apt[@]}" purge --yes "${brokers[@]}"
    fi
    rm -rf /etc/mosquitto
  fi
  # Purging inherited tools must not remove any runtime dependency.
  dpkg-query -W -f='${binary:Package}\t${db:Status-Status}\n' "${runtime[@]}" "linux-image-$flavour" |
    awk -F '\t' '$2 != "installed" { print "Missing runtime package: " $0; failed = 1 }
      END { exit failed }'
  # The operator account is separate from every daemon's authority.
  if ! getent passwd nabos >/dev/null; then
    existing=$(getent passwd 1000 || true)
    if [[ -n $existing ]]; then
      # The locked official Lite images contain the disabled first-boot pi
      # placeholder. Replace it rather than inheriting its privileged groups.
      [[ ${existing%%:*} == pi ]] || { echo 'Unexpected account at uid 1000' >&2; exit 1; }
      userdel --remove pi
      if getent group pi >/dev/null; then groupdel pi; fi
      rm -f /etc/sudoers.d/010_pi-nopasswd
    fi
    useradd --uid 1000 --user-group --create-home --home-dir /var/lib/nabos --shell /bin/bash nabos
  fi
  usermod -G '' nabos
  passwd --lock nabos
  # Fixed identities also keep persistent ownership stable across image updates.
  for entry in nab-app:1001 nab-hardware:1002 device-core:1003 nab-audio:1004; do
    name=${entry%:*}; uid=${entry#*:}
    if ! getent passwd "$name" >/dev/null; then
      useradd --uid "$uid" --user-group --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin "$name"
    fi
    [[ $(id -u "$name") == "$uid" && $(id -g "$name") == "$uid" ]]
    passwd --lock "$name"
  done
  if ! getent group nab-media >/dev/null; then groupadd --gid 1005 nab-media; fi
  [[ $(getent group nab-media | cut -d: -f3) == 1005 ]]
  usermod --home /var/lib/nabos/lva nab-audio
  usermod -G audio nab-audio
  mkdir -p /var/lib/systemd/linger
  rm -f /var/lib/systemd/linger/nabos
  touch /var/lib/systemd/linger/nab-audio
  # Select the image kernel explicitly; uname reports the build host kernel.
  mapfile -t kernels < <(find /lib/modules -mindepth 1 -maxdepth 1 -type d -name "*-$flavour" -printf '%f\n' | sort -V)
  (( ${#kernels[@]} > 0 )) || { echo "No $flavour kernel installed" >&2; exit 1; }
  kernel=${kernels[-1]}
  if [[ $phase == build-packages ]]; then
    [[ -f /lib/modules/$kernel/build/Module.symvers ]] || { echo 'Matching kernel build symbols missing' >&2; exit 1; }
    [[ $(dpkg-query -W -f='${Version}' "linux-image-$kernel") == \
       "$(dpkg-query -W -f='${Version}' "linux-headers-$kernel")" ]] || { echo 'Kernel/header package version mismatch' >&2; exit 1; }
    for option in CONFIG_BCM2835_WDT=y CONFIG_WATCHDOG_HANDLE_BOOT_ENABLED=y \
      'CONFIG_SQUASHFS=[ym]' CONFIG_SQUASHFS_ZSTD=y; do
      grep -qxE "$option" "/lib/modules/$kernel/build/.config" || { echo "Kernel lacks $option" >&2; exit 1; }
    done
    printf '%s\n' "$kernel" > /nabos-build/kernel-release
    dpkg-query -W -f='${Package}\t${Version}\t${Architecture}\n' > /nabos-build/builder-packages.tsv
  else
    [[ $kernel == "$(cat /nabos-build/kernel-release)" ]] || { echo 'Builder/runtime kernel mismatch' >&2; exit 1; }
    dpkg-query -W -f='${Package}\t${Version}\t${Architecture}\n' > /tmp/runtime-packages.tsv
    awk -F '\t' 'NR == FNR { versions[$1 FS $3] = $2; next }
      versions[$1 FS $3] != $2 { print "Builder/runtime package mismatch: " $0; failed = 1 }
      END { exit failed }' /nabos-build/builder-packages.tsv /tmp/runtime-packages.tsv
  fi
  ;;
drivers)
  kernel=$(cat /nabos-build/kernel-release)
  mkdir -p "$stage/usr/lib/modules/$kernel/updates/nabos" "$stage/boot/firmware/overlays"
  directory=$src/sound
  [[ -d $directory ]] || exit 1
  if [[ -f /nabos-build/image/patches/sound.patch ]]; then
    patch --directory="$directory" -p1 < "/nabos-build/image/patches/sound.patch"
  fi
  make -C "/lib/modules/$kernel/build" M="$directory" -j2 modules
  find "$directory" -maxdepth 1 -name '*.ko' -exec install -m644 '{}' "$stage/usr/lib/modules/$kernel/updates/nabos/" \;
  for overlay in "$directory"/*-overlay.dts; do
    [[ -f $overlay ]] || continue
    # Kernel headers are unavailable to dtc's parser; preprocess DTS first.
    cpp -nostdinc -undef -D__DTS__ -x assembler-with-cpp -I "/lib/modules/$kernel/build/include" "$overlay" |
      dtc -@ -I dts -O dtb -o "$stage/boot/firmware/overlays/$(basename "${overlay%-overlay.dts}").dtbo"
  done
  make -C "$src/sound" tagtagtag-mixerd
  install -Dm755 "$src/sound/tagtagtag-mixerd" "$stage/usr/local/sbin/tagtagtag-mixerd"
  install -Dm644 "$src/sound/mixer.conf.default" "$stage/var/lib/tagtagtag-sound/mixer.conf.default"
  install -m644 "$src/sound/mixer.conf.default" "$stage/var/lib/tagtagtag-sound/mixer.conf"
  install -Dm644 "$src/sound/tagtagtag-mixerd.service" "$stage/usr/lib/systemd/system/tagtagtag-mixerd.service"
  # Keep the existing DMA/PWM library instead of reimplementing LED timing.
  cmake -S "$src/led" -B "$src/led-build" -DBUILD_SHARED=ON -DBUILD_TEST=OFF -DCMAKE_INSTALL_PREFIX=/usr
  cmake --build "$src/led-build" --parallel 2
  # Copy the shared library only; CMake's install also includes development headers.
  install -Dm644 "$src/led-build/libws2811.so" "$stage/usr/lib/libws2811.so"
  ;;
wheels)
  if [[ $target == zero2-arm64 ]]; then
    # Only the disposable builder runs the Python build backend.
    mkdir -p "$inputs/wheels" /opt/linux-voice-assistant
    cp -a "$src/lva/." /opt/linux-voice-assistant/
    export SETUPTOOLS_SCM_PRETEND_VERSION=1.1.15
    # GitHub source archives have no .git metadata. Enable setuptools-scm's
    # explicit version override for the pinned release.
    printf '\n[tool.setuptools_scm]\n' >> /opt/linux-voice-assistant/pyproject.toml
    python3 -m venv /opt/linux-voice-assistant/.venv
    pip=/opt/linux-voice-assistant/.venv/bin/pip
    if [[ -f "$inputs/wheels/SHA256SUMS" ]]; then
      (cd "$inputs/wheels" && sha256sum --check SHA256SUMS)
    else
      "$pip" download --only-binary=:all: --require-hashes --dest "$inputs/wheels" \
        -r /nabos-build/image/lva-requirements.lock
      "$pip" install --no-index --no-deps "$inputs"/wheels/*.whl
      "$pip" wheel --no-deps --no-build-isolation --wheel-dir "$inputs/wheels" /opt/linux-voice-assistant
      (cd "$inputs/wheels" && sha256sum ./*.whl > SHA256SUMS)
    fi
    # Upstream resolves its data files relative to the module source directory.
    mkdir -p "$stage/opt/linux-voice-assistant"
    for item in linux_voice_assistant version.txt sounds wakewords LICENSE.md; do
      cp -a "$src/lva/$item" "$stage/opt/linux-voice-assistant/"
    done
  fi
  ;;
finalize)
  kernel=$(cat /nabos-build/kernel-release)
  depmod -a "$kernel"
  ldconfig
  if [[ $target == zero2-arm64 ]]; then
    (cd "$inputs/wheels" && sha256sum --check SHA256SUMS)
    python3 -m venv /opt/linux-voice-assistant/.venv
    pip=/opt/linux-voice-assistant/.venv/bin/pip
    "$pip" install --no-cache-dir --only-binary=:all: --no-index --find-links "$inputs/wheels" linux-voice-assistant==1.1.15
    "$pip" list --format=freeze > "$inputs/wheels/manifest.txt"
  fi
  # U-Boot loads the kernel and vendor DTB from the selected root partition.
  mkdir -p /boot/dtb
  # Raspberry Pi OS keeps a compatibility symlink into the firmware FAT.
  # Slot-local overlays must be actual files in the root filesystem.
  if [[ -L /boot/overlays ]]; then rm /boot/overlays; fi
  cp -a /boot/firmware/overlays /boot/
  cp /boot/firmware/*.dtb /boot/dtb/
  # Linux hardware profile, only in the DTB U-Boot loads from this slot.
  dtc -@ -I dts -O dtb -o /tmp/nabos.dtbo /nabos-build/image/nabos-overlay.dts
  fdtoverlay -i "/boot/dtb/$dtb" -o "/boot/dtb/$dtb" /tmp/nabos.dtbo
  if [[ $target == zero-armv6 ]]; then
    cp /boot/firmware/kernel.img /boot/kernel
  else
    if gzip -t /boot/firmware/kernel8.img 2>/dev/null; then
      gzip -dc /boot/firmware/kernel8.img > /boot/kernel
    else
      cp /boot/firmware/kernel8.img /boot/kernel
    fi
  fi
  # The modern image does not start the stock onboarding, resize or APT jobs.
  for service in ssh.socket apt-daily.timer apt-daily-upgrade.timer unattended-upgrades regenerate_ssh_host_keys sshd-keygen userconfig resize2fs_once cloud-init cloud-init-local cloud-config cloud-final; do
    systemctl mask "$service"
  done
  apt-get clean
  rm -rf /var/lib/apt/lists/* /tmp/* /root/.cache /root/.cargo /root/.rustup /root/go
  find /var/log -type f -exec truncate -s0 '{}' +
  # SSH keys, machine identity and random seeds belong to the device, not the image.
  rm -f /etc/ssh/ssh_host_* /etc/ssh/sshd_config.d/rename_user.conf /var/lib/systemd/random-seed
  dpkg-query -W -f='${Package}\t${Version}\t${Architecture}\n' > /usr/share/nabos/packages.tsv
  /usr/lib/nabos/image-setup
  rm -f /usr/lib/nabos/image-setup /usr/sbin/policy-rc.d /etc/apt/apt.conf.d/99nabos-build
  ;;
*) exit 2 ;;
esac
