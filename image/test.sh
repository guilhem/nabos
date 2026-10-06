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
GO=${GO:-go}
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
for tool in sudo losetup udevadm mount mountpoint umount modinfo make setsid fdtget unshare ip; do
  command -v "$tool" >/dev/null || { echo "Missing host tool: $tool" >&2; exit 1; }
done
command -v "$GO" >/dev/null || { echo "Missing Go tool: $GO" >&2; exit 1; }
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
export TMPDIR="$work/tmp" GOMODCACHE="$work/go-modcache"
export GOCACHE=${GOCACHE:-$repo/build/cache/image-tests/$target/go}
mkdir -p "$GOCACHE"
export GOPROXY=off GOTOOLCHAIN=local GOENV=off GOWORK=off GOFLAGS='-mod=readonly -trimpath'
# Modules remain archived/offline inputs; upstream build writes stay disposable.
cp -a --reflink=auto "$payload/inputs/go-modcache" "$GOMODCACHE"
cp --reflink=auto --sparse=always -- "$sd_image" "$work/sdcard.img"
loop=$(sudo losetup --find --show --partscan "$work/sdcard.img")
sudo udevadm settle
sudo mount -o ro "${loop}p2" "$root"
sudo mount -o ro "${loop}p1" "$boot"
sudo mount -o rw "${loop}p4" "$root/data"
# Exercise runtime paths with the root still read-only.
sudo mount -t tmpfs -o nosuid,nodev,mode=0755 tmpfs "$root/run"
sudo mount -t proc proc "$root/proc"
sudo mount --rbind /dev "$root/dev"
sudo mount --make-rslave "$root/dev"
sudo env QEMU_CPU=arm1176 chroot "$root" /usr/bin/python3 -B - <<'PY'
import configparser
import ctypes
import errno
import fcntl
import json
import os
import subprocess
import time
from pathlib import Path

os.environ['LC_ALL'] = 'C'
unit = configparser.ConfigParser(strict=False)
unit.read('/usr/lib/systemd/system/nabos.service')
assert unit['Service']['AmbientCapabilities'] == 'CAP_NET_BIND_SERVICE'
assert 'NABOS_HTTP_ADDR=:80' in unit['Service']['Environment']
assert unit['Service']['CapabilityBoundingSet'] == 'CAP_NET_BIND_SERVICE'
assert unit['Service']['PrivateDevices'] == 'yes'
assert unit['Service']['ReadWritePaths'] == '/data/nabos'
assert unit['Service']['RuntimeDirectory'] == 'nabos'
assert unit['Service']['WorkingDirectory'] == '/run/nabos'
hardware_unit = configparser.ConfigParser(strict=False)
hardware_unit.read('/usr/lib/systemd/system/nab-hardware.service')
assert hardware_unit['Service']['Type'] == 'notify'
assert 'BusName' not in hardware_unit['Service']
for key, value in {'NotifyAccess': 'main', 'WatchdogSec': '1s',
                   'WatchdogSignal': 'SIGKILL', 'KillMode': 'control-group',
                   'ExecStopPost': '/usr/bin/nab-hardware --stop-hardware',
                   'TimeoutStopSec': '6s', 'Restart': 'always', 'RestartSec': '2',
                   'RuntimeDirectory': 'nab-hardware',
                   'WorkingDirectory': '/run/nab-hardware'}.items():
    assert hardware_unit['Service'][key] == value, key
assert 'HOME=/run/nab-hardware' in hardware_unit['Service']['Environment']
assert hardware_unit['Service'].get('AmbientCapabilities', '') == ''
assert hardware_unit['Service']['CapabilityBoundingSet'] == ''
assert hardware_unit['Service']['SupplementaryGroups'] == 'gpio'
led_paths = {f'/sys/class/leds/multi:indicator-{i}/{attr}' for i in range(5)
             for attr in ('brightness', 'multi_intensity')}
led_paths.add('/sys/class/leds/multi:indicator-0/sync')
assert set(hardware_unit['Service']['ReadWritePaths'].split()) == led_paths
for i in range(5):
    device = rf'sys-class-leds-multi:indicator\x2d{i}.device'
    for dependency in ('Requires', 'After'):
        assert device in hardware_unit['Unit'][dependency].split()
