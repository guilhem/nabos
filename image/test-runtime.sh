#!/usr/bin/env bash
# Exercise the exact NixOS closure with a genuinely RO root. No guest PID 1:
# systemd confinement, initrd/disk growth and hardware still need boot tests.
set -euo pipefail
if [[ $# == 4 && $1 == --namespace ]]; then
  shift
else
  [[ $# == 3 ]] || { echo 'Usage: bash image/test-runtime.sh TARGET ROOTFS_EXT4 DATA_EXT4' >&2; exit 2; }
  case $1 in
    zero2-arm64) [[ $(uname -m) == aarch64 ]] || { echo 'ARM64 runtime tests require native ARM64' >&2; exit 1; } ;;
    zero-armv6)
      [[ $(uname -m) == x86_64 ]] || { echo 'ARMv6 runtime tests require x86_64/QEMU' >&2; exit 1; }
      handler=false
      for entry in /proc/sys/fs/binfmt_misc/*; do
        if [[ -f $entry ]] && grep -q '^enabled$' "$entry" &&
            grep -Eq '^interpreter .*qemu-arm(-static)?$' "$entry" && grep -Eq '^flags: .*F' "$entry"; then
          handler=true
        fi
      done
      $handler || { echo 'Need a runner-provided qemu-arm binfmt handler with flag F; this test never registers host handlers' >&2; exit 1; }
      ;;
    *) echo "Unknown target: $1" >&2; exit 2 ;;
  esac
  [[ -f $2 && -f $3 ]] || { echo 'Expected regular extracted ext4 files, never block devices' >&2; exit 2; }
  exec sudo -n env "PATH=$PATH" unshare --mount --net --pid --fork --kill-child --propagation private \
    bash "$(realpath "$0")" --namespace "$1" "$(realpath "$2")" "$(realpath "$3")"
fi
[[ $EUID == 0 && $$ == 1 && -f $2 && -f $3 ]] || {
  echo 'Internal entrypoint requires root in the private PID namespace' >&2; exit 1;
}
case $1 in zero-armv6|zero2-arm64) ;; *) exit 2 ;; esac
repo=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d "$(dirname "$2")/runtime-$1.XXXXXX")
root=$work/root
cleanup() {
  local status=$? released=true
  trap - EXIT
  trap '' INT TERM
  if mountpoint -q "$root"; then umount -n --recursive "$root" || { released=false; status=1; }; fi
  if mountpoint -q "$work/metadata"; then umount -n "$work/metadata" || { released=false; status=1; }; fi
  if $released; then rm -rf -- "$work"; else echo "Unreleased test workspace: $work" >&2; fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
mkdir -p "$root" "$work/metadata"
# mount's auto-clearing loops also disappear if the namespace is killed.
# Copies prevent loop reuse with another reader of the supplied partitions.
cp --reflink=auto --sparse=always -- "$2" "$work/root.ext4"
mount -n -t ext4 -o loop,ro,noload "$work/root.ext4" "$root"
cp --reflink=auto --sparse=always -- "$3" "$work/data.ext4"
mount -n -t ext4 -o loop,rw,nosuid,nodev "$work/data.ext4" "$root/data"
for path in run var tmp; do
  mode=0755; [[ $path != tmp ]] || mode=1777
  mount -n -t tmpfs -o "nosuid,nodev,mode=$mode" tmpfs "$root/$path"
done
mount -n -t proc -o nosuid,nodev,noexec proc "$root/proc"
mount -n -t tmpfs -o mode=0755,nosuid tmpfs "$root/dev"
for entry in null:3 zero:5 random:8 urandom:9; do
  mknod -m 666 "$root/dev/${entry%:*}" c 1 "${entry#*:}"
done
mkdir -m 1777 "$root/dev/shm"
ln -s /proc/self/fd "$root/dev/fd"
for entry in stdin:0 stdout:1 stderr:2; do
  ln -s "/proc/self/fd/${entry#*:}" "$root/dev/${entry%:*}"
done
ip link set lo up
init=$(sed -n 's/^nabos_init=//p' "$root/boot/init")
[[ $init =~ ^/nix/store/[a-z0-9]{32}-[^/]+/init$ ]] || { echo 'Invalid shipped /boot/init' >&2; exit 1; }
system=${init%/init}
# Read bootspec from the image BEFORE adding host tools to the store.
readarray -t etc_paths < <(python3 - "$root$system/boot.json" <<'PY'
import json, re, sys
spec = json.load(open(sys.argv[1]))['org.nixos.nixos-init.v1']
for key in ('etc_metadata_image', 'etc_basedir'):
    path = spec[key]
    assert re.fullmatch(r'/nix/store/[a-z0-9]{32}-[^/\s]+', path), path
    print(path)
PY
)
[[ ${#etc_paths[@]} == 2 && -f $root${etc_paths[0]} && -d $root${etc_paths[1]} ]]
cp -- "$root${etc_paths[0]}" "$work/etc.erofs"
mount -n -t erofs -o loop,ro,nodev,nosuid "$work/etc.erofs" "$work/metadata"
mount -n -t overlay -o "ro,nodev,nosuid,redirect_dir=on,metacopy=on,lowerdir=$work/metadata::$root${etc_paths[1]}" overlay "$root/etc"

# Native test Python/systemd/D-Bus are test inputs, never product dependencies.
# Image paths win collisions, so product executables/configuration stay exact.
mkdir "$root/run/nabos-test-tools"
for tool in python3 setpriv busctl dbus-daemon systemd-tmpfiles sh cat sleep; do
  binary=$(realpath "$(command -v "$tool")")
  [[ $binary == /nix/store/* ]] || { echo "Run in the Nix dev shell: $tool is not a Nix test tool ($binary)" >&2; exit 1; }
  ln -s "$binary" "$root/run/nabos-test-tools/$tool"
done
busctl=$(readlink "$root/run/nabos-test-tools/busctl")
systemd=${busctl%/bin/busctl}
[[ -f $systemd/lib/libsystemd.so.0 ]]
mkdir "$root/run/nabos-image-store"
mount -n --bind "$root/nix/store" "$root/run/nabos-image-store"
mount -n -o remount,bind,ro "$root/run/nabos-image-store"
mount -n -t overlay -o "ro,nodev,nosuid,lowerdir=$root/run/nabos-image-store:/nix/store" overlay "$root/nix/store"
ln -s "$system" "$root/run/current-system"
touch "$root/run/nabos-test-tools/persist.sh"
mount -n --bind "$repo/nix/runtime/persist.sh" "$root/run/nabos-test-tools/persist.sh"
mount -n -o remount,bind,ro "$root/run/nabos-test-tools/persist.sh"
# shellcheck source=nix/runtime/persist.sh
. "$repo/nix/runtime/persist.sh"
seeds=("$root/run/nabos-image-store"/*-nabos-persistent-defaults)
[[ ${#seeds[@]} == 1 && -d ${seeds[0]} ]] || { echo 'Expected one shipped persistence seed closure' >&2; exit 1; }
bind_state() {
  local path store
  for path in $PERSIST; do
    store=$root/data/system$path
    if [[ ! -d $store ]]; then
      mkdir -p "${store%/*}"
      cp -a "${seeds[0]}$path" "$store.seed"
      mv "$store.seed" "$store"
    fi
    case $path in /var/*) mkdir -p "$root$path" ;; esac
    mount -n --bind "$store" "$root$path"
  done
}
bind_state
# Rebind existing state, as on the next boot; seeding must not overwrite it.
radio=$root/var/lib/NetworkManager/NetworkManager.state
grep -Eq '^WirelessEnabled=true$' "$radio"
printf '[main]\nWirelessEnabled=false\n' > "$radio"
printf 'persistent-home\n' > "$root/var/lib/nabos/.runtime-test"
for path in $PERSIST; do umount -n "$root$path"; done
bind_state
grep -Eq '^WirelessEnabled=false$' "$radio"
grep -qx 'persistent-home' "$root/var/lib/nabos/.runtime-test"
echo 'PASS: first-use state seeds and saved radio/home survive rebinding'
id=$root/data/system/machine-id
if ! grep -Eqx '[0-9a-f]{32}' "$id" 2>/dev/null; then
  tr -d '-' < /proc/sys/kernel/random/uuid > "$id"
  chmod 0444 "$id"
fi
mount -n --bind "$id" "$root/etc/machine-id"
mount -n -o remount,bind,ro "$root/etc/machine-id"
mkdir -p "$root/var/tmp" "$root/var/log"
chmod 1777 "$root/var/tmp"
env -i "PATH=/run/nabos-test-tools:$system/sw/bin" \
  "NABOS_SYSTEM=$system" "NABOS_TEST_LIBSYSTEMD=$systemd/lib/libsystemd.so.0" \
  "NABOS_RUNTIME_TARGET=$1" \
  NABOS_PERSIST_LIB=/run/nabos-test-tools/persist.sh QEMU_CPU=arm1176 \
  "$(command -v chroot)" "$root" /run/nabos-test-tools/python3 -B - < "$repo/image/test-service-accounts.py"
echo 'PASS: disposable NixOS RO-root guest checks (systemd confinement/initrd/hardware not exercised)'
