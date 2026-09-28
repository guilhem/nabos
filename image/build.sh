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
target=${1:?Usage: image/build.sh zero-armv6|zero2-arm64 VERSION [--development] [--replay INPUTS.tar.xz]}
version=${2:?release version required}
shift 2
development=false
replay=
while (( $# )); do
  case $1 in
    --development) development=true; shift ;;
    --replay) replay=$(realpath "${2:?archive required}"); shift 2 ;;
    *) echo "Unknown argument: $1" >&2; exit 2 ;;
  esac
done
[[ $target == zero-armv6 || $target == zero2-arm64 ]] || exit 2
[[ $version =~ ^[a-zA-Z0-9][a-zA-Z0-9.+_-]{0,63}$ ]] || { echo 'Invalid version' >&2; exit 2; }
for tool in sudo python3 curl xz tar sfdisk losetup e2fsck resize2fs genimage mkfs.vfat mkfs.ext4 mcopy mkimage mkenvimage rauc openssl clang ld.lld cargo go patch; do
  command -v "$tool" >/dev/null || { echo "Missing host tool: $tool" >&2; exit 1; }
done
sudo -n true
python3 - "$repo/image/sources.lock.json" <<'PY'
import json,subprocess,sys
versions=json.load(open(sys.argv[1]))['tools']
for tool,command in [('rust',['rustc','--version']),('go',['go','version'])]:
    actual=subprocess.check_output(command,text=True).split()
    found=actual[1] if tool=='rust' else actual[2].removeprefix('go')
    if found != versions[tool]:
        raise SystemExit(f'{tool}: expected {versions[tool]}, got {found}')
PY
mapfile -t values < <(python3 - "$repo/image/sources.lock.json" "$target" <<'PY'
import json,sys
lock=json.load(open(sys.argv[1])); t=lock['targets'][sys.argv[2]]
for key in ('arch','rust_target','goarch','goarm','compatible','extract_sha256','kernel_image','dtb'):
    print(t[key])
PY
)
arch=${values[0]}; rust_target=${values[1]}; goarch=${values[2]}; goarm=${values[3]}
compatible=${values[4]}; image_hash=${values[5]}; kernel_image=${values[6]}; dtb=${values[7]}
mkdir -p "$repo/build/iot" "$repo/dist/$target"
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
  if [[ -n $loop ]]; then sudo losetup --detach "$loop" || true; fi
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
  python3 - "$replay" "$payload/inputs" <<'PY'
import sys,tarfile
with tarfile.open(sys.argv[1], 'r:xz') as archive:
    archive.extractall(sys.argv[2], filter='data')
PY
  cmp "$repo/image/sources.lock.json" "$payload/inputs/sources.lock.json"
  cmp "$repo/core/Cargo.lock" "$payload/inputs/Cargo.lock"
  cmp "$repo/services/go.sum" "$payload/inputs/go.sum"
  cmp "$repo/image/lva-requirements.lock" "$payload/inputs/lva-requirements.lock"
fi
python3 "$repo/image/fetch.py" "$target" "$payload/inputs"
cp "$repo/image/sources.lock.json" "$payload/inputs/sources.lock.json"
cp "$repo/core/Cargo.lock" "$repo/services/go.sum" "$payload/inputs/"
cp "$repo/image/lva-requirements.lock" "$payload/inputs/"
python3 - "$repo/image" "$payload" <<'PY'
import sys,json
from pathlib import Path
sys.path.insert(0,sys.argv[1]); from fetch import unpack
payload=Path(sys.argv[2]); lock=json.loads((payload/'inputs/sources.lock.json').read_text())
for name in lock['sources']:
    archive=payload/'inputs'/f'{name}.tar.gz'
    if archive.exists():
        stage=payload/'src'/f'.{name}'
        unpack(archive,stage).rename(payload/'src'/name)
        stage.rmdir()