for dependency in ('Requires', 'After'):
    for name in ('dbus.socket', r'dev-i2c\x2d1.device'):
        assert name in hardware_unit['Unit'][dependency].split(), (dependency, name)
assert 'nabos-rfid.service' not in hardware_unit['Unit']['After']
assert Path('/etc/modules-load.d/nabos.conf').read_text().splitlines() == ['i2c-dev', 'bcm2835-ws2812']
udev = Path('/etc/udev/rules.d/60-nabos.rules').read_text().splitlines()
assert 'SUBSYSTEM=="i2c-dev", KERNEL=="i2c-1", GROUP:="nab-hardware", MODE:="0660", TAG+="systemd"' in udev
assert 'SUBSYSTEM=="gpio", KERNEL=="gpiochip*", GROUP="gpio", MODE="0660"' in udev
assert not any('KERNEL=="ear' in line or 'KERNEL=="mem"' in line or 'KERNEL=="vcio"' in line for line in udev)
assert any('SUBSYSTEM=="leds"' in line and 'SYSTEMD_ALIAS' in line for line in udev)
assert not any(Path('/usr/lib').rglob('libws2811.so*'))
assert any(Path('/lib/modules').rglob('bcm2835-ws2812.ko*'))
for name in ('tagtagtag-ears', 'ears', 'cr14', 'st25r391x'):
    assert not any(Path('/lib/modules').rglob(name + '.ko*')), name
    for directory in ('/boot/overlays', '/boot/firmware/overlays'):
        assert not Path(directory, name + '.dtbo').exists(), name
assert not Path('/usr/lib/nabos/rfid-probe').exists()
for directory in ('/usr/lib/systemd/system', '/etc/systemd/system', '/etc/systemd/system/multi-user.target.wants'):
    assert not os.path.lexists(directory + '/nabos-rfid.service')
for name in ('nabos', 'nab-hardware', 'device-core'):
    assert os.access('/usr/bin/' + name, os.X_OK), name
for name in ('nab-core', 'nab-service', 'mosquitto'):
    assert not Path('/usr/bin/' + name).exists(), name
    for directory in ('/usr/lib/systemd/system', '/etc/systemd/system'):
        assert not os.path.lexists(directory + '/' + name + '.service'), name
assert not Path('/usr/sbin/mosquitto').exists()
assert not Path('/etc/mosquitto').exists()
assert not Path('/etc/dbus-1/system.d/org.nabaztag.Core.conf').exists()
# Global groups must not give the application raw GPIO/video/memory access.
import grp
assert not {'gpio', 'video', 'kmem'} & {g.gr_name for g in grp.getgrall() if 'nab-app' in g.gr_mem}
import pwd
hardware_gid = grp.getgrnam('nab-hardware').gr_gid
for name in ('nabos', 'nab-app', 'device-core', 'nab-audio'):
    account = pwd.getpwnam(name)
    assert hardware_gid not in os.getgrouplist(name, account.pw_gid), name
assert Path('/etc/dbus-1/system.d/io.github.guilhem.NabHardware1.conf').is_file()
assert not Path('/etc/comitup.conf').exists()
assert not Path('/usr/share/comitup').exists()
device_unit = configparser.ConfigParser(strict=False)
device_unit.read('/usr/lib/systemd/system/device-core.service')
assert 'Environment=DEVICE_CORE_MAINTENANCE_USERS=nab-app:nab-hardware' in Path('/usr/lib/systemd/system/device-core.service').read_text().splitlines()
assert device_unit['Service']['PrivateDevices'] == 'yes'
assert device_unit['Service']['CapabilityBoundingSet'] == ''
release_env = dict(line.split('=', 1) for line in Path('/etc/nabos/release.env').read_text().splitlines())
has_lva = os.access('/opt/linux-voice-assistant/.venv/bin/python', os.X_OK)
assert release_env['DEVICE_CORE_LVA_UNIT'] == ('linux-voice-assistant.service' if has_lva else '')
# Resolve the shipped ALSA configuration, including the package's conf.d links.
# Both audio clients must reach PipeWire without opening a hardware device.
alsa = ctypes.CDLL('libasound.so.2')
assert alsa.snd_config_update() >= 0
alsa.snd_config_search.argtypes = [ctypes.c_void_p, ctypes.c_char_p, ctypes.POINTER(ctypes.c_void_p)]
alsa.snd_config_get_string.argtypes = [ctypes.c_void_p, ctypes.POINTER(ctypes.c_char_p)]
config = ctypes.c_void_p.in_dll(alsa, 'snd_config')
for key in (b'pcm.default.type', b'ctl.default.type'):
    node, value = ctypes.c_void_p(), ctypes.c_char_p()
    assert alsa.snd_config_search(config, key, ctypes.byref(node)) == 0, key
    assert alsa.snd_config_get_string(node, ctypes.byref(value)) == 0, key
    assert value.value == b'pipewire', (key, value.value)
