#!/bin/bash
# Build on a disposable Linux runner; only loop devices owned by this process are modified.
set -euo pipefail
# Keep chroot mounts out of host services' mount namespaces. Otherwise a host
# service can retain the filesystem after umount and prevent its final fsck.
if [[ ${NABOS_BUILD_NAMESPACE:-} != 1 ]]; then
  exec sudo --preserve-env env "PATH=$PATH" NABOS_BUILD_NAMESPACE=1 \
    unshare --mount --propagation private \
    setpriv --reuid="$(id -u)" --regid="$(id -g)" --init-groups \
    bash "$0" "$@"
fi
repo=$(cd "$(dirname "$0")/.." && pwd)
GO=${GO:-go}
export GO
target=${1:?Usage: image/build.sh TARGET VERSION [--development] [--defer-tests] [--replay INPUTS.tar.xz | --components DIR]}
version=${2:?release version required}
shift 2
development=false
defer_tests=false
replay=
components=
while (( $# )); do
  case $1 in
    --development) development=true; shift ;;
    --defer-tests) defer_tests=true; shift ;;
    --replay) replay=$(realpath "${2:?archive required}"); shift 2 ;;
    --components) components=$(realpath "${2:?component directory required}"); shift 2 ;;
    *) echo "Unknown argument: $1" >&2; exit 2 ;;
  esac
done
[[ -z $replay || -z $components ]] || { echo 'Choose --replay or --components' >&2; exit 2; }
[[ $target == zero-armv6 || $target == zero2-arm64 ]] || exit 2
[[ $version =~ ^[a-zA-Z0-9][a-zA-Z0-9.+_-]{0,63}$ ]] || { echo 'Invalid version' >&2; exit 2; }
# python3 is required by the upstream U-Boot build; NabOS host helpers use Go.
for tool in sudo python3 curl xz tar sfdisk losetup e2fsck resize2fs genimage mkfs.vfat mkfs.ext4 mcopy mkimage mkenvimage rauc openssl patch jq; do
  command -v "$tool" >/dev/null || { echo "Missing host tool: $tool" >&2; exit 1; }
done
command -v "$GO" >/dev/null || { echo "Missing Go tool: $GO" >&2; exit 1; }
sudo -n true
lock=$repo/image/sources.lock.json
# Host-side helper (locked inputs, safe extraction); standard library only.
mkdir -p "$repo/build/iot"
nab_image=$repo/build/iot/nab-image
(cd "$repo/services" && GOTOOLCHAIN=local CGO_ENABLED=0 "$GO" build -o "$nab_image" ./cmd/nab-image)
[[ $("$GO" env GOVERSION) == "go$("$nab_image" get "$lock" tools.go)" ]] ||
  { echo 'Go toolchain mismatch' >&2; exit 1; }
