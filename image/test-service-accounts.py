#!/usr/bin/python3
"""Run as root in a disposable image: read-only /, writable /data and /run,
private tmpfs /dev without sound devices, and shipped binaries/accounts/rules.
The caller supplies isolated mount/network namespaces; no host bus is used.
"""
import os
import pwd
import grp
import select
import signal
import subprocess as sp
import tempfile
import time
import wave
from pathlib import Path

CORE = 'io.github.guilhem.DeviceCore1'
HARDWARE = 'io.github.guilhem.NabHardware1'
ROOT = '/io/github/guilhem/DeviceCore1'
AGENT = ROOT + '/Agent'
PEER = '''import ctypes as c, os, sys
lib, bus, name = c.CDLL('libsystemd.so.0'), c.c_void_p(), c.c_char_p()
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


def main():
    require(os.geteuid() == 0, 'Run INSIDE the disposable image as root')
    for name, uid in [('nabos', 1000), ('nab-app', 1001), ('nab-hardware', 1002),
                      ('device-core', 1003), ('nab-audio', 1004)]:
        p = pwd.getpwnam(name)
        require((p.pw_uid, p.pw_gid) == (uid, uid) and grp.getgrgid(uid).gr_name == name, name)
    require(os.statvfs('/').f_flag & os.ST_RDONLY, 'Root must actually be read-only')
    for directory in ('/data', '/run'):
        require(not os.statvfs(directory).f_flag & os.ST_RDONLY, directory + ' must be writable')
    mounts = Path('/proc/self/mountinfo').read_text().splitlines()
    require(any(l.split()[4] == '/dev' and l.split(' - ')[1].startswith('tmpfs ') for l in mounts),
            '/dev must be a private tmpfs, never a host /dev bind')
    require(not any(Path('/dev/' + p).exists() for p in ('snd', 'mem', 'vcio', 'gpiochip0')),
            'Host/hardware devices exposed')
    base = {'PATH': '/usr/sbin:/usr/bin:/sbin:/bin', 'LC_ALL': 'C', 'QEMU_CPU': 'arm1176'}
    children = []
    with tempfile.TemporaryDirectory(prefix='account-checks-', dir='/run') as tmp:
        work = Path(tmp)
        work.chmod(0o755)

        def prefix(uid, groups=()):
            return ['setpriv', '--reuid=' + str(uid), '--regid=' + str(uid),
                    '--groups=' + ','.join(map(str, groups)) if groups else '--clear-groups',
                    '--no-new-privs'] + ([] if uid == 0 else ['--bounding-set=-all'])

        def run(uid, args, env=base, groups=(), **kw):
            return sp.run(prefix(uid, groups) + args, env=env, cwd=work,
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
            # Exercise ownership migration on existing bytes, including sounds
            # uploaded as 0600 by the former shared account.
            sound = Path('/data/nabos/media/sounds/user/existing.wav')
            sound.parent.mkdir(parents=True, exist_ok=True)
            settings = Path('/data/nabos/application.json')
            for file, contents in ((sound, 'old sound'), (settings, '{}')):
                file.write_text(contents)
                file.chmod(0o600)
                os.chown(file, 1000, 1000)
            expect(run(0, ['sh', '-c', 'NABOS_BOOT_INIT_LIB=1 . /usr/lib/nabos/boot-init; media_permissions']))
            expect(run(0, ['systemd-tmpfiles', '--create', '--prefix=/data/nabos']))
            expect(run(1003, ['cat', str(sound)], groups=(1005,)), output='old sound')
            expect(run(1001, ['sh', '-c', 'umask 027; printf new > /data/nabos/media/sounds/user/new.wav']))
            expect(run(1003, ['cat', str(sound.parent / 'new.wav')], groups=(1005,)), output='new')
            expect(run(1001, ['cat', str(settings)]), output='{}')
            expect(run(1003, ['cat', str(settings)], groups=(1005,)), error='Permission denied')
            print('PASS: existing media and private settings survive ownership migration', flush=True)
            config = '''<busconfig><type>system</type><auth>EXTERNAL</auth>