PY
cp -a "$repo/image" "$payload/image"
xz --decompress --stdout "$payload/inputs/raspios.img.xz" > "$work/base.img"
printf '%s  %s\n' "$image_hash" "$work/base.img" | sha256sum --check
rm "$payload/inputs/raspios.img.xz"
root_start=$(python3 - "$work/base.img" <<'PY'
import struct,sys
with open(sys.argv[1],'rb') as image:
    image.seek(446+16+8); print(struct.unpack('<I',image.read(4))[0])
PY
)
truncate -s "$((root_start * 512 + 6 * 1024 * 1024 * 1024))" "$work/base.img"
printf 'start=%s,size=%s\n' "$root_start" "$((6 * 1024 * 1024 * 1024 / 512))" | sfdisk --no-reread -N2 "$work/base.img"
loop=$(sudo losetup --find --show --partscan "$work/base.img")
sudo udevadm settle
sudo e2fsck -pf "${loop}p2" || [[ $? == 1 ]]
sudo resize2fs "${loop}p2"
sudo mount "${loop}p2" "$root"
sudo mkdir -p "$root/boot/firmware" "$root/nabos-build"
sudo mount "${loop}p1" "$root/boot/firmware"
sudo mount --bind "$payload" "$root/nabos-build"
sudo mount --rbind /dev "$root/dev"
sudo mount --make-rslave "$root/dev"
sudo mount -t proc proc "$root/proc"
sudo mount --rbind /sys "$root/sys"
sudo mount --make-rslave "$root/sys"
sudo rm -f "$root/etc/resolv.conf"
sudo cp -L /etc/resolv.conf "$root/etc/resolv.conf"
if [[ $arch == armhf ]]; then
  sudo update-binfmts --enable qemu-arm
  qemu_cpu=arm1176
else
  [[ $(uname -m) == aarch64 ]] || { echo 'ARM64 image requires the standard native ARM64 runner' >&2; exit 1; }
  qemu_cpu=cortex-a53
fi
in_target() { sudo env QEMU_CPU="$qemu_cpu" chroot "$root" /bin/bash /nabos-build/image/prepare.sh "$1" "$target"; }
in_target packages
# Execute boot.scr in U-Boot's actual parser, including both slot choices and
# failure paths. Build this out of tree before the native target U-Boot build.
make -C "$payload/src/uboot" O="$work/uboot-sandbox" sandbox_defconfig
"$payload/src/uboot/scripts/config" --file "$work/uboot-sandbox/.config" \
  -d SANDBOX_SDL -d TOOLS_MKEFICAPSULE -d UNIT_TEST -d EFI_CAPSULE_AUTHENTICATE \
  -d EFI_CAPSULE_ON_DISK -d CMD_UPL -d UPL
make -C "$payload/src/uboot" O="$work/uboot-sandbox" olddefconfig
make -C "$payload/src/uboot" O="$work/uboot-sandbox" -j2 CONFIG_PYLIBFDT= u-boot tools
in_target drivers
# Locked source archives are retained; release images do not need build objects.
sudo rm -rf "$payload/src/led-build"

# Cross-link against the image's libc and libgcc, never Ubuntu's ARMv7 runtime.
linker=$work/target-cc
python3 - "$linker" "$root" "$arch" <<'PY'
import pathlib,shlex,sys
path,root,arch=sys.argv[1:]
triple='arm-linux-gnueabihf' if arch=='armhf' else 'aarch64-linux-gnu'
cpu=['-mcpu=arm1176jzf-s','-mfpu=vfp','-mfloat-abi=hard'] if arch=='armhf' else ['-mcpu=cortex-a53']
command=['clang','--target='+triple,'--sysroot='+root,'--gcc-toolchain='+root+'/usr','-fuse-ld=lld']+cpu
pathlib.Path(path).write_text('#!/bin/sh\nexec '+shlex.join(command)+' "$@"\n')
pathlib.Path(path).chmod(0o755)
PY
linker_key=CARGO_TARGET_$(tr '[:lower:]-' '[:upper:]_' <<< "$rust_target")_LINKER
if [[ ! -d $payload/inputs/cargo-vendor ]]; then
  cargo vendor --locked --manifest-path "$repo/core/Cargo.toml" "$payload/inputs/cargo-vendor" > /dev/null
