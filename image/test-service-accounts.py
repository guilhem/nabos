#!/usr/bin/env python3
"""Run as root in a disposable image: read-only /, writable /data and /run,
private tmpfs /dev without sound devices, and shipped binaries/accounts/rules.
The caller supplies isolated mount/network namespaces; no host bus is used.
"""
import os
import configparser
import errno
import filecmp
import fcntl
import json
import pwd
import grp
import select
import shlex
import signal
import subprocess as sp
import sys
import tempfile
import time
import wave
import xml.etree.ElementTree as ET
from pathlib import Path

CORE = 'io.github.guilhem.DeviceCore1'
HARDWARE = 'io.github.guilhem.NabHardware1'
ROOT = '/io/github/guilhem/DeviceCore1'
AGENT = ROOT + '/Agent'
PEER = '''import ctypes as c, os, sys
lib, bus, name = c.CDLL(os.environ['NABOS_TEST_LIBSYSTEMD']), c.c_void_p(), c.c_char_p()
def checked(result):
    if result < 0: raise OSError(-result, os.strerror(-result))
checked(lib.sd_bus_new(c.byref(bus)))
checked(lib.sd_bus_set_address(bus, sys.argv[1].encode()))
checked(lib.sd_bus_set_bus_client(bus, 1))
checked(lib.sd_bus_start(bus))
if sys.argv[2] != '-':
    checked(lib.sd_bus_request_name(bus, sys.argv[2].encode(), c.c_uint64(0)))
@c.CFUNCTYPE(c.c_int, c.c_void_p, c.c_void_p, c.c_void_p)
def reply(message, userdata, error):
    return lib.sd_bus_reply_method_errorf(c.c_void_p(message),
        b'org.freedesktop.DBus.Error.UnknownMethod', b'account-test-peer')
checked(lib.sd_bus_add_fallback(bus, None, b'/', reply, None))
checked(lib.sd_bus_get_unique_name(bus, c.byref(name)))
print(name.value.decode(), flush=True)
while True:
    result = lib.sd_bus_process(bus, None)
    checked(result)
    if result == 0: checked(lib.sd_bus_wait(bus, c.c_uint64(100000)))
'''


def require(ok, message):
    if not ok:
        raise RuntimeError(message)


def unit_values(directory, name, key):
    """Read Service assignments, including NixOS resets and ordered drop-ins."""
    unit = Path(directory) / (name + '.service')
    values = []
    for file in [unit, *sorted(Path(str(unit) + '.d').glob('*.conf'))]:
        section = ''
        for line in file.read_text().replace('\\\n', ' ').splitlines():
            line = line.strip()
            if line.startswith('['):
                section = line
            if section == '[Service]' and line.startswith(key + '='):
                value = line.partition('=')[2]
                if value:
                    values.append(value)
                else:
                    values.clear()
    return values


def unit_environment(directory, name):
    env = {}
    for value in unit_values(directory, name, 'Environment'):
        env.update(word.split('=', 1) for word in shlex.split(value))
    for value in unit_values(directory, name, 'EnvironmentFile'):
        for filename in shlex.split(value):
            file = Path(filename.removeprefix('-'))
            if filename.startswith('-') and not file.exists():
                continue
            for line in file.read_text().splitlines():
                if line.strip() and not line.lstrip().startswith('#'):
                    key, value = line.split('=', 1)
                    env[key] = ''.join(shlex.split(value))
    return env


def unit_command(directory, name):
    values = unit_values(directory, name, 'ExecStart')
    require(len(values) == 1, 'Expected one ExecStart: ' + name)
    command = shlex.split(values[0])
    require(command[0].startswith('/nix/store/') and Path(command[0]).is_file(),
            'Missing shipped Nix executable: ' + str(command))
    shipped_file(command[0])
    return command


def shipped_file(filename):
    binary = Path(filename).resolve(strict=True)
    image = Path('/run/nabos-image-store') / binary.relative_to('/nix/store')
    require(image.is_file() and filecmp.cmp(binary, image, shallow=False),
            'Product file absent from original image store: ' + str(binary))
    return str(binary)


