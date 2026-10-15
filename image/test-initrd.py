#!/usr/bin/env python3
"""Check the shipped initrd without mounting filesystems or executing ARM code."""
import configparser
import os
import re
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile


def check(archive, recovery=False):
    assert Path(archive).is_file(), 'Expected a regular initrd archive, never a block device'
    errors = []
    with tempfile.TemporaryDirectory(prefix='nabos-initrd-test-') as work:
        root = Path(work)
        with subprocess.Popen(['zstd', '-dc', archive], stdout=subprocess.PIPE) as decoder:
            subprocess.run(['cpio', '-idm', '--quiet', '--no-absolute-filenames',
                            '--no-preserve-owner'], stdin=decoder.stdout, cwd=root, check=True)
            decoder.stdout.close()
            assert decoder.wait() == 0, 'initrd decompression failed'

        # Rebase only the disposable copy so pathlib cannot resolve against the host store.
        for directory, dirs, files in os.walk(root, followlinks=False):
            Path(directory).chmod(0o700)
            for name in dirs + files:
                path = Path(directory) / name
                if path.is_symlink() and path.readlink().is_absolute():
                    target = root / str(path.readlink()).lstrip('/')
                    path.unlink()
                    path.symlink_to(os.path.relpath(target, path.parent))

        def included(path, executable=False):
            try:
                resolved = (root / path.lstrip('/')).resolve(strict=True)
                assert resolved.is_relative_to(root), f'path escapes initrd: {path}'
                assert not executable or (resolved.is_file() and os.access(resolved, os.X_OK))
                return resolved
            except (OSError, AssertionError) as error:
                errors.append(f'{path}: {error}')

        if recovery:
            init = included('/init', executable=True)
            assert init is not None, 'missing independent recovery init'
            script = init.read_text()
            included(script.splitlines()[0].removeprefix('#!'), executable=True)
            assert '. /lib/nabos-recovery.sh' in script
            included('/lib/nabos-recovery.sh')
            included('/lib/nabos-layout.sh')
            included('/bin/sh', executable=True)
            included('/bin/nabos-recovery-led', executable=True)
            config = included('/etc/fw_env.config')
            assert config is not None
            assert config.read_text().splitlines()[-2:] == [
                '/dev/mmcblk0 0x100000 0x10000', '/dev/mmcblk0 0x200000 0x10000']
            path = next(line.split('=', 1)[1] for line in script.splitlines()
                        if line.startswith('PATH='))
            for command in ('sleep', 'mkdir', 'cat', 'stat', 'sfdisk', 'awk', 'grep',
                            'mkfs.ext4', 'e2fsck', 'blkid', 'sync', 'mount', 'umount',
                            'fw_setenv', 'fw_printenv', 'timeout', 'modprobe', 'reboot'):
                candidates = [root / directory.lstrip('/') / command for directory in path.split(':')]
                assert any(p.is_file() and os.access(p, os.X_OK) and
                           p.resolve().is_relative_to(root) for p in candidates), command
            for module in ('mmc_block', 'bcm2835-ws2812'):
                assert any(module in p.name for p in (root / 'lib/modules').rglob('*')), module
            assert 'nabos-persist.service' not in str(list(root.rglob('*')))
            # Check every shipped ELF interpreter and shared library against RAM contents.
            for executable in root.rglob('*'):
                if not executable.is_file() or executable.is_symlink():
                    continue
                with executable.open('rb') as handle:
                    if handle.read(4) != b'\x7fELF':
                        continue
                metadata = subprocess.check_output(['readelf', '-l', '-d', str(executable)], text=True)
                interpreter = re.search(r'Requesting program interpreter: ([^\]]+)', metadata)
                if interpreter:
                    included(interpreter[1], executable=True)
                needed = re.findall(r'\(NEEDED\).*?\[([^\]]+)\]', metadata)
                rpaths = re.findall(r'\((?:RUNPATH|RPATH)\).*?\[([^\]]+)\]', metadata)
                directories = [executable.parent] + [root / part.lstrip('/')
                    for value in rpaths for part in value.split(':') if part.startswith('/')]
                for name in needed:
                    assert any((d / name).is_file() and (d / name).resolve().is_relative_to(root)
                               for d in directories), (executable, name)
            if errors:
                raise AssertionError('\n'.join(errors))
            print(f'PASS: {archive}: independent recovery scripts, tools, modules and ELF dependencies')
            return

        unit_path = included('/etc/systemd/system/nabos-persist.service')
        assert unit_path is not None, 'missing persistence service'
        included('/etc/systemd/system/initrd-switch-root.service.requires/nabos-persist.service')
        unit = configparser.ConfigParser(interpolation=None, strict=False)
        unit.read(unit_path)
        script_path = included(unit['Service']['ExecStart'], executable=True)
        assert script_path is not None, 'missing persistence script'
        script = script_path.read_text()
        included(script.splitlines()[0].removeprefix('#!'), executable=True)
        exports = dict(shlex.split(line.removeprefix('export '))[0].split('=', 1)
                       for line in script.splitlines() if line.startswith('export '))
        library = included(exports['NABOS_PERSIST_LIB'])
        seeds = included(exports['NABOS_STATE_SEEDS'])
        if library is not None and seeds is not None:
            paths = next(line.split('=', 1)[1] for line in library.read_text().splitlines()
                         if line.startswith('PERSIST='))
            for path in shlex.split(paths)[0].split():
                included(exports['NABOS_STATE_SEEDS'] + path)
            assert 'journal' not in paths and 'boot-logs' not in paths, 'logging must not gate boot health'

        collector_unit_path = included('/etc/systemd/system/nabos-initrd-log.service')
        assert collector_unit_path is not None, 'missing optional initrd log collector'
        collector_unit = configparser.ConfigParser(interpolation=None, strict=False)
        collector_unit.read(collector_unit_path)
        collector_path = included(shlex.split(collector_unit['Service']['ExecStart'])[0], executable=True)
        assert collector_path is not None, 'missing collector script'
        included(collector_path.read_text().splitlines()[0].removeprefix('#!'), executable=True)
        included('/bin/nabos-initrd-log', executable=True)
        assert collector_unit['Unit']['DefaultDependencies'] == 'false'
        assert 'systemd-journald.service' in collector_unit['Unit']['After'].split()
        assert 'shutdown.target' in collector_unit['Unit']['Conflicts'].split()
        assert 'shutdown.target' in collector_unit['Unit']['Before'].split()
        assert 'FailureAction' not in collector_unit['Unit']
        assert collector_unit['Service']['Restart'] == 'no'
        assert collector_unit['Service']['TimeoutStopSec'] == '2s'
        assert collector_unit['Service']['LimitFSIZE'] == '4194304'
        assert collector_unit['Service']['LimitCORE'] == '0'
        assert not (root / 'etc/systemd/system/initrd-switch-root.service.requires/nabos-initrd-log.service').exists()

        for command in ('sleep', 'mkdir', 'touch', 'rm', 'cat', 'sfdisk', 'partx',
                        'resize2fs', 'e2fsck', 'mount', 'mountpoint', 'umount',
                        'cp', 'mv', 'grep', 'tr', 'chmod', 'find', 'systemctl',
                        'timeout', 'journalctl', 'findmnt', 'stat', 'readlink', 'install'):
            candidates = [root / directory.lstrip('/') / command
                          for directory in exports['PATH'].split(':')]
            found = next((path for path in candidates if path.is_file() and
                          os.access(path, os.X_OK) and path.resolve().is_relative_to(root)), None)
            if found is None:
                errors.append(f'{command} is unavailable in the shipped PATH')
        if unit['Unit'].get('FailureAction') != 'reboot-force':
            errors.append('persistence failure must reboot to consume a U-Boot slot attempt')
        if errors:
            raise AssertionError('\n'.join(errors))
    print(f'PASS: {archive}: persistence, optional bounded logging, commands and failure policy')


if __name__ == '__main__':
    recovery = len(sys.argv) == 3 and sys.argv[1] == '--recovery'
    if len(sys.argv) != 2 and not recovery:
        sys.exit('Usage: python3 image/test-initrd.py [--recovery] INITRD')
    check(os.path.realpath(sys.argv[-1]), recovery=recovery)