# Check the installed packages, including bindings inherited from the Lite base.
# Jinja2 remains a dependency of cloud-init; Python also serves the voice assistant.
installed = {
    name.split(':')[0]
    for name, status in (line.split('\t') for line in subprocess.check_output(
        ['dpkg-query', '-W', '-f=${binary:Package}\t${db:Status-Status}\n'], text=True).splitlines())
    if status == 'installed'
}
obsolete = {
    'mosquitto', 'mosquitto-clients', 'comitup', 'python3-cachetools', 'python3-cairo', 'python3-flask',
    'python3-gi', 'python3-networkmanager', 'python3-dbus',
    'python3-click', 'python3-itsdangerous', 'python3-werkzeug',
    'python3-rpi-lgpio', 'python3-lgpio', 'liblgpio1',
}
assert not installed & obsolete, f'Obsolete runtime packages: {sorted(installed & obsolete)}'
assert Path('/usr/sbin/dnsmasq').is_file()
assert 'address=/#/10.41.0.1' in Path('/etc/NetworkManager/dnsmasq-shared.d/nabos.conf').read_text()
# Validate the actual dnsmasq configuration without opening a radio or listener.
subprocess.run(['dnsmasq', '--test', '--conf-file=/etc/NetworkManager/dnsmasq-shared.d/nabos.conf'], check=True)

# Matching the package rules at build time must avoid all writes to /etc at boot.
result = subprocess.run(['systemd-tmpfiles', '--create', '--prefix=/etc/mtab',
                         '--prefix=/etc/polkit-1/rules.d'], capture_output=True, text=True)
assert result.returncode == 0 and 'Read-only file system' not in result.stderr, result.stderr
assert Path('/etc/cloud/cloud-init.disabled').is_file()
assert Path('/etc/timezone').read_text() == 'Europe/Paris\n'
assert os.readlink('/etc/localtime') == '/usr/share/zoneinfo/Europe/Paris'
assert Path('/etc/localtime').is_file()
# Check the slot's directory before persist hides it with the data bind mount.
info = Path('/var/lib/NetworkManager').stat()
assert info.st_mode & 0o777 == 0o700, oct(info.st_mode)
assert (info.st_uid, info.st_gid) == (0, 0)
info = Path('/var/lib/NetworkManager/NetworkManager.state').stat()
assert (info.st_uid, info.st_gid) == (0, 0)
network = configparser.ConfigParser(interpolation=None)
network.read_string(subprocess.check_output(['/usr/sbin/NetworkManager', '--print-config'], text=True))
# The distribution supplies NetworkManager without a downstream rebuild.
assert '+nabos' not in subprocess.check_output(['dpkg-query', '-W', '-f=${Version}', 'network-manager'], text=True)

assert network['main']['rc-manager'] == 'unmanaged'
assert os.readlink('/etc/resolv.conf') == '/run/NetworkManager/resolv.conf'
radio = configparser.ConfigParser()
radio.read('/var/lib/NetworkManager/NetworkManager.state')
assert radio.getboolean('main', 'WirelessEnabled'), 'Wi-Fi must be enabled on first boot'
# Exercise first-boot seeding and a later boot with a saved radio preference.
seed = ('NABOS_BOOT_INIT_LIB=1 . /usr/lib/nabos/boot-init; '
        'PERSIST=/var/lib/NetworkManager; persist; mountpoint -q /var/lib/NetworkManager')
