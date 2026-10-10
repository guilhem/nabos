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
      # Ubuntu registers its arm-binfmt-P wrapper rather than qemu-arm-static.
      handler=/proc/sys/fs/binfmt_misc/qemu-arm
      if [[ ! -r $handler ]] || ! grep -qx enabled "$handler" || ! grep -Eq '^flags:.*F' "$handler"; then
        echo 'Need a runner-provided qemu-arm binfmt handler with flag F; this test never registers host handlers' >&2
        exit 1
      fi
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
# fsconfig limits string values to 256 bytes; use short paths relative to work.
if ! (cd "$work" && mount -n -t overlay -o "ro,nodev,nosuid,redirect_dir=on,metacopy=on,lowerdir=metadata::root${etc_paths[1]}" overlay "$root/etc"); then
  uname -r >&2
  dmesg | tail -20 >&2 || true
  exit 1
fi

ln -s "$system" "$root/run/current-system"
bash "$repo/image/test-generated-units.sh" "$1" "$root"

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
(cd "$work" && mount -n -t overlay -o "ro,nodev,nosuid,lowerdir=root/run/nabos-image-store:/nix/store" overlay "$root/nix/store")
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
# Initialize the seeded home as boot does, before the UID 1004 write check.
env -i "PATH=/run/nabos-test-tools:$system/sw/bin" \
  "$(command -v chroot)" "$root" /run/nabos-test-tools/systemd-tmpfiles --create --prefix=/var/lib/nabos
[[ $(stat -c '%u:%g:%a' "$root/var/lib/nabos") == 0:0:711 ]]
[[ $(stat -c '%u:%g:%a' "$root/var/lib/nabos/admin") == 1000:1000:700 ]]
[[ $(stat -c '%u:%g:%a' "$root/var/lib/nabos/lva") == 1004:1004:700 ]]
# Debian rc6 used the same backing paths and fixed service identities. Exercise
# existing state through NixOS tmpfiles and both mount maps, without copying it.
legacy_persist='/etc/NetworkManager/system-connections /var/lib/NetworkManager /var/lib/systemd/timesync /var/lib/tagtagtag-sound /var/lib/nabos'
[[ $PERSIST == "$legacy_persist" ]] || { echo 'Debian persistence map changed; migration needs qualification' >&2; exit 1; }
ssh-keygen -q -t ed25519 -N '' -f "$work/legacy-host-key"
cat > "$work/legacy-state.py" <<'PY'
import hashlib, json, os, stat, sys
from pathlib import Path

mode, root, snapshot = sys.argv[1:]
root, snapshot = Path(root), Path(snapshot)
if mode == 'seed':
    settings = {'version': 1, 'settings': {'locale': 'fr_FR', 'timezone': 'Europe/Paris',
        'volume': 42, 'auto_check_updates': True, 'voice_enabled': False,
        'updates': {'automatic': False, 'channel': 'test',
            'start': {'hour': 3, 'min': 0}, 'end': {'hour': 5, 'min': 0}}}}
    files = [
        ('/etc/NetworkManager/system-connections/home.nmconnection', 0, 0, 0o600,
         '[connection]\nid=home\nuuid=dc5b93e3-d6ac-430a-a0fe-7f8c094229c7\ntype=wifi\n[wifi]\nssid=Home\n[wifi-security]\nkey-mgmt=wpa-psk\npsk=saved-wifi-secret\n'),
        ('/var/lib/NetworkManager/NetworkManager.state', 0, 0, 0o600,
         '[main]\nNetworkingEnabled=true\nWirelessEnabled=false\n'),
        ('/var/lib/systemd/timesync/clock', 0, 0, 0o644, 'saved-clock\n'),
        ('/var/lib/tagtagtag-sound/mixer.conf', 0, 0, 0o644, 'headphone-low=220\nlineout-mode=headphone\n'),
        ('/var/lib/nabos/lva/preferences.json', 1004, 1004, 0o600,
         '{"active_wake_words":["okay_nabu"],"volume":0.42}\n'),
        ('/var/lib/nabos/.bash_history', 1000, 1000, 0o600, 'legacy-admin-history\n'),
        ('/data/nabos/application.json', 1001, 1001, 0o600,
         '{"version":1,"ears":[3,10],"home_assistant":{"host":"192.168.1.20","username":"legacy","password":"saved-secret"}}\n'),
        ('/data/nabos/media/sounds/user/legacy.wav', 1001, 1005, 0o640, 'saved-upload\n'),
        ('/data/device-core/settings.json', 1003, 1003, 0o600, json.dumps(settings) + '\n'),
        ('/data/device-core/updates/state.json', 1003, 1003, 0o600,
         '{"pending":null,"target":"v0.1.0-rc6","last_window":"2026-10-01","last_result":"","suspended":"","blocked":{"v0.1.0-rc5":"failed"}}\n'),
        ('/data/device-core/ssh/authorized_keys', 1003, 1003, 0o600,
         (snapshot.parent / 'legacy-host-key.pub').read_text()),
        ('/data/system/ssh/etc/ssh/ssh_host_ed25519_key', 0, 0, 0o600,
         (snapshot.parent / 'legacy-host-key').read_text()),
        ('/data/system/.data-grown', 0, 0, 0o644, ''),
        ('/data/system/machine-id', 0, 0, 0o644, '0123456789abcdef0123456789abcdef\n'),
    ]
    for name, uid, gid, permissions, contents in files:
        path = root / (('data/system' + name) if name.startswith(('/etc/', '/var/')) else name.lstrip('/'))
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(contents)
        path.chmod(permissions)
        os.chown(path, uid, gid)
    home = root / 'data/system/var/lib/nabos'
    home.chmod(0o711)
    os.chown(home, 1000, 1000)
    names = [row[0] for row in files] + ['/etc/machine-id', '/data/rauc/ca.cert.pem']
