#!/usr/bin/env python3
"""Check the shipped initrd without mounting filesystems or executing ARM code."""
import configparser
import os
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile


def check(archive):
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

        for command in ('sleep', 'mkdir', 'touch', 'rm', 'cat', 'sfdisk', 'partx',
                        'resize2fs', 'e2fsck', 'mount', 'mountpoint', 'umount',
                        'cp', 'mv', 'grep', 'tr', 'chmod', 'find'):
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
    print(f'PASS: {archive}: persistence library, seeds, interpreter, commands and failure action')


if __name__ == '__main__':
    if len(sys.argv) != 2:
        sys.exit('Usage: python3 image/test-initrd.py INITRD')
    check(os.path.realpath(sys.argv[1]))