state = Path('/var/lib/NetworkManager/NetworkManager.state')
saved = Path('/data/system/var/lib/NetworkManager/NetworkManager.state')
default_state = state.read_text()
subprocess.run(['sh', '-c', seed], check=True)
assert saved.read_text() == default_state
saved.write_text('[main]\nWirelessEnabled=false\n')
subprocess.run(['umount', '/var/lib/NetworkManager'], check=True)
subprocess.run(['sh', '-c', seed], check=True)
radio.read(state)
assert not radio.getboolean('main', 'WirelessEnabled'), 'Saved radio preference must be preserved'
subprocess.run(['umount', '/var/lib/NetworkManager'], check=True)

# First-boot device state and the unchanged persistent home on a real read-only
# root. Simulation executes the shipped binary as device-core without capabilities;
# effective systemd sandboxing and hardware still require device qualification.
try:
    Path('/.nabos-readonly-probe').write_text('must fail')
except OSError as error:
    assert error.errno == errno.EROFS, error
else:
    raise AssertionError('The test root must actually be read-only')
subprocess.run(['sh', '-c', 'NABOS_BOOT_INIT_LIB=1 . /usr/lib/nabos/boot-init; '
                'PERSIST=/var/lib/nabos; persist; mountpoint -q /var/lib/nabos'], check=True)
subprocess.run(['systemd-tmpfiles', '--create', '--prefix=/data/device-core',
                '--prefix=/var/lib/nabos/lva', '--prefix=/run/lock/device-core'], check=True)
runtime = Path('/run/device-core')
runtime.mkdir(mode=0o700)
os.chown(runtime, 1003, 1003)
lock = Path('/run/lock/device-core/network')
assert lock.stat().st_mode & 0o777 == 0o600
assert lock.parent.stat().st_uid == 0 and lock.parent.stat().st_mode & 0o777 == 0o755
inode = lock.stat().st_ino
# A live FD deliberately survives the first daemon and protects the same inode.
guard = lock.open('r+')
fcntl.flock(guard, fcntl.LOCK_SH)
service = 'io.github.guilhem.DeviceCore1'
root_path = '/io/github/guilhem/DeviceCore1'
as_core = ['setpriv', '--reuid=1003', '--regid=1003', '--clear-groups',
            '--no-new-privs', '--bounding-set=-all']
env = dict(os.environ, HOME='/data/device-core', XDG_RUNTIME_DIR=str(runtime),
    DEVICE_CORE_DATA_DIR='/data/device-core',
    DEVICE_CORE_NETWORK_GUARD=str(lock), DEVICE_CORE_UPDATE_REPO='', DEVICE_CORE_UPDATE_ASSET='')
env.pop('DEVICE_CORE_HTTP_ADDR', None)
# A session bus admits its owner; exercise all peers as the service user.
address, pid = subprocess.check_output(as_core + ['dbus-daemon', '--session', '--fork',
    '--address=unix:path=/run/device-core/bus', '--print-address', '--print-pid'], env=env, text=True).splitlines()
env['DEVICE_CORE_BUS_ADDRESS'] = address
process = None
try:
    for boot in range(2):
        with (runtime / 'simulation.log').open('w') as log:
            process = subprocess.Popen(as_core + [
                '/usr/bin/device-core', '--simulate'], cwd=runtime, env=env,
                stdout=log, stderr=subprocess.STDOUT)
            deadline = time.monotonic() + 60
            while True:
                ready = subprocess.run(as_core + ['busctl', '--address=' + address, '--timeout=2',
                    'get-property', service, root_path, service + '.Manager', 'Ready'],
                    env=env, capture_output=True, text=True)
                if ready.returncode == 0 and ready.stdout.strip() == 'b true':
                    break
                assert process.poll() is None and time.monotonic() < deadline, (runtime / 'simulation.log').read_text()
                time.sleep(0.1)
            call = as_core + ['busctl', '--address=' + address, '--timeout=5', '--json=short',
                    'call', service, root_path + '/Config', service + '.Config']
            revision = json.loads(subprocess.check_output(call + ['Read'], text=True))['data'][0]
            if boot == 0:
                subprocess.run(call + ['Update', 's(ssub(bs(uu)(uu))b)', revision,
                    'fr_FR', 'Europe/Paris', '35', 'true', 'false', 'stable', '3', '0', '5', '0', 'true'], check=True)
                assert Path('/data/device-core/voice-enabled').is_file()
            else:
                assert revision != first_revision, 'A restart must invalidate old revisions'
                settings = json.loads(Path('/data/device-core/settings.json').read_text())
                assert settings['settings']['volume'] == 35 and settings['settings']['voice_enabled']
            first_revision = revision
            assert lock.stat().st_ino == inode, 'Restart replaced the network lock'
            process.terminate()
            process.wait(timeout=10)
            process = None
    subprocess.run(['setpriv', '--reuid=1004', '--regid=1004', '--clear-groups', '/bin/sh', '-ec',
        'touch /var/lib/nabos/lva/.image-write-check; rm /var/lib/nabos/lva/.image-write-check'], check=True)
