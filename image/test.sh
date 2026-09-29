#!/bin/bash
# Verify a disposable copy of the assembled image; no toolchain enters the image.
set -euo pipefail
[[ $# == 4 ]] || { echo 'Usage: image/test.sh TARGET SD_IMAGE PAYLOAD EXPECTED_UBOOT' >&2; exit 2; }
target=$1
case $target in
  zero-armv6) flavour=rpi-v6 ;;
  zero2-arm64) flavour=rpi-v8 ;;
  *) echo "Unknown target: $target" >&2; exit 2 ;;
esac
# Also allow standalone use, with the same private mount namespace as build.sh.
if [[ ${NABOS_BUILD_NAMESPACE:-} != 1 ]]; then
  exec sudo --preserve-env env "PATH=$PATH" NABOS_BUILD_NAMESPACE=1 \
    unshare --mount --propagation private \
    setpriv --reuid="$(id -u)" --regid="$(id -g)" --init-groups \
    bash "$0" "$@"
fi
repo=$(cd "$(dirname "$0")/.." && pwd)
sd_image=$(realpath "$2")
payload=$(realpath "$3")
expected_uboot=$(realpath "$4")
[[ -f $sd_image && -s $expected_uboot && -d $payload/src/uboot && -d $payload/inputs/go-modcache ]]
before=$(sha256sum -- "$sd_image")
work=
loop=
tests_pid=
cleanup() {
  local status=$? released=true after directory
  trap - EXIT
  trap '' INT TERM
  # The harness stops its children normally; also cover Go timeouts/signals.
  if [[ -n $tests_pid ]]; then
    kill -KILL -- "-$tests_pid" 2>/dev/null || true
    wait "$tests_pid" 2>/dev/null || true
  fi
  if [[ -n $work ]]; then
    for directory in "$work/boot" "$work/root"; do
      if mountpoint -q "$directory"; then
        if ! sudo umount --recursive "$directory"; then released=false; status=1; fi
      fi
    done
  fi
  if [[ -n $loop ]]; then
    if ! sudo losetup --detach "$loop"; then released=false; status=1; fi
  fi
  if [[ -n $work ]]; then
    if $released; then
      sudo rm -rf -- "$work" || status=1
    else
      echo "Could not release test mounts/device; workspace retained: $work" >&2
    fi
  fi
  if ! after=$(sha256sum -- "$sd_image") || [[ $after != "$before" ]]; then
    echo "Original SD image changed or cannot be read: $sd_image" >&2
    status=1
  else
    echo "Original SD image unchanged: ${before%% *}"
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
for tool in sudo losetup udevadm mount mountpoint umount modinfo make go setsid; do
  command -v "$tool" >/dev/null || { echo "Missing host tool: $tool" >&2; exit 1; }
done
sudo -n true
if [[ $target == zero-armv6 ]]; then
  command -v qemu-arm-static >/dev/null
else
  [[ $(uname -m) == aarch64 ]] || { echo 'ARM64 verification requires a native ARM64 host' >&2; exit 1; }
fi
mkdir -p "$repo/build/iot"
work=$(mktemp -d "$repo/build/iot/test-$target.XXXXXX")
root=$work/root
boot=$work/boot
mkdir -p "$root" "$boot" "$work/tmp"
export TMPDIR="$work/tmp" GOCACHE="$work/go-cache" GOMODCACHE="$work/go-modcache"
export GOPROXY=off GOTOOLCHAIN=local GOENV=off GOWORK=off GOFLAGS=-mod=readonly
# Keep build caches and any upstream build writes disposable as well.
cp -a --reflink=auto "$payload/inputs/go-modcache" "$GOMODCACHE"
cp -a --reflink=auto "$payload/src/uboot" "$work/uboot-src"
cp --reflink=auto --sparse=always -- "$sd_image" "$work/sdcard.img"
loop=$(sudo losetup --find --show --partscan "$work/sdcard.img")
sudo udevadm settle
sudo mount -o ro "${loop}p2" "$root"
sudo mount -o ro "${loop}p1" "$boot"
# Exercise the shipped GPIO import with the root still read-only. systemd creates
# RuntimeDirectory before applying WorkingDirectory; reproduce those two steps.
sudo mount -t tmpfs -o nosuid,nodev,mode=0755 tmpfs "$root/run"
sudo mount -t proc proc "$root/proc"
sudo env QEMU_CPU=arm1176 chroot "$root" /usr/bin/python3 -B - <<'PY'
import configparser
import os
import subprocess
from pathlib import Path

os.environ['LC_ALL'] = 'C'
unit = configparser.ConfigParser()
unit.read('/etc/systemd/system/comitup.service.d/nabos.conf')
service = unit['Service']
runtime = Path('/run') / service['RuntimeDirectory']
runtime.mkdir()
os.chdir(service['WorkingDirectory'])
# Import the failing dependency directly: RPi.GPIO also requires a real Pi DTB.
import lgpio  # creates a notification FIFO in cwd, without opening a GPIO chip

# Matching the package rules at build time must avoid all writes to /etc at boot.
result = subprocess.run(['systemd-tmpfiles', '--create', '--prefix=/etc/mtab',
                         '--prefix=/etc/polkit-1/rules.d'], capture_output=True, text=True)
assert result.returncode == 0 and 'Read-only file system' not in result.stderr, result.stderr
assert Path('/etc/cloud/cloud-init.disabled').is_file()
for name in ('systemd-growfs-root.service', 'cloud-init-main.service', 'cloud-init-network.service',
             'bluetooth.service'):
    assert os.readlink('/etc/systemd/system/' + name) == '/dev/null', name

# Run the actual package generator: its output must never ask for a file-backed
# swap or for removal of an inherited /var/swap on the read-only root.
generator = Path('/run/systemd/generator')
outputs = [generator, Path(str(generator) + '.early'), Path(str(generator) + '.late')]
for directory in outputs:
    directory.mkdir(parents=True)
subprocess.run(['/usr/lib/systemd/system-generators/rpi-swap-generator', *outputs], check=True)
assert (generator / 'swap.target.wants/dev-zram0.swap').is_symlink()
assert 'fs-type=swap' in Path('/run/systemd/zram-generator.conf.d/20-rpi-swap-zram0-ctrl.conf').read_text()
for path in Path('/run/systemd').rglob('*'):
    text = str(path)
    if path.is_symlink():
        text += os.readlink(path)
    elif path.is_file():
        text += path.read_text()
    assert not any(word in text for word in ('backingfile', 'rpi-setup-loop', 'rpi-resize-swap',
                                            'rpi-remove-swap', 'writeback', '/var/swap')), path
PY
cmp "$expected_uboot" "$boot/u-boot.bin"
[[ -x $root/usr/bin/dtoverlay ]] || { echo 'Missing runtime dtoverlay command' >&2; exit 1; }
# Parse the shipped OpenSSH configuration (including inherited snippets) without
# host keys: these are deliberately absent until the user enables SSH.
ssh_config=$(sudo env QEMU_CPU=arm1176 chroot "$root" /usr/sbin/sshd -G)
for setting in 'allowusers nabos' 'permitrootlogin no' 'authenticationmethods publickey' \
  'passwordauthentication no' 'kbdinteractiveauthentication no' 'usepam yes' \
  'strictmodes yes' 'authorizedkeysfile /data/nabos/ssh/authorized_keys'; do
  grep -qxF "$setting" <<< "$ssh_config" || { echo "Unexpected SSH configuration: $setting" >&2; exit 1; }
done
[[ $(sudo chroot "$root" getent passwd nabos) == 'nabos:x:1000:1000:'*':/var/lib/nabos:/bin/bash' ]] ||
  { echo 'Unexpected SSH account' >&2; exit 1; }
sudo env QEMU_CPU=arm1176 chroot "$root" /usr/sbin/visudo --check
for path in /nabos-build /usr/bin/gcc /usr/bin/make /usr/bin/cmake; do
  [[ ! -e $root$path && ! -L $root$path ]] || { echo "Build artifact shipped: $path" >&2; exit 1; }
done
if [[ $target == zero2-arm64 ]]; then
  sudo chroot "$root" /bin/sh -c 'cd /opt/linux-voice-assistant && exec .venv/bin/python -E -B -' <<'PY'
from importlib.metadata import version
from pathlib import Path
from linux_voice_assistant import util

base = Path.cwd()
assert Path(util.__file__).resolve() == base / "linux_voice_assistant/util.py"
assert util.get_version() == (base / "version.txt").read_text().strip() != "unknown"
assert util.get_version()
assert (base / "sounds").is_dir() and (base / "wakewords").is_dir()
print({p: version(p) for p in ("linux-voice-assistant", "aioesphomeapi", "soundcard")})
PY
fi
# Both raw redundant environments must be the same initial image (genimage.cfg).
dd if="$work/sdcard.img" of="$work/uboot.env" bs=65536 skip=16 count=1 status=none
dd if="$work/sdcard.img" of="$work/uboot-redund.env" bs=65536 skip=32 count=1 status=none
cmp "$work/uboot.env" "$work/uboot-redund.env"

make -C "$work/uboot-src" O="$work/uboot-sandbox" CROSS_COMPILE= sandbox_defconfig
"$work/uboot-src/scripts/config" --file "$work/uboot-sandbox/.config" \
  -d SANDBOX_SDL -d TOOLS_MKEFICAPSULE -d UNIT_TEST -d EFI_CAPSULE_AUTHENTICATE \
  -d EFI_CAPSULE_ON_DISK -d CMD_UPL -d UPL
make -C "$work/uboot-src" O="$work/uboot-sandbox" CROSS_COMPILE= olddefconfig
make -C "$work/uboot-src" O="$work/uboot-sandbox" CROSS_COMPILE= -j"$(nproc)" CONFIG_PYLIBFDT= u-boot tools
[[ -x $work/uboot-sandbox/u-boot ]]

# Inspect modules from the cloned root, without using the host's kernel release.
shopt -s nullglob
module_dirs=("$root"/lib/modules/*-"$flavour"/updates/nabos)
(( ${#module_dirs[@]} == 1 )) || { echo 'Expected one shipped NabOS kernel module directory' >&2; exit 1; }
kernel=${module_dirs[0]%/updates/nabos}
kernel=${kernel##*/}
modules=("${module_dirs[0]}/"*.ko)
(( ${#modules[@]} > 0 )) || { echo 'No shipped NabOS modules' >&2; exit 1; }
for module in "${modules[@]}"; do
  vermagic=$(modinfo -F vermagic "$module")
  [[ $vermagic == "$kernel "* ]] || { echo "Kernel mismatch: $module: $vermagic" >&2; exit 1; }
done

# Use the shipped loader/libc for the core; the Go service is static.
for name in nab-core nab-service; do
  prefix=()
  if [[ $target == zero-armv6 ]]; then
    sysroot=/
    if [[ $name == nab-core ]]; then sysroot=$root; fi
    prefix=(qemu-arm-static -cpu arm1176 -L "$sysroot")
  elif [[ $name == nab-core ]]; then
    prefix=("$root/usr/lib/aarch64-linux-gnu/ld-linux-aarch64.so.1" --library-path "$root/usr/lib/aarch64-linux-gnu")
  fi
  printf '#!/bin/bash\nexec %s"$@"\n' "$(printf '%q ' "${prefix[@]}" "$root/usr/bin/$name")" > "$work/$name-test"
  chmod 755 "$work/$name-test"
done
# The sandbox simulates boot decisions; it does not boot a kernel or hardware.
export NABOS_INTEGRATION=1 NAB_CORE_BIN="$work/nab-core-test" NAB_SERVICE_BIN="$work/nab-service-test"
export NABOS_TEST_ASSETS="$root/usr/share/nabos" NABOS_UBOOT_SANDBOX="$work/uboot-sandbox" NABOS_SOURCES="$payload/src"
export NABOS_VENDOR_DTBS="$root/boot/dtb" NABOS_IMAGE_OVERLAYS="$root/boot/overlays"
export NABOS_IMAGE_BOOT="$boot" NABOS_IMAGE_ENV="$work/uboot.env" NABOS_IMAGE_TARGET="$target"
cd "$repo/services"
setsid go test -count=1 -timeout 20m -v ./tests/integration ./tests/image &
tests_pid=$!
wait "$tests_pid"