<listen>unix:path={}/bus</listen><policy context="default">
<allow user="*"/><deny own="*"/><deny send_type="method_call"/>
<allow send_type="signal"/><allow send_requested_reply="true" send_type="method_return"/><allow send_requested_reply="true" send_type="error"/>
<allow receive_type="method_call"/><allow receive_type="method_return"/><allow receive_type="error"/><allow receive_type="signal"/>
<allow send_destination="org.freedesktop.DBus" send_interface="org.freedesktop.DBus"/>
</policy><include>/etc/dbus-1/system.d/io.github.guilhem.DeviceCore1.conf</include>
<include>/etc/dbus-1/system.d/io.github.guilhem.NabHardware1.conf</include></busconfig>'''.format(work)
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
            fake_core = start(1003, ['python3', '-B', '-c', PEER, address, CORE], 'core-peer', pipe=True)
            line(fake_core)
            line(start(1002, ['python3', '-B', '-c', PEER, address, HARDWARE], 'hardware-peer', pipe=True))
            app = line(start(1001, ['python3', '-B', '-c', PEER, address, '-'], 'app-peer', pipe=True))
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
            coreenv = base | dict(line[12:].split('=', 1) for line in
                Path('/usr/lib/systemd/system/device-core.service').read_text().splitlines() if line.startswith('Environment='))
            coreenv.update(DEVICE_CORE_BUS_ADDRESS=address, DBUS_SYSTEM_BUS_ADDRESS=address,
                           XDG_RUNTIME_DIR=str(runtime), TMPDIR=str(runtime), DEVICE_CORE_UPDATE_REPO='', DEVICE_CORE_UPDATE_ASSET='')
            start(1003, ['/usr/bin/device-core', '--simulate'], 'device-core', coreenv, (1004,))
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
            # Use the shipped package's service account; only its authority name is added.
            policy = 'org.freedesktop.PolicyKit1'
            conf.write_text(config.replace('</busconfig>',
                '<include>/usr/share/dbus-1/system.d/org.freedesktop.PolicyKit1.conf</include></busconfig>'))
            expect(call(0, 'org.freedesktop.DBus', '/org/freedesktop/DBus', 'org.freedesktop.DBus', 'ReloadConfig'))
            polkit = pwd.getpwnam('polkitd')
            env = base | {'DBUS_SYSTEM_BUS_ADDRESS': address, 'HOME': polkit.pw_dir, 'TMPDIR': str(work)}
            # The package's primary GID need not equal its UID.
            with (work / 'polkit.log').open('w') as log:
                children.append(sp.Popen(['setpriv', '--reuid=' + str(polkit.pw_uid),
                    '--regid=' + str(polkit.pw_gid), '--clear-groups', '--no-new-privs', '--bounding-set=-all',
                    '/usr/lib/polkit-1/polkitd', '--no-debug'], env=env, cwd=work, stdout=log, stderr=log))
            wait_for(lambda: call(0, 'org.freedesktop.DBus', '/org/freedesktop/DBus', 'org.freedesktop.DBus',
                                 'NameHasOwner', 's', policy).stdout.strip() == 'b true')
            for uid in (1000, 1001, 1002, 1003, 1004):
                subject = start(uid, ['sleep', '120'], 'subject-' + str(uid))
                wait_for(lambda: f'Uid:\t{uid}\t{uid}\t{uid}\t{uid}' in
                         Path('/proc/' + str(subject.pid) + '/status').read_text())
                starttime = Path('/proc/' + str(subject.pid) + '/stat').read_text().rsplit(')', 1)[1].split()[19]
                checked = run(0, ['pkcheck', '--action-id', 'org.freedesktop.NetworkManager.network-control',
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
                               'XDG_CONFIG_HOME': str(work / 'config')}
            audioenv['DBUS_SESSION_BUS_ADDRESS'] = line(start(1004,
                ['dbus-daemon', '--session', '--nofork', '--print-address=1',
                 '--address=unix:path=' + str(audio_runtime / 'bus')], 'audio-bus', audioenv, pipe=True))
            for program in ('pipewire', 'wireplumber', 'pipewire-pulse'):
                start(1004, [program], program, audioenv)
            clientenv = base | {'HOME': '/data/device-core', 'XDG_RUNTIME_DIR': str(runtime),
                                'PIPEWIRE_REMOTE': '/run/nabos-audio/pipewire-0'}
            wait_for(lambda: 'Volume:' in run(1003, ['wpctl', 'get-volume', '@DEFAULT_AUDIO_SINK@'],
                                             clientenv, (1004,)).stdout)
            expect(run(1003, ['wpctl', 'set-volume', '@DEFAULT_AUDIO_SINK@', '0.42'], clientenv, (1004,)))
            require('0.42' in run(1003, ['wpctl', 'get-volume', '@DEFAULT_AUDIO_SINK@'],
                                 clientenv, (1004,)).stdout, 'Volume did not change')
            sound = work / 'silence.wav'
            with wave.open(str(sound), 'wb') as wav:
                wav.setparams((2, 2, 48000, 0, 'NONE', 'not compressed'))
                wav.writeframes(bytes(4800 * 4))
            expect(run(1003, ['aplay', '-q', '-D', 'default', str(sound)], clientenv, (1004,)))
            wait_for(lambda: 'test-sink' in run(1004, ['pactl', 'list', 'short', 'sinks'], audioenv).stdout)
            socket = Path('/run/nabos-audio/pipewire-0').stat()
            require((socket.st_uid, socket.st_gid, socket.st_mode & 0o777) == (1004, 1004, 0o660),
                    'Wrong shared audio socket permissions')
            for uid in (1000, 1001, 1002):
                denied = run(uid, ['python3', '-B', '-c',
                    "import socket; socket.socket(socket.AF_UNIX).connect('/run/nabos-audio/pipewire-0')"])
                require(denied.returncode != 0 and 'PermissionError' in denied.stderr,
                        f'Audio socket was accessible to uid {uid}')
            print('PASS: shared audio socket, device-core volume/playback, audio session and denied peers', flush=True)
        except BaseException:
            for log in work.glob('*.log'):
                print(str(log) + ':\n' + log.read_text(errors='replace'), flush=True)
            raise
        finally:
            for sig in (signal.SIGTERM, signal.SIGINT):
                signal.signal(sig, signal.SIG_IGN)
            for p in children[:][::-1]:
                stop(p)


if __name__ == '__main__':
    main()