finally:
    if process is not None:
        process.kill()
        process.wait()
    guard.close()
    os.kill(int(pid), 15)

for name in ('systemd-growfs-root.service', 'cloud-init-main.service', 'cloud-init-network.service',
             'bluetooth.service'):
    assert os.readlink('/etc/systemd/system/' + name) == '/dev/null', name

# Run the actual package generator: all backing-file writes must use /data.
assert os.readlink('/etc/systemd/system-generators/zram-generator') == '/dev/null'
assert os.access('/usr/lib/systemd/system-generators/zram-generator', os.X_OK)
assert 'zram' in Path('/usr/lib/modules-load.d/20-zram-generator.conf').read_text().splitlines()
generator = Path('/run/systemd/generator')
outputs = [generator, Path(str(generator) + '.early'), Path(str(generator) + '.late')]
for directory in outputs:
    directory.mkdir(parents=True)
subprocess.run(['/usr/lib/systemd/system-generators/rpi-swap-generator', *outputs],
               stdin=subprocess.DEVNULL, check=True)
assert (generator / 'swap.target.wants/dev-zram0.swap').is_symlink()
zram = Path('/run/systemd/zram-generator.conf.d/20-rpi-swap-zram0-ctrl.conf').read_text()
assert 'fs-type=swap' in zram and 'writeback-device=/dev/disk/by-backingfile/data-swap' in zram, zram
for unit in ('rpi-resize-swap-file.service', 'rpi-setup-loop@data-swap.service'):
    dropins = '\n'.join(p.read_text() for p in (generator / (unit + '.d')).glob('*.conf'))
    assert 'RequiresMountsFor=/data/swap' in dropins, dropins
for path in Path('/run/systemd').rglob('*'):
    text = str(path)
    if path.is_symlink():
        text += os.readlink(path)
    elif path.is_file():
        text += path.read_text()
    assert '/var/swap' not in text and 'var-swap' not in text, path

# Create and reuse the real file on the disposable data partition, without
# activating swap or attaching any loop device to the host kernel.
swap = Path('/data/swap')
assert not swap.exists()
for attempt in range(2):
    subprocess.run(['/usr/lib/rpi-swap/bin/rpi-resize-swap-file'], stdin=subprocess.DEVNULL, check=True)
    assert swap.stat().st_size > 0 and swap.stat().st_mode & 0o777 == 0o600
    assert subprocess.check_output(['blkid', '-p', '-s', 'TYPE', '-o', 'value', swap], text=True).strip() == 'swap'
assert not Path('/var/swap').exists()
PY
# Exercise shipped authorization with real UIDs, without exposing host services
# or devices. Mount changes and child processes die with this nested namespace.
sudo unshare --mount --net --pid --fork bash -s -- "$root" "$repo/image/test-service-accounts.py" <<'SH'
set -euo pipefail
mount --make-rprivate /
mount -t proc proc "$1/proc"
mount -t tmpfs -o mode=0755,nosuid tmpfs "$1/dev"
for entry in null:3 zero:5 random:8 urandom:9; do
  mknod -m 666 "$1/dev/${entry%:*}" c 1 "${entry#*:}"