def bus_policy(name, config=Path('/etc/dbus-1/system.conf')):
    """Locate policies via the shipped bus config, not Debian package paths."""
    def find(config, seen):
        config = config.resolve()
        if config in seen:
            return []
        seen.add(config)
        found = []
        for entry in ET.parse(config).getroot():
            if entry.tag not in ('include', 'includedir') or not entry.text:
                continue
            path = Path(entry.text)
            if not path.is_absolute():
                path = config.parent / path
            if entry.tag == 'includedir':
                if (path / name).is_file():
                    found.append(path / name)
            elif path.is_file():
                if path.name == name:
                    found.append(path)
                found.extend(find(path, seen))
        return found
    paths = find(config, set())
    require(paths, 'Policy not included by shipped D-Bus config: ' + name)
    return str(paths[0])


def main():
    require(os.geteuid() == 0, 'Run INSIDE the disposable image as root')
    for name, uid in [('nabos', 1000), ('nab-app', 1001), ('nab-hardware', 1002),
                      ('device-core', 1003), ('nab-audio', 1004)]:
        p = pwd.getpwnam(name)
        require((p.pw_uid, p.pw_gid) == (uid, uid) and grp.getgrgid(uid).gr_name == name, name)
    require((grp.getgrnam('nab-media').gr_gid, grp.getgrnam('gpio').gr_gid) == (1005, 1006),
            'Shared media/GPIO GIDs changed')
    for directory in ('/', '/etc', '/nix/store'):
        require(os.statvfs(directory).f_flag & os.ST_RDONLY, directory + ' must actually be read-only')
    for path in ('/.nabos-readonly-probe', '/etc/.nabos-readonly-probe'):
        try:
            Path(path).write_text('must fail')
        except OSError as error:
            require(error.errno == errno.EROFS, str(error))
        else:
            raise RuntimeError('Write succeeded on immutable path: ' + path)
    for directory in ('/data', '/run'):
        require(not os.statvfs(directory).f_flag & os.ST_RDONLY, directory + ' must be writable')
    mounts = Path('/proc/self/mountinfo').read_text().splitlines()
    require(any(l.split()[4] == '/dev' and l.split(' - ')[1].startswith('tmpfs ') for l in mounts),
            '/dev must be a private tmpfs, never a host /dev bind')
    require(not any(Path('/dev/' + p).exists() for p in ('snd', 'mem', 'vcio', 'gpiochip0')),
            'Host/hardware devices exposed')
    base = {key: os.environ[key] for key in
            ('PATH', 'NABOS_PERSIST_LIB', 'NABOS_TEST_LIBSYSTEMD', 'QEMU_CPU')}
    base['LC_ALL'] = 'C'
    system_units, user_units = '/etc/systemd/system', '/etc/systemd/user'
    system = Path(os.environ['NABOS_SYSTEM'])
    python = sys.executable

    def product(name):
        binary = (system / 'sw/bin' / name).resolve(strict=True)
        require(str(binary).startswith('/nix/store/'), 'Expected a shipped store binary: ' + name)
        return shipped_file(binary)

    children = []
    with tempfile.TemporaryDirectory(prefix='account-checks-', dir='/run') as tmp:
        work = Path(tmp)
        work.chmod(0o755)

        def prefix(uid, groups=()):
            return ['/run/nabos-test-tools/setpriv', '--reuid=' + str(uid), '--regid=' + str(uid),
                    '--groups=' + ','.join(map(str, groups)) if groups else '--clear-groups',
                    '--no-new-privs'] + ([] if uid == 0 else ['--bounding-set=-all'])

        def run(uid, args, env=base, groups=(), cwd=work, **kw):
            return sp.run(prefix(uid, groups) + args, env=env, cwd=cwd,
                          capture_output=True, text=True, timeout=15, **kw)

        def start(uid, args, label, env=base, groups=(), pipe=False):
            with (work / (label + '.log')).open('w') as log:
                p = sp.Popen(prefix(uid, groups) + args, env=env, cwd=work,
                             stdout=sp.PIPE if pipe else log, stderr=log, text=True)
            children.append(p)
            return p

        def line(p):
            require(select.select([p.stdout], [], [], 10)[0], 'Child startup timed out')
            value = p.stdout.readline().strip()
            require(value and p.poll() is None, 'Child startup failed')
            return value

        def expect(result, output=None, error=None):
            require((result.returncode == 0 and (output is None or result.stdout.strip() == output))
                    if error is None else result.returncode != 0 and error in result.stderr,
                    f'{result.args}: rc={result.returncode}\n{result.stdout}{result.stderr}')

        def wait_for(check):
            deadline = time.monotonic() + 60
            while not check():
                require(all(p.poll() is None for p in children) and time.monotonic() < deadline,
                        'Service exited or readiness timed out')
                time.sleep(0.1)

        def stop(p):
            if p.poll() is None:
                p.terminate()
                try:
                    p.wait(timeout=5)
                except sp.TimeoutExpired:
                    p.kill()
                    p.wait(timeout=5)
            children.remove(p)

        def interrupted(signum, frame):
            raise RuntimeError('Interrupted: ' + str(signum))

        for sig in (signal.SIGTERM, signal.SIGINT):
            signal.signal(sig, interrupted)
        try:
            # A regular sentinel in private /dev exercises DAC with real image
            # accounts; it never opens an I2C controller or validates hardware.
            i2c = Path('/dev/i2c-1')
            i2c.touch()
            os.chown(i2c, 0, grp.getgrnam('nab-hardware').gr_gid)
            i2c.chmod(0o660)
            for uid in (1000, 1001, 1002, 1003, 1004):
                result = run(uid, [python, '-B', '-c',
                    "import os; os.close(os.open('/dev/i2c-1', os.O_RDWR))"])
                expect(result, error=None if uid == 1002 else 'PermissionError')
            i2c.unlink()
            print('PASS: I2C bus file permissions allow only the hardware account', flush=True)
            # Exercise first-use media permissions, including uploaded 0600 bytes.
            sound = Path('/data/nabos/media/sounds/user/existing.wav')
            sound.parent.mkdir(parents=True, exist_ok=True)
            settings = Path('/data/nabos/application.json')
            for file, contents in ((sound, 'old sound'), (settings, '{}')):
                file.write_text(contents)
                file.chmod(0o600)
                os.chown(file, 1001, 1001)
            expect(run(0, ['sh', '-ec', '. "$NABOS_PERSIST_LIB"; media_permissions']))
            expect(run(0, ['systemd-tmpfiles', '--create', '--prefix=/data/nabos']))
            expect(run(1003, ['cat', str(sound)], groups=(1005,)), output='old sound')
            expect(run(1001, ['sh', '-c', 'umask 027; printf new > /data/nabos/media/sounds/user/new.wav']))
            expect(run(1003, ['cat', str(sound.parent / 'new.wav')], groups=(1005,)), output='new')
            expect(run(1001, ['cat', str(settings)]), output='{}')
            expect(run(1003, ['cat', str(settings)], groups=(1005,)), error='Permission denied')
            print('PASS: persistent media and private settings enforce service ownership', flush=True)
            network = run(0, [product('NetworkManager'), '--print-config'],
                          base | unit_environment(system_units, 'NetworkManager'))
            expect(network)
            parsed = configparser.ConfigParser(interpolation=None)
            parsed.read_string(network.stdout)
            require(parsed['main']['rc-manager'] == 'unmanaged', 'NetworkManager resolver writes enabled')
            require(os.readlink('/etc/resolv.conf') == '/run/NetworkManager/resolv.conf', 'Resolver target changed')
            dnsmasq = list(Path('/run/nabos-image-store').glob('*-dnsmasq-*/bin/dnsmasq'))
            require(len(dnsmasq) == 1, 'Expected one shipped hotspot DNS helper')
            dnsmasq = '/nix/store/' + str(dnsmasq[0].relative_to('/run/nabos-image-store'))
            expect(run(0, [shipped_file(dnsmasq), '--test',
                           '--conf-file=/etc/NetworkManager/dnsmasq-shared.d/nabos.conf']))
            ssh = run(0, [product('sshd'), '-G', '-f', '/etc/ssh/sshd_config'])
            expect(ssh)
            ssh_lines = [key.lower() + ' ' + value for key, value in
                         (line.split(None, 1) for line in ssh.stdout.splitlines())]
            for setting in ('permitrootlogin no', 'passwordauthentication no',
                            'kbdinteractiveauthentication no', 'authenticationmethods publickey',
                            'authorizedkeyscommanduser device-core', 'allowusers nabos'):
                require(setting in ssh_lines, 'SSH setting missing: ' + setting + '\n' + ssh.stdout)
            expect(run(0, [product('visudo'), '--check']))
            print('PASS: shipped NetworkManager/DNS/SSH/sudo configuration parses on RO root', flush=True)
            config = '''<busconfig><type>system</type><auth>EXTERNAL</auth>
<listen>unix:path={}/bus</listen><policy context="default">
<allow user="*"/><deny own="*"/><deny send_type="method_call"/>
<allow send_type="signal"/><allow send_requested_reply="true" send_type="method_return"/><allow send_requested_reply="true" send_type="error"/>
<allow receive_type="method_call"/><allow receive_type="method_return"/><allow receive_type="error"/><allow receive_type="signal"/>
<allow send_destination="org.freedesktop.DBus" send_interface="org.freedesktop.DBus"/>
</policy><include>{}</include><include>{}</include></busconfig>'''.format(work,
                bus_policy(CORE + '.conf'), bus_policy(HARDWARE + '.conf'))
            conf = work / 'bus.conf'
            conf.write_text(config)
            address = line(start(0, ['dbus-daemon', '--nofork', '--nopidfile', '--print-address=1',
                                      '--config-file=' + str(conf)], 'bus', pipe=True))

            def call(uid, dest, path, iface, member, *args):
                return run(uid, ['busctl', '--address=' + address, '--auto-start=no', '--timeout=5',
                                 'call', dest, path, iface, member, *args])

            uids = (0, 1000, 1001, 1002, 1003, 1004)
            for service, owner in ((CORE, 1003), (HARDWARE, 1002)):
                for uid in uids:
                    expect(call(uid, 'org.freedesktop.DBus', '/org/freedesktop/DBus',
                                'org.freedesktop.DBus', 'RequestName', 'su', service, '4'),
                           output='u 1', error=None if uid == owner else 'Access denied')
            fake_core = start(1003, [python, '-B', '-c', PEER, address, CORE], 'core-peer', pipe=True)
            line(fake_core)
            line(start(1002, [python, '-B', '-c', PEER, address, HARDWARE], 'hardware-peer', pipe=True))
            app = line(start(1001, [python, '-B', '-c', PEER, address, '-'], 'app-peer', pipe=True))
            for service, allowed in ((CORE, (0, 1001, 1002, 1003)), (HARDWARE, (0, 1001))):
                for uid in uids:
                    expect(call(uid, service, '/', CORE + '.Probe', 'Probe'),
                           error='account-test-peer' if uid in allowed else 'Access denied')
            for method in ('Acquire', 'Abort', 'Release'):
                for uid in uids:
                    expect(call(uid, app, AGENT, CORE + '.Agent', method, 's', 'test-token'),
                           error='account-test-peer' if uid == 1003 else 'Access denied')
                for path, iface, member in ((AGENT + '/Other', CORE + '.Agent', method),
                                            (AGENT, CORE + '.Other', method), (AGENT, CORE + '.Agent', 'Other')):
                    expect(call(1003, app, path, iface, member, 's', 'test-token'), error='Access denied')
            stop(fake_core)
            expect(run(0, ['systemd-tmpfiles', '--create', '--prefix=/data/device-core',
                           '--prefix=/run/lock/device-core', '--prefix=/run/nabos-audio']))
            runtime = work / 'core'
            runtime.mkdir(mode=0o700)
            os.chown(runtime, 1003, 1003)
            coreenv = base | unit_environment(system_units, 'device-core')
            coreenv.update(DEVICE_CORE_BUS_ADDRESS=address, DBUS_SYSTEM_BUS_ADDRESS=address,
                           XDG_RUNTIME_DIR=str(runtime), TMPDIR=str(runtime), DEVICE_CORE_UPDATE_REPO='', DEVICE_CORE_UPDATE_ASSET='')
            core_command = unit_command(system_units, 'device-core') + ['--simulate']
            core = start(1003, core_command, 'device-core', coreenv, (1005, 1004))
            wait_for(lambda: call(1001, CORE, ROOT, 'org.freedesktop.DBus.Properties', 'Get',
                                 'ss', CORE + '.Manager', 'Ready').stdout.strip() == 'v b true')
            for uid in (0, 1001, 1002, 1003, 1004):
                expect(call(uid, CORE, ROOT + '/Network', CORE + '.Network', 'ReportPresence',
                            't', str(time.monotonic_ns())), error=None if uid == 1002 else
                       'Access denied')
            # Only this fixture grant bypasses policy, to test the daemon's audio-UID rejection too.
            conf.write_text(config.replace('</busconfig>', '<policy user="nab-audio"><allow send_destination="' + CORE +
                '" send_path="' + ROOT + '/Network" send_interface="' + CORE + '.Network" send_member="ReportPresence"/></policy></busconfig>'))
            expect(call(0, 'org.freedesktop.DBus', '/org/freedesktop/DBus', 'org.freedesktop.DBus', 'ReloadConfig'))
            expect(call(1004, CORE, ROOT + '/Network', CORE + '.Network', 'ReportPresence',
                        't', str(time.monotonic_ns())), error='Access denied')
            print('PASS: real UID ownership, service sends, exact callbacks and ReportPresence credentials', flush=True)
            lock = Path('/run/lock/device-core/network')
            require((lock.stat().st_uid, lock.stat().st_mode & 0o777) == (1003, 0o600),
                    'Network lock ownership/mode')
            require((lock.parent.stat().st_uid, lock.parent.stat().st_mode & 0o777) == (0, 0o755),
                    'Network lock directory must remain root-owned')
            inode = lock.stat().st_ino
            with lock.open('r+') as guard:
                fcntl.flock(guard, fcntl.LOCK_SH)
                for boot in range(2):
                    config_call = ['busctl', '--address=' + address, '--auto-start=no', '--timeout=5',
                                   '--json=short', 'call', CORE, ROOT + '/Config', CORE + '.Config']
                    read = run(1001, config_call + ['Read'])
                    expect(read)
                    revision = json.loads(read.stdout)['data'][0]
                    if boot == 0:
                        expect(run(1001, config_call + ['Update', 's(ssub(bs(uu)(uu))b)', revision,
                            'fr_FR', 'Europe/Paris', '35', 'true', 'false', 'stable', '3', '0', '5', '0', 'true']))
                        require(Path('/data/device-core/voice-enabled').is_file(), 'Voice flag not persisted')
                        first_revision = revision
                    else:
                        require(revision != first_revision, 'Restart must invalidate configuration revisions')
                        settings = json.loads(Path('/data/device-core/settings.json').read_text())
                        require(settings['settings']['volume'] == 35 and settings['settings']['voice_enabled'],
                                'Device settings lost across restart')
                    require(lock.stat().st_ino == inode, 'Restart replaced the network lock')
                    stop(core)
                    if boot == 0:
                        core = start(1003, core_command, 'device-core-restart', coreenv, (1005, 1004))
                        wait_for(lambda: call(1001, CORE, ROOT, 'org.freedesktop.DBus.Properties', 'Get',
                            'ss', CORE + '.Manager', 'Ready').stdout.strip() == 'v b true')
            for uid, own_home, other_home in ((1000, '/var/lib/nabos/admin', '/var/lib/nabos/lva'),
                                               (1004, '/var/lib/nabos/lva', '/var/lib/nabos/admin')):
                require(pwd.getpwuid(uid).pw_dir == own_home, f'Wrong home for UID {uid}')
                expect(run(uid, [python, '-B', '-c',
                    "from pathlib import Path; import sys; p=Path(sys.argv[1])/'.write-check'; p.touch(); p.unlink()",
                    own_home]))
                denied = run(uid, [python, '-B', '-c',
                    "from pathlib import Path; import sys; Path(sys.argv[1]).touch()",
                    other_home + '/.must-not-write'])
                require(denied.returncode != 0 and 'PermissionError' in denied.stderr,
                        f'UID {uid} must not write another account home')
            print('PASS: first-use settings, persistent voice home, restart revision and stable lock inode', flush=True)
            # Use the shipped package's service account; only its authority name is added.
            policy = 'org.freedesktop.PolicyKit1'
            conf.write_text(config.replace('</busconfig>',
                '<include>' + bus_policy('org.freedesktop.PolicyKit1.conf') + '</include></busconfig>'))
            expect(call(0, 'org.freedesktop.DBus', '/org/freedesktop/DBus', 'org.freedesktop.DBus', 'ReloadConfig'))
            polkit = pwd.getpwnam(unit_values(system_units, 'polkit', 'User')[-1])
            env = base | {'DBUS_SYSTEM_BUS_ADDRESS': address, 'HOME': polkit.pw_dir, 'TMPDIR': str(work)}
            # The package's primary GID need not equal its UID.
            with (work / 'polkit.log').open('w') as log:
                children.append(sp.Popen(['/run/nabos-test-tools/setpriv', '--reuid=' + str(polkit.pw_uid),
                    '--regid=' + str(polkit.pw_gid), '--clear-groups', '--no-new-privs', '--bounding-set=-all',
                    *unit_command(system_units, 'polkit'), '--no-debug'],
                    env=env | unit_environment(system_units, 'polkit'), cwd=work, stdout=log, stderr=log))
            wait_for(lambda: call(0, 'org.freedesktop.DBus', '/org/freedesktop/DBus', 'org.freedesktop.DBus',
                                 'NameHasOwner', 's', policy).stdout.strip() == 'b true')
            for uid in (1000, 1001, 1002, 1003, 1004):
                subject = start(uid, ['sleep', '120'], 'subject-' + str(uid))
                wait_for(lambda: f'Uid:\t{uid}\t{uid}\t{uid}\t{uid}' in
                         Path('/proc/' + str(subject.pid) + '/status').read_text())
                starttime = Path('/proc/' + str(subject.pid) + '/stat').read_text().rsplit(')', 1)[1].split()[19]
                checked = run(0, [product('pkcheck'), '--action-id', 'org.freedesktop.NetworkManager.network-control',
                                  '--process', f'{subject.pid},{starttime},{uid}'], env)
                # The operator may receive a distro authentication challenge;
                # service accounts must be explicitly denied without a prompt.
                expected = (0,) if uid == 1003 else (1, 2) if uid == 1000 else (1,)
                require(checked.returncode in expected,
                        f'Polkit UnixProcess uid={uid}: rc={checked.returncode} {checked.stdout}{checked.stderr}')
                stop(subject)
            print('PASS: shipped Polkit network-control rules with real UnixProcess service UIDs', flush=True)
            # Both audio clients use the package's PipeWire/ALSA configuration.
            # A null sink keeps this check independent of the board's sound card.
            expect(run(0, ['systemd-tmpfiles', '--create', '--prefix=/var/lib/nabos',
                           '--prefix=/run/nabos-audio']))
            audio_runtime = work / 'audio'
            audio_runtime.mkdir(mode=0o700)
            os.chown(audio_runtime, 1004, 1004)
            config_dir = work / 'config/pipewire/pipewire.conf.d'
            config_dir.mkdir(parents=True)
            (config_dir / '99-test-null.conf').write_text('''context.objects = [
{ factory = adapter args = { factory.name = support.null-audio-sink
node.name = test-sink media.class = Audio/Sink node.driver = true audio.position = [ FL FR ] } }
]''')
            audioenv = base | {'HOME': '/var/lib/nabos/lva', 'XDG_RUNTIME_DIR': str(audio_runtime),
                               'XDG_CONFIG_HOME': str(work / 'config'),
                               'XDG_STATE_HOME': '/var/lib/nabos/lva/state',
                               'XDG_CACHE_HOME': '/var/lib/nabos/lva/cache',
                               'DBUS_SYSTEM_BUS_ADDRESS': address}
            audioenv['DBUS_SESSION_BUS_ADDRESS'] = line(start(1004,
                ['dbus-daemon', '--session', '--nofork', '--print-address=1',
                 '--address=unix:path=' + str(audio_runtime / 'bus')], 'audio-bus', audioenv, pipe=True))
            # Unlike the systemd session, this fixture has no socket activation.
            start(1004, unit_command(user_units, 'pipewire'), 'pipewire',
                  audioenv | unit_environment(user_units, 'pipewire'))
            wait_for(lambda: (audio_runtime / 'pipewire-0').is_socket())
            for program in ('wireplumber', 'pipewire-pulse'):
                start(1004, unit_command(user_units, program), program,
                      audioenv | unit_environment(user_units, program))
            clientenv = base | {'HOME': '/data/device-core', 'XDG_RUNTIME_DIR': str(runtime),
                                'PIPEWIRE_REMOTE': '/run/nabos-audio/pipewire-0'}
            wait_for(lambda: 'Volume:' in run(1003, [product('wpctl'), 'get-volume', '@DEFAULT_AUDIO_SINK@'],
                                             clientenv, (1004,)).stdout)
            sound = work / 'silence.wav'
            with wave.open(str(sound), 'wb') as wav:
                wav.setparams((2, 2, 48000, 0, 'NONE', 'not compressed'))
                wav.writeframes(bytes(4800 * 4))
            expect(run(1003, [product('aplay'), '-q', '-D', 'default', str(sound)], clientenv, (1004,)))
            # The null sink applies volume only after its first audio format
            # negotiation. Exercise playback before checking its software mixer.
            expect(run(1003, [product('wpctl'), 'set-volume', '@DEFAULT_AUDIO_SINK@', '0.42'], clientenv, (1004,)))
            expect(run(1003, [product('wpctl'), 'get-volume', '@DEFAULT_AUDIO_SINK@'], clientenv, (1004,)),
                   output='Volume: 0.42')
            wait_for(lambda: 'test-sink' in run(1004, [product('pactl'), 'list', 'short', 'sinks'], audioenv).stdout)
            socket = Path('/run/nabos-audio/pipewire-0').stat()
            require((socket.st_uid, socket.st_gid, socket.st_mode & 0o777) == (1004, 1004, 0o660),
                    'Wrong shared audio socket permissions')
            for uid in (1000, 1001, 1002):
                denied = run(uid, [python, '-B', '-c',
                    "import socket; socket.socket(socket.AF_UNIX).connect('/run/nabos-audio/pipewire-0')"])
                require(denied.returncode != 0 and 'PermissionError' in denied.stderr,
                        f'Audio socket was accessible to uid {uid}')
            print('PASS: shared audio socket, device-core volume/playback, audio session and denied peers', flush=True)
            voice_unit = Path(system_units) / 'linux-voice-assistant.service'
            release = unit_environment(system_units, 'device-core')
            require(release.get('DEVICE_CORE_LVA_UNIT', '') ==
                    ('linux-voice-assistant.service' if voice_unit.exists() else ''),
                    'Voice availability must match shipped release configuration')
            if voice_unit.exists():
                require(os.environ['NABOS_RUNTIME_TARGET'] == 'zero2-arm64', 'LVA is ARM64-only')
                directory = Path(unit_values(system_units, 'linux-voice-assistant', 'WorkingDirectory')[-1])
                for source in (directory / 'linux_voice_assistant').rglob('*.py'):
                    compile(source.read_bytes(), str(source), 'exec')
                require((directory / 'sounds').is_dir() and (directory / 'wakewords').is_dir() and
                        (directory / 'version.txt').read_text().strip() != 'unknown', 'LVA source/assets incomplete')
                print('PASS (source inspection): LVA source syntax and packaged assets', flush=True)
                voiceenv = audioenv | unit_environment(system_units, 'linux-voice-assistant')
                voiceenv.update(XDG_RUNTIME_DIR=str(audio_runtime),
                                PULSE_SERVER='unix:' + str(audio_runtime / 'pulse/native'))
                expect(run(1004, unit_command(system_units, 'linux-voice-assistant') + ['--help'],
                           voiceenv, cwd=directory))
                print('PASS: shipped LVA CLI/dependency smoke on private PulseAudio; '
                      'satellite/model first-use still unqualified', flush=True)
        except BaseException:
            for log in work.glob('*.log'):
                print(str(log) + ':\n' + log.read_text(errors='replace'), flush=True)
            raise
        finally:
            for sig in (signal.SIGTERM, signal.SIGINT):
                signal.signal(sig, signal.SIG_IGN)
            for p in children[:][::-1]:
                stop(p)