fi
env "$linker_key=$linker" CARGO_TARGET_DIR="$work/rust-target" \
  cargo --config 'source.crates-io.replace-with="vendored-sources"' \
    --config "source.vendored-sources.directory=\"$payload/inputs/cargo-vendor\"" \
    build --locked --offline --release --manifest-path "$repo/core/Cargo.toml" --target "$rust_target"
export GOMODCACHE="$payload/inputs/go-modcache"
if [[ -n $replay ]]; then export GOPROXY=off; else (cd "$repo/services" && go mod download); fi
(cd "$repo/services" && CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" GOARM="$goarm" \
  go build -trimpath -ldflags="-s -w -X main.version=$version" -o "$work/nab-service" ./cmd/nab-service)
sudo install -m755 "$work/rust-target/$rust_target/release/nab-core" "$root/usr/bin/nab-core"
sudo install -m755 "$work/nab-service" "$root/usr/bin/nab-service"
# Exercise the target binaries against a real broker before assembling artifacts.
# ARMv6 uses the actual image's loader/libc and an ARM1176 CPU, including Go's runtime.
python3 - "$work" "$root" "$arch" <<'PY'
import pathlib,shlex,sys
work,root,arch=sys.argv[1:]
for name in ['nab-core','nab-service']:
    prefix=[]
    if arch=='armhf':
        # QEMU's -L rewrites file lookups too, including /etc/machine-id.
        # Only the dynamically linked core needs the target sysroot; Go is static.
        prefix=['qemu-arm-static','-cpu','arm1176','-L',root if name=='nab-core' else '/']
    elif name=='nab-core':
        prefix=[root+'/usr/lib/aarch64-linux-gnu/ld-linux-aarch64.so.1',
                '--library-path',root+'/usr/lib/aarch64-linux-gnu']
    wrapper=pathlib.Path(work)/(name+'-test')
    wrapper.write_text('#!/bin/sh\nexec '+shlex.join(prefix+[root+'/usr/bin/'+name])+' "$@"\n')
    wrapper.chmod(0o755)
PY
NAB_CORE_BIN="$work/nab-core-test" NAB_SERVICE_BIN="$work/nab-service-test" \
  python3 "$repo/tools/integration.py"
rm -rf "$work/rust-target"
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
printf 'NABOS_VERSION=%s\nNABOS_UPDATE_REPO=%s\nNABOS_UPDATE_ASSET=nabos-%s.raucb\n' \
  "$version" "${GITHUB_REPOSITORY:-guilhem/nabos}" "$target" | sudo tee "$root/etc/nabos/release.env" >/dev/null
if [[ $arch == armhf ]]; then
  printf 'NABOS_LVA_UNIT=\n' | sudo tee -a "$root/etc/nabos/release.env" >/dev/null
fi
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
NABOS_UBOOT_SANDBOX="$work/uboot-sandbox" NABOS_SOURCES="$payload/src" \
  NABOS_VENDOR_DTBS="$root/boot/dtb" python3 -m unittest discover -s "$repo/image" -p 'test_*.py' -v
sudo rm -rf "$payload/src/uboot" "$work/uboot-sandbox"
sudo env QEMU_CPU="$qemu_cpu" chroot "$root" /usr/bin/nab-core --version
sudo env QEMU_CPU="$qemu_cpu" chroot "$root" /usr/bin/nab-service --version
kernel=$(cat "$payload/kernel-release")
for module in "$root/lib/modules/$kernel/updates/nabos/"*.ko; do
  vermagic=$(sudo env QEMU_CPU="$qemu_cpu" chroot "$root" modinfo -F vermagic "${module#"$root"}")
  [[ $vermagic == "$kernel "* ]] || { echo "Kernel mismatch: $module: $vermagic" >&2; exit 1; }
done
sudo ln -sfn /run/NetworkManager/resolv.conf "$root/etc/resolv.conf"

# The immutable firmware partition contains no application kernel or modules.
for file in "$root"/boot/firmware/{bootcode.bin,start*.elf,fixup*.dat}; do
  [[ -f $file ]] && cp "$file" "$work/boot/"
done
cp "$payload/u-boot.bin" "$work/boot/u-boot.bin"
cp "$root/boot/firmware/LICENCE.broadcom" "$work/boot/"
cp "$repo/image/boot/config.txt" "$work/boot/config.txt"
cp "$root/boot/dtb/"*.dtb "$work/boot/"
sed -e "s/@TARGET@/$target/g" -e "s/@KERNEL_IMAGE@/$kernel_image/g" -e "s/@DTB@/$dtb/g" \
  "$repo/image/boot/boot.env.in" > "$work/boot/boot.env"
mkimage -A arm -T script -C none -n 'NabOS RAUC A/B' -d "$repo/image/boot/boot.cmd" "$work/boot/boot.scr"
cp "$root/usr/share/nabos/packages.tsv" "$out/packages-$target.tsv"
python3 - "$version" "$target" "$payload/kernel-release" "$development" "$out/build-$target.json" <<'PY'
import json,pathlib,subprocess,sys
version,target,kernel,dev,out=sys.argv[1:]
revision=subprocess.check_output(['git','rev-parse','HEAD'],text=True).strip()
dirty=bool(subprocess.check_output(['git','status','--porcelain'],text=True))
pathlib.Path(out).write_text(json.dumps({'version':version,'target':target,'kernel':pathlib.Path(kernel).read_text().strip(),'source_revision':revision,'source_dirty':dirty,'development':dev=='true','hardware_validated':False},indent=2)+'\n')
PY
sudo sync
# Discard freed package/compiler blocks before copying the sparse filesystem.
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
mkdir "$work/empty"
genimage --config "$repo/image/genimage.cfg" --rootpath "$work/empty" --inputpath "$work/images" --outputpath "$work/images" --tmppath "$work/genimage-tmp"
mkdir "$work/bundle"
ln "$work/images/rootfs.ext4" "$work/bundle/rootfs.ext4"
printf '[update]\ncompatible=%s\nversion=%s\n\n[bundle]\nformat=verity\n\n[image.rootfs]\nfilename=rootfs.ext4\n' \
  "$compatible" "$version" > "$work/bundle/manifest.raucm"
rauc bundle --cert="$signing_cert" --key="$signing_key" "$work/bundle" "$out/nabos-$target.raucb"
rauc info --keyring="$signing_cert" "$out/nabos-$target.raucb"
xz -T2 --stdout "$work/images/sdcard.img" > "$out/nabos-$target.img.xz"
cp "$repo/image/sources.lock.json" "$out/sources-$target.lock.json"
dpkg-query -W -f='${Package}\t${Version}\t${Architecture}\n' > "$out/host-packages-$target.tsv"
sudo tar -C "$payload/inputs" -cJf "$out/build-inputs-$target.tar.xz" .
sudo chown "$(id -u):$(id -g)" "$out/build-inputs-$target.tar.xz"
python3 - "$out" <<'PY'
from pathlib import Path
import sys
for path in Path(sys.argv[1]).iterdir():
    if path.is_file() and path.stat().st_size >= 2**31:
        raise SystemExit(f'{path.name} exceeds the GitHub Release 2 GiB asset limit')
PY
(cd "$out" && sha256sum ./*.img.xz ./*.raucb ./*.tar.xz ./*.json ./*.tsv > "SHA256SUMS-$target")
echo "Built $version for $target in $out"
