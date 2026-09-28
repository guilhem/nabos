#!/bin/bash
# Executed inside the target Raspberry Pi OS filesystem, never on the host.
set -euo pipefail
[[ -f /etc/rpi-issue && -d /nabos-build ]] || { echo 'Target chroot required' >&2; exit 1; }
phase=${1:?packages, drivers or finalize}
target=${2:?zero-armv6 or zero2-arm64}
case "$target" in
  zero-armv6) flavour=rpi-v6 ;;
  zero2-arm64) flavour=rpi-v8 ;;
  *) exit 2 ;;
esac
export DEBIAN_FRONTEND=noninteractive
runtime=(ca-certificates curl dbus dbus-user-session polkitd systemd-timesyncd openssl
  pipewire pipewire-pulse pipewire-alsa wireplumber pulseaudio-utils alsa-utils
  libasound2t64 libmpg123-0t64 mpg123 mosquitto mosquitto-clients
  network-manager comitup avahi-daemon rauc rauc-service u-boot-tools libubootenv-tool i2c-tools raspi-utils-dt
  util-linux fdisk e2fsprogs python3)
development=(build-essential cmake pkg-config libasound2-dev libssl-dev
  bison flex bc device-tree-compiler python3-dev python3-setuptools python3-pyelftools
  "linux-headers-$flavour")
inputs=/nabos-build/inputs
src=/nabos-build/src