done
env QEMU_CPU=arm1176 chroot "$1" /usr/bin/python3 -B - < "$2"
SH
cmp "$expected_uboot" "$boot/u-boot.bin"
# Parse the shipped OpenSSH configuration (including inherited snippets) without
# host keys: these are deliberately absent until the user enables SSH.
ssh_config=$(sudo env QEMU_CPU=arm1176 chroot "$root" /usr/sbin/sshd -G)
for setting in 'allowusers nabos' 'permitrootlogin no' 'authenticationmethods publickey' \
  'passwordauthentication no' 'kbdinteractiveauthentication no' 'usepam yes' \
  'strictmodes yes' 'authorizedkeysfile none' 'authorizedkeyscommand /usr/bin/cat /data/device-core/ssh/authorized_keys' 'authorizedkeyscommanduser device-core'; do
  grep -qxF "$setting" <<< "$ssh_config" || { echo "Unexpected SSH configuration: $setting" >&2; exit 1; }
done
[[ $(sudo chroot "$root" getent passwd nabos) == 'nabos:x:1000:1000:'*':/var/lib/nabos:/bin/bash' ]] ||
  { echo 'Unexpected SSH account' >&2; exit 1; }
sudo env QEMU_CPU=arm1176 chroot "$root" /usr/sbin/visudo --check
for path in /nabos-build /usr/bin/gcc /usr/bin/make /usr/bin/cmake \
  /usr/sbin/policy-rc.d /etc/apt/apt.conf.d/99nabos-build \
  /usr/bin/cargo /usr/bin/rustc /usr/local/go /root/.cargo /root/.rustup \
  /root/go /root/.cache /build/device-core; do
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

if [[ -n ${NABOS_UBOOT_SANDBOX:-} ]]; then
  sandbox=$(realpath "$NABOS_UBOOT_SANDBOX")
  (cd "$sandbox" && sha256sum --check --strict SHA256SUMS)
  for binary in u-boot scripts/dtc/dtc tools/mkimage tools/mkenvimage; do
    [[ -x $sandbox/$binary ]] || { echo "Missing sandbox executable: $binary" >&2; exit 1; }
  done
  cp -a "$sandbox/." "$work/uboot-sandbox"
  echo 'Using the prebuilt U-Boot test sandbox'
else
  cp -a --reflink=auto "$payload/src/uboot" "$work/uboot-src"
  cc=gcc
  if command -v ccache >/dev/null; then
    export CCACHE_DIR=${CCACHE_DIR:-$repo/build/cache/image-tests/$target/ccache}
    export CCACHE_BASEDIR=$work CCACHE_COMPILERCHECK=content CCACHE_TEMPDIR=$work/tmp/ccache
    mkdir -p "$CCACHE_DIR" "$CCACHE_TEMPDIR"
    cc="ccache $cc"
  fi
  CC="$cc" bash "$repo/image/uboot-sandbox.sh" "$work/uboot-src" "$work/uboot-sandbox"
fi

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
# Linux gets the hardware profile (image/nabos-overlay.dts); the firmware and
# U-Boot keep the kernel package's pristine DTB, with its UART and watchdog.
dtb=$(sed -n 's/^nabos_dtb=//p' "$boot/boot.env")
# Newer kernels make linux-image-$kernel a symlink into modules/$kernel/dtb.
pristine=$(find -H "$root/usr/lib/linux-image-$kernel" -name "$dtb" -print -quit)
cmp "$boot/$dtb" "$pristine"
[[ $(fdtget -d okay "$boot/$dtb" /soc/watchdog@7e100000 status) == okay ]] ||
  { echo "Watchdog disabled in $dtb" >&2; exit 1; }
for node in /soc/serial@7e201000 /soc/fb /soc/mailbox@7e00b840 /soc/usb@7e980000 /cam1_regulator /cam_dummy_reg; do
  [[ $(fdtget "$root/boot/dtb/$dtb" "$node" status) == disabled ]] || { echo "Profile not applied: $node" >&2; exit 1; }
done
# The sound overlay resolves its targets through these symbols.
fdtget "$root/boot/dtb/$dtb" /__symbols__ i2s /__symbols__ i2c1 /__symbols__ gpio /__symbols__ sound >/dev/null
i2c1=$(fdtget "$root/boot/dtb/$dtb" /__symbols__ i2c1)
[[ $(fdtget "$root/boot/dtb/$dtb" "$i2c1" status) == okay ]] ||
  { echo 'I2C bus 1 disabled in the Linux slot DTB' >&2; exit 1; }

