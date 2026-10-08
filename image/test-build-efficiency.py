#!/usr/bin/env python3
"""Exercise image orchestration with tiny files and mocked builders, without Nix builds."""
import json
import os
from pathlib import Path
import shutil
import subprocess
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
elif name == 'openssl':
    pathlib.Path(args[args.index('-keyout') + 1]).write_text('private fixture key')
    pathlib.Path(args[args.index('-out') + 1]).write_text('public fixture certificate')
elif name == 'genimage':
    failed('sd')
    (pathlib.Path(args[args.index('--outputpath') + 1]) / 'sdcard.img').write_bytes(b'SD fixture')
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
    assert os.environ['EXPECTED_VERSION'] == 'dev-local'
    failed('tests')
elif name == 'fixture-runtime':
    failed('runtime')
else:
    assert name in ('fakeroot', 'mcopy', 'debugfs', 'go'), name
'''


class BuildEfficiency(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='nabos-build-test-')
        self.addCleanup(self.tmp.cleanup)
        self.repo = Path(self.tmp.name) / 'repo with spaces'
        image = self.repo / 'image'
        image.mkdir(parents=True)
        for file in ('build.sh', 'test-artifact.sh', 'benchmark.sh', 'manifest.raucm.in',
                     'sources.lock.json', 'genimage.cfg'):
            shutil.copy2(IMAGE / file, image / file)
        (image / 'boot').mkdir()
        shutil.copy2(IMAGE / 'boot/boot.cmd', image / 'boot/boot.cmd')
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
        mock.write_text(MOCK)
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
        result = self.run_script('test-artifact.sh', TARGET, str(self.out), fail='runtime')
        self.assertEqual(result.returncode, 42, result.stderr)
        self.assertFalse(report.exists())

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