def self_test():
    """Runnable fixture for Nix unit resets/quoting and policy discovery."""
    with tempfile.TemporaryDirectory(prefix='nabos-runtime-fixture-') as tmp:
        directory = Path(tmp)
        original, overlay = directory / 'original', directory / 'overlay'
        original.write_bytes(b'shipped'); overlay.write_bytes(b'shipped')
        assert not os.path.samefile(original, overlay) and filecmp.cmp(original, overlay, shallow=False)
        overlay.write_bytes(b'altered')
        assert not filecmp.cmp(original, overlay, shallow=False)
        envfile = directory / 'release.env'
        envfile.write_text('DEVICE_CORE_LVA_UNIT="linux-voice-assistant.service"\nEMPTY=\n')
        unit = directory / 'device-core.service'
        unit.write_text('[Unit]\nEnvironment=IGNORED=true\n[Service]\n'
                        'ExecStart=/old/device-core\nEnvironment="LABEL=with spaces" OLD=value\n')
        dropins = Path(str(unit) + '.d')
        dropins.mkdir()
        (dropins / '10-reset.conf').write_text('[Service]\nExecStart=\nExecStart=/nix/store/core/bin/device-core\n'
                                             'Environment=\nEnvironment="HOME=/data/device-core"\n')
        (dropins / '20-env.conf').write_text('[Service]\nEnvironment="LABEL=with spaces" NEXT=one '
                                            + '\\' + '\nLAST=two\nEnvironmentFile=' + str(envfile) + '\n')
        assert unit_values(directory, 'device-core', 'ExecStart') == ['/nix/store/core/bin/device-core']
        assert unit_environment(directory, 'device-core') == {
            'HOME': '/data/device-core', 'LABEL': 'with spaces', 'NEXT': 'one', 'LAST': 'two',
            'DEVICE_CORE_LVA_UNIT': 'linux-voice-assistant.service', 'EMPTY': ''}
        policy = directory / 'system.d'
        policy.mkdir()
        (policy / 'test.conf').write_text('<busconfig/>')
        included = directory / 'included.conf'
        included.write_text('<busconfig><includedir>system.d</includedir></busconfig>')
        config = directory / 'system.conf'
        config.write_text('<busconfig><include>included.conf</include></busconfig>')
        assert bus_policy('test.conf', config) == str(policy / 'test.conf')
        try:
            bus_policy('missing.conf', config)
        except RuntimeError:
            pass
        else:
            raise AssertionError('Missing policies must fail')
    print('PASS: Nix unit/drop-in/environment and shipped-policy lookup fixtures')


if __name__ == '__main__':
    if sys.argv[1:] == ['--self-test']:
        self_test()
    else:
        main()