# The LED controller is already part of the Linux slot DTB.
pwm=$(fdtget "$root/boot/dtb/$dtb" /__symbols__ pwm)
[[ $(fdtget "$root/boot/dtb/$dtb" "$pwm" compatible) == guilhem,bcm2835-ws2812 ]]
[[ $(fdtget "$root/boot/dtb/$dtb" "$pwm" status) == okay ]]
[[ $(fdtget "$root/boot/dtb/$dtb" "$pwm" dma-names) == tx ]]

# Use the shipped loader/libc for Rust; the Go application is static.
# Variables in the generated wrappers are resolved when the child runs.
# shellcheck disable=SC2016
for name in nab-hardware device-core nabos; do
  {
    printf '#!/bin/bash\nroot=$(cd -- "$(dirname -- "$0")/root" && pwd)\nexec '
    if [[ $target == zero-armv6 ]]; then
      if [[ $name == nabos ]]; then
        printf 'qemu-arm-static -cpu arm1176 -L / '
      else
        printf 'qemu-arm-static -cpu arm1176 -L "$root" '
      fi
    elif [[ $name != nabos ]]; then
      printf '"$root/usr/lib/aarch64-linux-gnu/ld-linux-aarch64.so.1" --library-path "$root/usr/lib/aarch64-linux-gnu" '
    fi
    printf '"$root/usr/bin/%s" "$@"\n' "$name"
  } > "$work/$name-test"
  chmod 755 "$work/$name-test"
done
# The sandbox simulates boot decisions; it does not boot a kernel or hardware.
export NABOS_INTEGRATION=1 NABOS_HARDWARE_BIN="$work/nab-hardware-test" NABOS_BIN="$work/nabos-test" DEVICE_CORE_BIN="$work/device-core-test"
export NABOS_TEST_ASSETS="$root/usr/share/nabos" NABOS_UBOOT_SANDBOX="$work/uboot-sandbox" NABOS_SOURCES="$payload/src"
export NABOS_VENDOR_DTBS="$root/boot/dtb" NABOS_IMAGE_OVERLAYS="$root/boot/overlays"
export NABOS_IMAGE_BOOT="$boot" NABOS_IMAGE_ENV="$work/uboot.env" NABOS_IMAGE_TARGET="$target"
export NABOS_IMAGE_DISK="$work/sdcard.img"
cd "$repo/services"
setsid "$GO" test -count=1 -timeout 20m -skip '^(TestDeviceCore|TestEndToEnd)$' -v ./tests/integration ./tests/image &
tests_pid=$!
wait "$tests_pid"
tests_pid=
# Daemon tests need the same account database as the shipped libc (also under
# QEMU). Build on the host, then use image accounts in a private namespace.
"$GO" test -c -o "$work/product-test" ./tests/integration
chmod 0755 "$work"
sudo --preserve-env unshare --mount --net --pid --fork --kill-child --propagation private \
  bash -s -- "$work" "$work/product-test" <<'SH'
set -euo pipefail
ip link set lo up
mount -t proc proc /proc
mount --bind "$1/root/etc/passwd" /etc/passwd
mount --bind "$1/root/etc/group" /etc/group
# Expose only this disposable tree through a traversable path. The caller's
# home/build ancestors retain their permissions.
exposed=$(mktemp -d /tmp/nabos-image-e2e.XXXXXX)
trap 'umount --recursive "$exposed"; rmdir "$exposed"' EXIT
mount --rbind "$1" "$exposed"
export TMPDIR="$exposed/tmp" NABOS_TEST_ASSETS="$exposed/root/usr/share/nabos"
export NABOS_BIN="$exposed/nabos-test" NABOS_HARDWARE_BIN="$exposed/nab-hardware-test"
export DEVICE_CORE_BIN="$exposed/device-core-test"
"$2" -test.run '^(TestDeviceCore|TestEndToEnd)$' -test.count=1 -test.v -test.timeout=10m
SH