case "$phase" in
packages)
  # dpkg post-install scripts must not start daemons in the build chroot.
  printf '#!/bin/sh\nexit 101\n' > /usr/sbin/policy-rc.d
  chmod 755 /usr/sbin/policy-rc.d
  mkdir -p "$inputs/debs"
  printf 'Binary::apt::APT::Keep-Downloaded-Packages "true";\nAcquire::Retries "3";\n' > /etc/apt/apt.conf.d/99nabos-build
  if [[ $target == zero-armv6 ]]; then
    # Use the official archive directly: the stock redirector can select a
    # mirror unreachable from standard GitHub runners. Keep its signing key.
    sed -i 's|http://raspbian.raspberrypi.com/raspbian/|https://archive.raspbian.org/raspbian/|' \
      /etc/apt/sources.list.d/raspbian.sources
  fi
  if [[ $target == zero2-arm64 ]]; then
    runtime+=(python3-venv libmpv2 libgomp1)
  fi
  if [[ -f "$inputs/debs/manifest.tsv" ]]; then
    # Replay uses the complete archived package set, not a live APT resolution.
    (cd "$inputs/debs" && sha256sum --check SHA256SUMS)
    printf 'deb [trusted=yes] file:%s/debs ./\n' "$inputs" > /run/nabos-apt.list
    mkdir -p /run/nabos-apt-lists/partial
    replay_apt=(-o Dir::Etc::sourcelist=/run/nabos-apt.list -o Dir::Etc::sourceparts=-
      -o Dir::State::lists=/run/nabos-apt-lists)
    apt-get "${replay_apt[@]}" update
    apt-get "${replay_apt[@]}" install --yes --no-install-recommends \
      "${runtime[@]}" "${development[@]}" "linux-image-$flavour"
  else
    apt-get update
    # APT does not retry all TLS EOFs. Retry only fetching, before dpkg changes
    # the image; successful downloads stay cached and remain hash-checked.
    for attempt in 1 2 3; do
      if apt-get install --download-only --yes --no-install-recommends \
        "${runtime[@]}" "${development[@]}" "linux-image-$flavour"; then break; fi
      (( attempt < 3 )) || exit 1
    done
    apt-get install --no-download --yes --no-install-recommends "${runtime[@]}" "${development[@]}" "linux-image-$flavour"
    cp /var/cache/apt/archives/*.deb "$inputs/debs/"
    dpkg-query -W -f='${Package}\t${Version}\t${Architecture}\n' > "$inputs/debs/manifest.tsv"
    (cd "$inputs/debs" && dpkg-scanpackages . /dev/null | gzip -n > Packages.gz
      sha256sum ./*.deb Packages.gz > SHA256SUMS)
  fi
  # A fixed uid is shared by headless PipeWire and the application services.
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
    useradd --uid 1000 --user-group --create-home --home-dir /var/lib/nabos --shell /usr/sbin/nologin nabos
  fi
  usermod -aG audio,video nabos
  passwd --lock nabos
  mkdir -p /var/lib/systemd/linger
  touch /var/lib/systemd/linger/nabos
  # Select the image kernel explicitly; uname reports the build host kernel.
  mapfile -t kernels < <(find /lib/modules -mindepth 1 -maxdepth 1 -type d -name "*-$flavour" -printf '%f\n' | sort -V)
  (( ${#kernels[@]} > 0 )) || { echo "No $flavour kernel installed" >&2; exit 1; }
  kernel=${kernels[-1]}
  [[ -f /lib/modules/$kernel/build/Module.symvers ]] || { echo 'Matching kernel build symbols missing' >&2; exit 1; }
  for option in CONFIG_BCM2835_WDT=y CONFIG_WATCHDOG_HANDLE_BOOT_ENABLED=y; do
    grep -qxF "$option" "/lib/modules/$kernel/build/.config" || { echo "Kernel lacks $option" >&2; exit 1; }
  done
  printf '%s\n' "$kernel" > /nabos-build/kernel-release
  ;;
drivers)
  kernel=$(cat /nabos-build/kernel-release)
  mkdir -p "/lib/modules/$kernel/updates/nabos" /boot/firmware/overlays
  for driver in ears sound cr14 nfc; do
    directory=$src/$driver
    [[ -d $directory ]] || exit 1
    if [[ -f /nabos-build/image/patches/$driver.patch ]]; then
      patch --directory="$directory" -p1 < "/nabos-build/image/patches/$driver.patch"
    fi
    make -C "/lib/modules/$kernel/build" M="$directory" -j2 modules
    find "$directory" -maxdepth 1 -name '*.ko' -exec install -m644 '{}' "/lib/modules/$kernel/updates/nabos/" \;
    for overlay in "$directory"/*-overlay.dts; do
      [[ -f $overlay ]] || continue
      # Kernel headers are unavailable to dtc's parser; preprocess DTS first.
      cpp -nostdinc -undef -D__DTS__ -x assembler-with-cpp -I "/lib/modules/$kernel/build/include" "$overlay" |
        dtc -@ -I dts -O dtb -o "/boot/firmware/overlays/$(basename "${overlay%-overlay.dts}").dtbo"
    done
  done
  depmod -a "$kernel"
  make -C "$src/sound" tagtagtag-mixerd
  install -Dm755 "$src/sound/tagtagtag-mixerd" /usr/local/sbin/tagtagtag-mixerd
  install -Dm644 "$src/sound/mixer.conf.default" /var/lib/tagtagtag-sound/mixer.conf.default
  install -m644 "$src/sound/mixer.conf.default" /var/lib/tagtagtag-sound/mixer.conf
  install -Dm644 "$src/sound/tagtagtag-mixerd.service" /usr/lib/systemd/system/tagtagtag-mixerd.service
  # Keep the existing DMA/PWM library instead of reimplementing LED timing.
  cmake -S "$src/led" -B "$src/led-build" -DBUILD_SHARED=ON -DBUILD_TEST=OFF -DCMAKE_INSTALL_PREFIX=/usr
  cmake --build "$src/led-build" --parallel 2
  cmake --install "$src/led-build"
  ldconfig
  ;;
finalize)
  kernel=$(cat /nabos-build/kernel-release)
  if [[ $target == zero2-arm64 ]]; then
    # Build/install all Python wheels in CI. The device never runs pip.
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
    "$pip" install --no-index --find-links "$inputs/wheels" linux-voice-assistant==1.1.15
    "$pip" list --format=freeze > "$inputs/wheels/manifest.txt"
    # SoundCard connects to PulseAudio on import; no audio server runs in the chroot.
    /opt/linux-voice-assistant/.venv/bin/python -c \
      'from importlib.metadata import version; print({p: version(p) for p in ("linux-voice-assistant", "aioesphomeapi", "soundcard")})'
  fi
  # U-Boot loads the kernel and vendor DTB from the selected root partition.
  mkdir -p /boot/dtb
  # Raspberry Pi OS keeps a compatibility symlink into the firmware FAT.
  # Slot-local overlays must be actual files in the root filesystem.
  if [[ -L /boot/overlays ]]; then rm /boot/overlays; fi
  cp -a /boot/firmware/overlays /boot/
  cp /boot/firmware/*.dtb /boot/dtb/
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
  for service in ssh sshd apt-daily.timer apt-daily-upgrade.timer unattended-upgrades regenerate_ssh_host_keys userconfig resize2fs_once cloud-init cloud-init-local cloud-config cloud-final; do
    systemctl mask "$service"
  done
  apt-get purge --yes --auto-remove "${development[@]}"
  apt-get clean
  rm -rf /var/lib/apt/lists/* /tmp/*
  # SSH keys, machine identity and random seeds belong to the device, not the image.
  rm -f /etc/ssh/ssh_host_* /var/lib/systemd/random-seed
  dpkg-query -W -f='${Package}\t${Version}\t${Architecture}\n' > /usr/share/nabos/packages.tsv
  /usr/lib/nabos/image-setup
  rm -f /usr/lib/nabos/image-setup /usr/sbin/policy-rc.d /etc/apt/apt.conf.d/99nabos-build
  ;;
*) exit 2 ;;
esac