else:
    names = list(json.loads(snapshot.read_text()))
state = {}
for name in names:
    path = root / name.lstrip('/')
    info = path.stat()
    state[name] = [hashlib.sha256(path.read_bytes()).hexdigest(),
                   stat.S_IMODE(info.st_mode), info.st_uid, info.st_gid, info.st_ino]
if mode == 'seed':
    snapshot.write_text(json.dumps(state))
else:
    for name, expected in json.loads(snapshot.read_text()).items():
        assert state[name] == expected, (name, state[name], expected)
    # Read under the old numeric accounts, inside each root view; ancestor
    # directory permissions matter as well as the files' own metadata.
    for name, (_, _, uid, _, _) in state.items():
        child = os.fork()
        if child == 0:
            os.chroot(root)
            os.chdir('/')
            os.setgroups([1005] if uid in (1001, 1003) else [])
            os.setgid(uid)
            os.setuid(uid)
            Path(name).read_bytes()
            os._exit(0)
        assert os.waitpid(child, 0)[1] == 0, 'Legacy account cannot read ' + name
PY
python3 "$work/legacy-state.py" seed "$root" "$work/legacy-state.json"
[[ $(stat -c '%u:%g:%a' "$root/var/lib/nabos") == 1000:1000:711 ]]
for path in $PERSIST; do umount -n "$root$path"; done
bind_state
# shellcheck disable=SC2016
env -i "PATH=/run/nabos-test-tools:$system/sw/bin" \
  NABOS_PERSIST_LIB=/run/nabos-test-tools/persist.sh \
  "$(command -v chroot)" "$root" /run/nabos-test-tools/sh -ec \
  '. "$NABOS_PERSIST_LIB"; media_permissions'
env -i "PATH=/run/nabos-test-tools:$system/sw/bin" \
  "$(command -v chroot)" "$root" /run/nabos-test-tools/systemd-tmpfiles --create \
  --prefix=/data/nabos --prefix=/data/device-core --prefix=/data/rauc --prefix=/var/lib/nabos
[[ $(stat -c '%u:%g:%a' "$root/var/lib/nabos") == 0:0:711 ]]
python3 "$work/legacy-state.py" verify "$root" "$work/legacy-state.json"
# A RO rollback view uses Debian's original mounts, sharing the same p4 bytes.
# This proves the data contract, not execution of Debian services or its init.
rollback=$root/run/nabos-rollback
mkdir -p "$rollback/data" "$rollback/etc"
touch "$rollback/etc/machine-id"
for path in $legacy_persist; do mkdir -p "$rollback$path"; done
mount -n --bind "$rollback" "$rollback"
mount -n -o remount,bind,ro "$rollback"
mount -n --bind "$root/data" "$rollback/data"
for path in $legacy_persist; do
  mount -n --bind "$root/data/system$path" "$rollback$path"
done
mount -n --bind "$id" "$rollback/etc/machine-id"
mount -n -o remount,bind,ro "$rollback/etc/machine-id"
python3 "$work/legacy-state.py" verify "$rollback" "$work/legacy-state.json"
umount -n --recursive "$rollback"
echo 'PASS: legacy Wi-Fi, preferences, application data, SSH, trust and identity retain bytes/owners/modes through NixOS and rollback mappings'
env -i "PATH=/run/nabos-test-tools:$system/sw/bin" \
  "NABOS_SYSTEM=$system" "NABOS_TEST_LIBSYSTEMD=$systemd/lib/libsystemd.so.0" \
  "NABOS_RUNTIME_TARGET=$1" \
  NABOS_PERSIST_LIB=/run/nabos-test-tools/persist.sh QEMU_CPU=arm1176 \
  "$(command -v chroot)" "$root" /run/nabos-test-tools/python3 -B - < "$repo/image/test-service-accounts.py"
echo 'PASS: disposable NixOS RO-root guest checks (systemd confinement/initrd/hardware not exercised)'