keys=()
for key in arch compatible extract_sha256 kernel_image dtb; do keys+=("targets.$target.$key"); done
mapfile -t values < <("$nab_image" get "$lock" "${keys[@]}")
(( ${#values[@]} == ${#keys[@]} )) || { echo "Incomplete target lock: $target" >&2; exit 1; }
arch=${values[0]}; compatible=${values[1]}; image_hash=${values[2]}; kernel_image=${values[3]}; dtb=${values[4]}
mkdir -p "$repo/dist/$target"
work=$(mktemp -d "$repo/build/iot/$target.XXXXXX")
out=$repo/dist/$target
root=$work/root
payload=$work/payload
loop=
monitor=
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  if [[ -n $monitor ]]; then kill "$monitor" 2>/dev/null || true; wait "$monitor" 2>/dev/null || true; fi
  if mountpoint -q "$root"; then sudo umount --recursive "$root" || true; fi
  if [[ -n $loop ]] && sudo losetup --detach "$loop"; then loop=; fi
  if [[ -z $loop ]] && ! mountpoint -q "$root"; then rm -f "$work/builder.img"; fi
  echo "Build workspace: $work"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
mkdir -p "$payload/inputs" "$payload/src" "$work/images" "$work/boot" "$root"
# Sampling includes compiler and package-manager peaks, not just final artifact sizes.
(while :; do date -u +%FT%TZ; df -B1 --output=used,avail "$work"; sleep 10; done) > "$out/disk-usage-$target.txt" &
monitor=$!
if [[ -n $replay ]]; then
  "$nab_image" extract "$replay" "$payload/inputs"
  cmp "$lock" "$payload/inputs/sources.lock.json"
  cmp "$repo/core/Cargo.lock" "$payload/inputs/nab-hardware/Cargo.lock"
  cmp "$repo/services/go.sum" "$payload/inputs/go.sum"
  cmp "$repo/image/lva-requirements.lock" "$payload/inputs/lva-requirements.lock"
fi
# CI supplies per-component artifacts; local/replay builds use the same Make targets.
revision=$(git -C "$repo" rev-parse HEAD)
replay_inputs=
if [[ -n $replay ]]; then replay_inputs=$payload/inputs; fi
for component in go rust device-core uboot; do
  component_out=$work/components/$component
  if [[ -n $components ]]; then
    "$nab_image" extract "$components/$component-$target.tar" "$component_out"
    if [[ $component != device-core ]]; then
      printf '%s\n' "$target" "$revision" | cmp - "$component_out/build-info"
    fi
    if [[ $component == go ]]; then
      [[ $(cat "$component_out/version") == "$version" ]] || { echo 'Service version mismatch' >&2; exit 1; }
    fi
  else
    make -C "$repo" "$component" TARGET="$target" VERSION="$version" \
      OUT="$component_out" INPUTS="$replay_inputs"
  fi
  if [[ $component == device-core ]]; then
    "$nab_image" verify-device-core "$lock" "$component_out" "$target" "$revision"
  fi
  cp -a "$component_out/inputs/." "$payload/inputs/"
done
base_cached=false
base_cache_enabled=false
fetch_args=()
if [[ -z $replay && ${NABOS_BASE_CACHE:-1} == 1 ]]; then
  base_cache_enabled=true
  if bash "$repo/image/base-cache.sh" restore "$target" "$work"; then
    base_cached=true
    fetch_args=(--sources-only)
  fi
fi
"$nab_image" fetch "$lock" "$target" "$payload/inputs" "${fetch_args[@]}"
cp "$lock" "$payload/inputs/sources.lock.json"
cp "$repo/core/Cargo.lock" "$payload/inputs/nab-hardware/"
cp "$repo/services/go.sum" "$payload/inputs/"
cp "$repo/image/lva-requirements.lock" "$payload/inputs/"
"$nab_image" unpack "$payload/inputs/sources.lock.json" "$payload/inputs" "$payload/src"
cp -a "$repo/image" "$payload/image"
if ! $base_cached; then
  xz --decompress --stdout "$payload/inputs/raspios.img.xz" > "$work/base.img"
  printf '%s  %s\n' "$image_hash" "$work/base.img" | sha256sum --check
  rm "$payload/inputs/raspios.img.xz"
  # First sector of MBR partition 2: little-endian 32 bits at 446 + 16 + 8.
  root_start=$(od -An -tu4 --endian=little -j470 -N4 "$work/base.img")
  root_start=${root_start//[[:space:]]/}
  [[ $root_start =~ ^[1-9][0-9]*$ ]] || { echo 'Invalid base image partition table' >&2; exit 1; }
  truncate -s "$((root_start * 512 + 6 * 1024 * 1024 * 1024))" "$work/base.img"
  printf 'start=%s,size=%s\n' "$root_start" "$((6 * 1024 * 1024 * 1024 / 512))" | sfdisk --no-reread -N2 "$work/base.img"
  loop=$(sudo losetup --find --show --partscan "$work/base.img")
  sudo udevadm settle
  sudo e2fsck -pf "${loop}p2" || [[ $? == 1 ]]
  sudo resize2fs "${loop}p2"
  sudo losetup --detach "$loop"
  loop=
fi
mount_image() {
  loop=$(sudo losetup --find --show --partscan "$1")
  sudo udevadm settle
  sudo mount "${loop}p2" "$root"
  sudo mkdir -p "$root/boot/firmware" "$root/nabos-build"
  sudo mount "${loop}p1" "$root/boot/firmware"
  sudo mount --bind "$payload" "$root/nabos-build"
  sudo mount --rbind /dev "$root/dev"
  sudo mount --make-rslave "$root/dev"
  sudo mount -t proc proc "$root/proc"
  sudo mount -t tmpfs -o nosuid,nodev tmpfs "$root/run"
  sudo mount --rbind /sys "$root/sys"
  sudo mount --make-rslave "$root/sys"
  sudo rm -f "$root/etc/resolv.conf"
  sudo cp -L /etc/resolv.conf "$root/etc/resolv.conf"
}
unmount_image() {
  sudo umount --recursive "$root"
  sudo losetup --detach "$loop"
  loop=
}
if [[ $arch == armhf ]]; then
  sudo update-binfmts --enable qemu-arm
  qemu_cpu=arm1176
else
  [[ $(uname -m) == aarch64 ]] || { echo 'ARM64 image requires the standard native ARM64 runner' >&2; exit 1; }
  qemu_cpu=cortex-a53
fi
in_target() { sudo env QEMU_CPU="$qemu_cpu" chroot "$root" /bin/bash /nabos-build/image/prepare.sh "$1" "$target"; }
# Compile target-dependent components on a disposable copy of the pristine base.
# Added development packages and compiler caches remain in that copy.
if ! $base_cached; then
  echo "$(date -u +%FT%TZ) Building components in disposable image"
  cp --reflink=auto --sparse=always "$work/base.img" "$work/builder.img"
  mount_image "$work/builder.img"
  in_target build-packages
  in_target drivers
  in_target wheels
  unmount_image
  rm "$work/builder.img"

  echo "$(date -u +%FT%TZ) Assembling runtime image from archived packages and components"
  mount_image "$work/base.img"
  in_target packages
  sudo cp -a "$payload/runtime/." "$root/"
  sudo rm -rf "$payload/runtime"
  sudo fstrim "$root"
  unmount_image
  if $base_cache_enabled; then
    bash "$repo/image/base-cache.sh" save "$target" "$work"
  fi
fi
mount_image "$work/base.img"
sudo install -m755 "$work/components/rust/nab-hardware" "$root/usr/bin/nab-hardware"
sudo install -m755 "$work/components/device-core/device-core" "$root/usr/bin/device-core"
sudo install -Dm644 "$payload/src/device_core/LICENSE" "$root/usr/share/doc/device-core/copyright"
sudo install -Dm644 "$payload/src/device_core/NOTICE" "$root/usr/share/doc/device-core/NOTICE"
sudo install -m755 "$work/components/go/nabos" "$root/usr/bin/nabos"
# Git checkout ownership/umask must not grant the runner write access to system units.
tar --create --file=- --directory="$repo/image/rootfs" --owner=0 --group=0 --mode=go-w . |
  sudo tar --extract --file=- --directory="$root"
sudo mkdir -p "$root/usr/share/nabos/sounds" "$root/usr/share/nabos/choreographies" "$root/etc/nabos" "$root/etc/rauc"
sudo install -Dm644 "$repo/LICENSE" "$root/usr/share/doc/nabos/copyright"
sudo install -Dm644 "$repo/NOTICE" "$root/usr/share/doc/nabos/NOTICE"
for directory in "$repo/assets/sounds" "$repo/assets/choreographies"; do
  sudo cp -a --no-preserve=ownership "$directory/." "$root/usr/share/nabos/$(basename "$directory")/"
done
printf '%s\n' "$version" | sudo tee "$root/etc/nabos/release" >/dev/null
printf 'NABOS_VERSION=%s\nDEVICE_CORE_IMAGE_VERSION=%s\nDEVICE_CORE_UPDATE_REPO=%s\nDEVICE_CORE_UPDATE_ASSET=nabos-%s.raucb\n' \
  "$version" "$version" "${GITHUB_REPOSITORY:-guilhem/nabos}" "$target" | sudo tee "$root/etc/nabos/release.env" >/dev/null
if $development; then
  mkdir -m700 "$work/signing"
  openssl req -x509 -newkey rsa:3072 -nodes -days 7 -subj '/CN=NabOS development only/' \
    -keyout "$work/signing/key.pem" -out "$work/signing/cert.pem" 2>/dev/null
  signing_key=$work/signing/key.pem
  signing_cert=$work/signing/cert.pem
else
  signing_key=${RAUC_KEY:?RAUC_KEY must point to the release signing key}
  signing_cert=${RAUC_CERT:?RAUC_CERT must point to the trusted release certificate}
fi
sudo install -m644 "$signing_cert" "$root/etc/rauc/ca.cert.pem"
sudo sed -i "s/@COMPATIBLE@/$compatible/g" "$root/etc/rauc/system.conf"
in_target finalize
kernel=$(cat "$payload/kernel-release")
sudo ln -sfn /run/NetworkManager/resolv.conf "$root/etc/resolv.conf"

# The shared boot image contains no application kernel, overlays or modules.
for file in "$root"/boot/firmware/{bootcode.bin,start*.elf,fixup*.dat}; do
  [[ -f $file ]] && cp "$file" "$work/boot/"
done
cp "$work/components/uboot/u-boot.bin" "$work/boot/u-boot.bin"
cp "$root/boot/firmware/LICENCE.broadcom" "$work/boot/"
cp "$repo/image/boot/config.txt" "$work/boot/config.txt"
# Pristine DTBs for the firmware and U-Boot: the slot copy has Linux's profile
# (image/nabos-overlay.dts), which disables a UART.
cp "$root/boot/firmware/"*.dtb "$work/boot/"
sed -e "s/@TARGET@/$target/g" -e "s/@KERNEL_IMAGE@/$kernel_image/g" -e "s/@DTB@/$dtb/g" \
  "$repo/image/boot/boot.env.in" > "$work/boot/boot.env"
mkimage -A arm -T script -C none -n 'NabOS RAUC A/B' -d "$repo/image/boot/boot.cmd" "$work/boot/boot.scr"
cp "$root/usr/share/nabos/packages.tsv" "$out/packages-$target.tsv"
status=$(git status --porcelain)
dirty=false
if [[ -n $status ]]; then dirty=true; fi
# Every value is validated or generated above; none needs JSON escaping.
[[ $kernel =~ ^[a-zA-Z0-9.+_-]+$ ]] || { echo "Unexpected kernel release: $kernel" >&2; exit 1; }
printf '{\n  "version": "%s",\n  "target": "%s",\n  "kernel": "%s",\n  "source_revision": "%s",\n  "device_core_revision": "%s",\n  "device_core_archive_sha256": "%s",\n  "device_core_binary_sha256": "%s",\n  "source_dirty": %s,\n  "development": %s,\n  "hardware_validated": false\n}\n' \
  "$version" "$target" "$kernel" "$revision" \
  "$("$nab_image" get "$lock" sources.device_core.commit)" \
  "$("$nab_image" get "$lock" sources.device_core.sha256)" \
  "$(cat "$work/components/device-core/binary.sha256")" "$dirty" "$development" > "$out/build-$target.json"
sudo sync
# Remove the build mount point itself, not just its externally stored contents.
sudo umount "$root/nabos-build"
sudo rmdir "$root/nabos-build"
# Discard freed package blocks before copying the sparse filesystem.
sudo fstrim "$root"
sudo umount --recursive "$root"
sudo e2fsck -p "${loop}p2" || [[ $? == 1 ]]
sudo dd if="${loop}p2" of="$work/images/rootfs.ext4" bs=4M conv=sparse status=none
sudo chown "$(id -u):$(id -g)" "$work/images/rootfs.ext4"
sudo losetup --detach "$loop"
loop=
rm "$work/base.img"
truncate -s 256M "$work/images/boot.vfat"
mkfs.vfat -F32 -n NABOSBOOT "$work/images/boot.vfat"
mcopy -i "$work/images/boot.vfat" -s "$work/boot/"* ::
truncate -s 1G "$work/images/data.ext4"
mkfs.ext4 -q -F -L nabos-data "$work/images/data.ext4"
mkenvimage -r -s 0x10000 -o "$work/images/uboot.env" "$repo/image/boot/uboot.env"
[[ $(stat -c %s "$work/images/uboot.env") == 65536 ]]
[[ $(stat -c %s "$work/images/boot.vfat") == $((256 * 1024 * 1024)) ]]
[[ $(stat -c %s "$work/images/rootfs.ext4") == $((6 * 1024 * 1024 * 1024)) ]]
[[ $(stat -c %s "$work/images/data.ext4") == $((1024 * 1024 * 1024)) ]]
mkdir "$work/empty"
genimage --config "$repo/image/genimage.cfg" --rootpath "$work/empty" --inputpath "$work/images" --outputpath "$work/images" --tmppath "$work/genimage-tmp"
chmod a-w "$work/images/sdcard.img" "$work/images/rootfs.ext4" "$work/images/boot.vfat"
if $defer_tests; then
  echo 'Image verification deferred to the separate CI job'
else
  echo "$(date -u +%FT%TZ) Testing a disposable copy of the assembled SD image"
  bash "$repo/image/test.sh" "$target" "$work/images/sdcard.img" "$payload" "$work/components/uboot/u-boot.bin"
fi
sudo rm -rf "$payload/src/uboot"
mkdir "$work/bundle"
ln "$work/images/rootfs.ext4" "$work/bundle/rootfs.ext4"
ln "$work/images/boot.vfat" "$work/bundle/boot.vfat"
sed -e "s/@COMPATIBLE@/$compatible/g" -e "s/@VERSION@/$version/g" \
  "$repo/image/manifest.raucm.in" > "$work/bundle/manifest.raucm"
rauc bundle --mksquashfs-args="-comp zstd -Xcompression-level 15" \
  --cert="$signing_cert" --key="$signing_key" "$work/bundle" "$out/nabos-$target.raucb"
rauc info --keyring="$signing_cert" "$out/nabos-$target.raucb"
rauc info --keyring="$signing_cert" --output-format=json "$out/nabos-$target.raucb" |
  jq -e --arg compatible "$compatible" --arg version "$version" \
    --arg rootfs "$(sha256sum "$work/images/rootfs.ext4" | cut -d' ' -f1)" \
    --arg boot "$(sha256sum "$work/images/boot.vfat" | cut -d' ' -f1)" \
    '.compatible == $compatible and .version == $version and (.images | length) == 2 and
     .images[0].rootfs.filename == "rootfs.ext4" and .images[0].rootfs.checksum == $rootfs and
     .images[1].bootloader.filename == "boot.vfat" and .images[1].bootloader.checksum == $boot'
echo "$(date -u +%FT%TZ) Compressing SD image"
xz -T0 -6 --stdout "$work/images/sdcard.img" > "$out/nabos-$target.img.xz"
cp "$repo/image/sources.lock.json" "$out/sources-$target.lock.json"
dpkg-query -W -f='${Package}\t${Version}\t${Architecture}\n' > "$out/host-packages-$target.tsv"
echo "$(date -u +%FT%TZ) Archiving build inputs"
sudo tar -C "$payload/inputs" -I 'xz -T0 -6' -cf "$out/build-inputs-$target.tar.xz" .
echo "$(date -u +%FT%TZ) Finished compression"
sudo chown "$(id -u):$(id -g)" "$out/build-inputs-$target.tar.xz"
large=$(find "$out" -maxdepth 1 -type f -size +2147483647c -printf '%f\n')
[[ -z $large ]] || { echo "Exceeds the GitHub Release 2 GiB asset limit: $large" >&2; exit 1; }
(cd "$out" && sha256sum ./*.img.xz ./*.raucb ./*.tar.xz ./*.json ./*.tsv > "SHA256SUMS-$target")
echo "Built $version for $target in $out"
