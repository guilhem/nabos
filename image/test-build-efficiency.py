#!/usr/bin/env python3
"""Exercise image orchestration with tiny files and mocked builders, without Nix builds."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

IMAGE = Path(__file__).resolve().parent
TARGET = 'zero2-arm64'
ROOTS = ['/nix/store/' + '0' * 32 + '-system', '/nix/store/' + '1' * 32 + '-uboot']
REVISION = 'a' * 40
MOCK = r'''#!/usr/bin/env python3
import json, os, pathlib, sys
name = pathlib.Path(sys.argv[0]).name
args = sys.argv[1:]
repo = pathlib.Path(os.environ['FIXTURE_REPO'])
with (repo / 'calls.jsonl').open('a') as log:
    log.write(json.dumps([name, args]) + '\n')
roots = json.loads(os.environ['FIXTURE_ROOTS'])
fail = os.environ.get('FAIL_STAGE')
def failed(stage):
    if fail == stage:
        sys.exit(42)
if name == 'nix':
    assert not any('key.pem' in arg or 'cert.pem' in arg for arg in args)
    if 'eval' in args:
        expr = args[args.index('--expr') + 1]
        if expr == 'builtins.currentSystem':
            print('x86_64-linux')
        else:
            assert expr.startswith('(let ') and expr.endswith(').cacheRoots'), expr
            assert '--raw' in args
            failed('eval')
            print('\n'.join(roots))
    elif 'build' in args:
        if '--expr' in args:
            assert args[args.index('always-allow-substitutes') + 1] == 'true'
        if any(arg.endswith('#uboot-sandbox') for arg in args):
            print(repo / 'sandbox')
        elif '--json' in args:
            print(json.dumps([{'outputs': {'out': root}} for root in roots]))
        else:
            assert (repo / 'dist' / os.environ['NABOS_TARGET'] /
                    ('cache-roots-' + os.environ['NABOS_TARGET'])).read_text().splitlines() == roots
            failed('nix')
            print(repo / 'payload')
    else:
        raise AssertionError(args)
elif name == 'git':
    if 'rev-parse' in args:
        print('a' * 40)
    else:
        assert 'status' in args
elif name == 'openssl' and args[0] == 'x509':
    assert '-noout' in args
elif name == 'openssl':
    pathlib.Path(args[args.index('-keyout') + 1]).write_text('private fixture key')
    pathlib.Path(args[args.index('-out') + 1]).write_text('public fixture certificate')
elif name == 'genimage':
    failed('sd')
    (pathlib.Path(args[args.index('--outputpath') + 1]) / 'sdcard.img').write_bytes(b'SD fixture')
elif name == 'rauc' and args[0] == 'info':
    assert args[1] == '--keyring=' + str(repo / 'certificate.pem')
    if os.environ.get('MUTATE_ARTIFACT'):
        path = pathlib.Path(os.environ['MUTATE_ARTIFACT'])
        path.write_bytes(path.read_bytes() + b'changed during validation')
    sys.stdout.write(pathlib.Path(args[-1]).read_text())
elif name == 'rauc':
    assert args[0] == 'bundle'
    bundle = pathlib.Path(args[-2])
    for file in ('rootfs.ext4', 'boot.vfat'):
        assert os.path.samefile(bundle / file, bundle.parent / 'images' / file)
    failed('rauc')
    pathlib.Path(args[-1]).write_bytes(b'signed bundle fixture')
elif name == 'xz':
    failed('xz')
    assert '-6' in args or '-3' in args or '--decompress' in args
    sys.stdout.buffer.write(b'compressed fixture')
elif name == 'dd':
    if not any(arg.startswith('if=') for arg in args):
        sys.stdin.buffer.read()
    destination = next(arg[3:] for arg in args if arg.startswith('of='))
    pathlib.Path(destination).write_bytes(b'extracted fixture')
elif name == 'fixture-test':
    assert len(args) == 7, args
    assert pathlib.Path(args[4]).name == 'data.ext4' and pathlib.Path(args[4]).is_file()
    assert os.environ['EXPECTED_VERSION'] == 'dev-local'
    failed('tests')
elif name == 'fixture-runtime':
    assert len(args) == 3 and pathlib.Path(args[2]).name == 'data.ext4', args
    failed('runtime')
elif name == 'sha256sum':
    os.execv(os.environ['REAL_SHA256SUM'], ['sha256sum', *args])
elif name == 'python3':
    # Geometry and initrd checks need a full image; this fixture exercises shell hashes.
    if args[0] == '-':
        sys.stdin.read()
    else:
        assert args[0].endswith('/test-initrd.py'), args
elif name == 'debugfs' and not os.environ.get('FIXTURE_VALIDATION'):
    if args[1] == 'cat /usr/share/nabos/ca.cert.pem':
        sys.stdout.write(pathlib.Path(args[-1]).parent.parent.joinpath('public-ca.pem').read_text())
elif name == 'debugfs' and os.environ.get('FIXTURE_VALIDATION'):
    request = args[1]
    if request == 'cat /boot/init':
        print('nabos_init=/nix/store/fixture/init')
    elif request == 'cat /boot/boot.scr':
        sys.stdout.write('x' * 72 + (repo / 'image/boot/slot.cmd').read_text())
    elif request == 'stat /boot/initrd':
        print('Size: 12')
    elif request.startswith('dump /boot/initrd '):
        pathlib.Path(request.removeprefix('dump /boot/initrd ')).write_bytes(b'initrd fixture')
    elif request == 'cat /usr/share/nabos/ca.cert.pem':
        sys.stdout.write((repo / 'certificate.pem').read_text())
    else:
        assert request in ('stat /nix/store', 'stat /rauc', 'stat /usr/share/nabos/ca.cert.pem'), request
        if request != 'stat /rauc':
            print('Type: regular User: 0 Group: 0 Mode: 0644')
elif name == 'unsquashfs':
    assert args[0] == '-cat', args
    if args[-1] in ('rauc-hook.sh', 'layout.sh'):
        sys.stdout.write((repo / 'image' / args[-1]).read_text())
    else:
        assert args[-1] == 'manifest.raucm', args
        print('filename=rauc-hook.sh\nhooks=install-check;')
else:
    assert name in ('fakeroot', 'mcopy', 'debugfs', 'go', 'e2fsck', 'mdir'), name
'''


class BuildEfficiency(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='nabos-build-test-')
        self.addCleanup(self.tmp.cleanup)
        self.repo = Path(self.tmp.name) / 'repo with spaces'
        image = self.repo / 'image'
        image.mkdir(parents=True)
        for file in ('build.sh', 'test-artifact.sh', 'benchmark.sh', 'manifest.raucm.in',
                     'sources.lock.json', 'genimage.cfg', 'rauc-hook.sh', 'layout.sh'):
            shutil.copy2(IMAGE / file, image / file)
        (image / 'boot').mkdir()
        for name in ('boot.cmd', 'slot.cmd'):
            shutil.copy2(IMAGE / 'boot' / name, image / 'boot' / name)
        for file, command in [('test.sh', 'fixture-test'), ('test-runtime.sh', 'fixture-runtime')]:
            (image / file).write_text('#!/usr/bin/env bash\nset -euo pipefail\n' + command + ' "$@"\n')
        (self.repo / 'services').mkdir()
        (self.repo / 'flake.lock').write_text(json.dumps({
            'nodes': {'root': {'inputs': {'nixpkgs': 'nixpkgs'}},
                      'nixpkgs': {'locked': {'rev': 'pinned-nixpkgs'}}}}))
        payload = self.repo / 'payload'
        payload.mkdir()
        for file in ('rootfs.ext4', 'boot.vfat', 'uboot.env'):
            (payload / file).write_bytes(file.encode())
            (payload / file).chmod(0o444)
        shutil.copy2(image / 'boot/boot.cmd', payload / 'boot.cmd')
        # The cache list must come from evaluation, not from a completed payload.
        (payload / 'build.json').write_text(json.dumps({
            'target': TARGET, 'version': 'dev-local', 'hardware_validated': False}))
        sandbox = self.repo / 'sandbox'
        sandbox.mkdir()
        (sandbox / 'SHA256SUMS').write_text('e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  empty\n')
        (sandbox / 'empty').touch()
        bin_dir = self.repo / 'bin'
        bin_dir.mkdir()
        mock = bin_dir / 'mock'
        mock.write_text(MOCK.replace('#!/usr/bin/env python3', '#!' + sys.executable, 1))
        mock.chmod(0o755)
        for command in ('nix', 'git', 'openssl', 'fakeroot', 'genimage', 'rauc', 'xz',
                        'dd', 'mcopy', 'debugfs', 'go', 'fixture-test', 'fixture-runtime'):
            (bin_dir / command).symlink_to(mock)
        self.env = os.environ | {'PATH': str(bin_dir) + ':' + os.environ['PATH'],
                                 'FIXTURE_REPO': str(self.repo), 'FIXTURE_ROOTS': json.dumps(ROOTS),
                                 'NABOS_NIX_SHELL': '1'}
        self.out = self.repo / 'dist' / TARGET

    def run_script(self, file, *args, fail=None):
        env = self.env | ({'FAIL_STAGE': fail} if fail else {})
        return subprocess.run(['bash', str(self.repo / 'image' / file), *args],
                              env=env, text=True, capture_output=True, timeout=20)

    def build(self, *options, fail=None):
        result = self.run_script('build.sh', TARGET, 'dev-local', '--development', *options, fail=fail)
        if fail:
            self.assertEqual(result.returncode, 42, result.stderr)
        else:
            self.assertEqual(result.returncode, 0, result.stderr)
        return result

    def calls(self, command):
        return [args for name, args in map(json.loads, (self.repo / 'calls.jsonl').read_text().splitlines())
                if name == command]

    def test_defaults_timings_links_and_checksum_layout(self):
        self.build()
        report = json.loads((self.out / f'build-{TARGET}.json').read_text())
        durations = report['durations_seconds']
        self.assertEqual(report['build_seconds'], durations['nix_build'])
        self.assertEqual(set(durations), {'nix_build', 'sd_assembly', 'rauc_bundle', 'xz', 'tests'})
        for seconds in durations.values():
            self.assertIsInstance(seconds, int)
            self.assertGreaterEqual(seconds, 0)
        self.assertEqual(report['compression'], dict(xz=3, rauc_zstd=6))
        self.assertIn('-3', self.calls('xz')[0])
        self.assertIn('--mksquashfs-args=-comp zstd -Xcompression-level 6', self.calls('rauc')[0])
        workspace = next((self.repo / 'build/nixos').iterdir())
        for file in ('rootfs.ext4', 'boot.vfat'):
            self.assertTrue(os.path.samefile(workspace / 'images' / file, workspace / 'bundle' / file))
            self.assertEqual((workspace / 'images' / file).read_bytes(), (self.repo / 'payload' / file).read_bytes())
            self.assertEqual((workspace / 'bundle' / file).stat().st_mode & 0o222, 0)
        self.assertFalse((workspace / 'signing').exists())
        expected = [f'nabos-{TARGET}.img.xz', f'nabos-{TARGET}.raucb', f'ca-{TARGET}.cert.pem',
                    f'build-{TARGET}.json', f'flake-{TARGET}.lock', f'boot-{TARGET}.cmd', f'cache-roots-{TARGET}']
        sums = self.out / f'SHA256SUMS-{TARGET}'
        self.assertEqual([line.split('  ')[1] for line in sums.read_text().splitlines()], expected)
        self.assertEqual(set(p.name for p in self.out.iterdir()), set(expected + [sums.name]))
        subprocess.run(['sha256sum', '--check', '--strict', sums.name], cwd=self.out,
                       check=True, capture_output=True)

    def test_deferred_tests(self):
        self.build('--defer-tests')
        report = json.loads((self.out / f'build-{TARGET}.json').read_text())
        self.assertEqual(report['compression'], dict(xz=3, rauc_zstd=6))
        self.assertIsNone(report['durations_seconds']['tests'])
        self.assertEqual(self.calls('fixture-test'), [])
        self.assertIn('-3', self.calls('xz')[0])
        self.assertIn('--mksquashfs-args=-comp zstd -Xcompression-level 6', self.calls('rauc')[0])

    def test_cache_roots_survive_build_and_packaging_failures(self):
        for stage in ('nix', 'sd', 'rauc', 'xz', 'tests'):
            with self.subTest(stage=stage):
                self.build(fail=stage)
                self.assertEqual((self.out / f'cache-roots-{TARGET}').read_text().splitlines(), ROOTS)
                for workspace in (self.repo / 'build/nixos').iterdir():
                    self.assertFalse((workspace / 'signing').exists())

    def test_failed_evaluation_removes_stale_cache_roots(self):
        self.out.mkdir(parents=True)
        roots = self.out / f'cache-roots-{TARGET}'
        roots.write_text('stale roots\n')
        self.build(fail='eval')
        self.assertFalse(roots.exists())

    def test_artifact_timings_do_not_modify_artifacts(self):
        self.build('--defer-tests')
        before = {p.name: p.read_bytes() for p in self.out.iterdir()}
        self.env |= {'EXPECTED_VERSION': 'dev-local', 'EXPECTED_DEVELOPMENT': 'true'}
        result = self.run_script('test-artifact.sh', TARGET, str(self.out))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual({p.name: p.read_bytes() for p in self.out.iterdir()}, before)
        report = self.repo / 'build/nix-tmp' / f'test-{TARGET}.json'
        measurements = json.loads(report.read_text())
        seconds = measurements.pop('durations_seconds')['tests']
        self.assertIsInstance(seconds, int)
        self.assertGreaterEqual(seconds, 0)
        self.assertEqual(measurements, dict(target=TARGET, version='dev-local', source_revision=REVISION))
        data_reads = [args for args in self.calls('dd') if 'skip=12804' in args]
        self.assertEqual(len(data_reads), 1)
        self.assertIn('of=' + self.calls('fixture-test')[0][4], data_reads[0])
        self.assertEqual(self.calls('fixture-test')[0][4], self.calls('fixture-runtime')[0][2])
        result = self.run_script('test-artifact.sh', TARGET, str(self.out), fail='runtime')
        self.assertEqual(result.returncode, 42, result.stderr)
        self.assertFalse(report.exists())

    def test_real_image_hashes_reject_corruption_and_mutation(self):
        shutil.copy2(IMAGE / 'test.sh', self.repo / 'image/test.sh')
        for command in ('sha256sum', 'python3', 'e2fsck', 'mdir', 'unsquashfs'):
            (self.repo / 'bin' / command).symlink_to('mock')
        self.env |= {'REAL_SHA256SUM': shutil.which('sha256sum'), 'FIXTURE_VALIDATION': '1',
                     'EXPECTED_VERSION': 'dev-local'}
        files = [self.repo / name for name in ('sdcard.img', 'rootfs\\ fixture.ext4', 'boot.vfat', 'data.ext4')]
        contents = [b'SD fixture', b'root fixture', b'boot fixture', b'data fixture']
        certificate = self.repo / 'certificate.pem'
        certificate.write_text('public fixture certificate')
        bundle = self.repo / 'bundle.json'
        bundle.write_text(json.dumps({'compatible': 'nabos-native-rauc-' + TARGET, 'version': 'dev-local',
                                     'images': [{'rootfs': {'checksum': hashlib.sha256(contents[1]).hexdigest()}},
                                                {'bootloader': {'checksum': hashlib.sha256(contents[2]).hexdigest()}}]}))
        args = [TARGET, *map(str, files), str(bundle), str(certificate)]
        for stage, paths in [('valid', [None]), ('corrupt', files[1:3]), ('mutate', files)]:
            for path in paths:
                with self.subTest(stage=stage, path=path):
                    for file, content in zip(files, contents):
                        file.write_bytes(content)
                    (self.repo / 'calls.jsonl').write_text('')
                    self.env.pop('MUTATE_ARTIFACT', None)
                    if stage == 'corrupt':
                        path.write_bytes(path.read_bytes() + b'corrupted before validation')
                    elif stage == 'mutate':
                        self.env['MUTATE_ARTIFACT'] = str(path)
                    result = self.run_script('test.sh', *args)
                    if stage == 'valid':
                        self.assertEqual(result.returncode, 0, result.stderr)
                        self.assertIn('originals unchanged', result.stdout)
                        self.assertEqual([file.read_bytes() for file in files], contents)
                    else:
                        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                    expected_hash_calls = 1 if stage == 'corrupt' else 2
                    self.assertEqual(self.calls('sha256sum'), [list(map(str, files))] * expected_hash_calls)
                    self.assertEqual(len(self.calls('unsquashfs')), 0 if stage == 'corrupt' else 3)
                    self.assertEqual(self.calls('dd'), [])
                    self.assertEqual(self.calls('e2fsck'), [['-fn', str(files[1])], ['-fn', str(files[3])]])
                    data_checks = [args for args in self.calls('debugfs') if '/rauc' in args[1]]
                    self.assertEqual(len(data_checks), 1)
                    trust_checks = [args for args in self.calls('debugfs') if '/usr/share/nabos/ca.cert.pem' in args[1]]
                    self.assertEqual(len(trust_checks), 2)
                    self.assertTrue(all(args[-1] == str(files[1]) for args in trust_checks))
                    self.assertTrue(all(args[-1] == str(files[3]) for args in data_checks))

    def test_existing_cache_benchmark_contract(self):
        for phase in ('cold', 'warm', 'version', 'nixpkgs'):
            with self.subTest(phase=phase):
                result = self.run_script('benchmark.sh', phase, TARGET, 'dev-local')
                self.assertEqual(result.returncode, 0, result.stderr)
                out = self.repo / 'dist/measurements' / TARGET / phase
                report = json.loads((out / f'build-{TARGET}.json').read_text())
                self.assertEqual(report['scope'], 'selected-cache-roots')
                self.assertIsInstance(report['build_seconds'], int)
                self.assertEqual((out / 'cache-roots.txt').read_text().splitlines(), ROOTS)


if __name__ == '__main__':
    unittest.main()
