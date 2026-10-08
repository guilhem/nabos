#!/usr/bin/env python3
"""Tiny downloaded artifacts and a fake gh runner; no network or dependencies."""
import copy
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch


SPEC = importlib.util.spec_from_file_location("edge_release", Path(__file__).with_name("edge-release.py"))
edge = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(edge)
SHA = "a" * 40
TAG = "edge-1.2.4.42"
REPO = "example/nabos"


def release(tag, draft=False, sha=SHA, id=1):
    return {"id": id, "draft": draft, **edge.release_metadata(tag, sha)}


class FakeGH:
    def __init__(self, releases=()):
        self.releases = copy.deepcopy(list(releases))
        self.assets = {}
        self.content = {}
        self.refs = []
        self.annotated = {}
        self.calls = []
        self.fail_upload = None
        self.uploads = 0
        self.omit_upload = None

    def run(self, args, **kwargs):
        self.calls.append((args[:], kwargs.get("input")))
        if args[:2] == ["release", "upload"]:
            assert args[-2:] == ["--repo", REPO] and "--clobber" not in args
            self.uploads += 1
            if self.uploads == self.fail_upload:
                raise ValueError("interrupted upload")
            target = next(r for r in self.releases if r["tag_name"] == args[2])
            assert target["draft"], "attempt to upload to published release"
            path = Path(args[3])
            if path.name != self.omit_upload:
                self.add_asset(target["id"], path.name, path.read_bytes())
            return subprocess.CompletedProcess(args, 0)
        assert args[0] == "api", args
        path = args[1]
        method = args[args.index("--method") + 1] if "--method" in args else "GET"
        data = json.loads(kwargs["input"]) if kwargs.get("input") else None
        root = f"repos/{REPO}"
        if "-H" in args:
            assert args[-1] == "Accept: application/octet-stream"
            kwargs["stdout"].write(self.content[int(path.rsplit("/", 1)[1])])
            return subprocess.CompletedProcess(args, 0)
        if path == f"{root}/releases?per_page=100":
            result = [self.releases[:1], self.releases[1:]]
        elif path.startswith(f"{root}/git/matching-refs/"):
            result = [self.refs]
        elif path.startswith(f"{root}/git/tags/"):
            result = {"object": self.annotated[path.rsplit("/", 1)[1]]}
        elif path == f"{root}/releases" and method == "POST":
            id = max([r["id"] for r in self.releases], default=0) + 1
            result = {"id": id, **{k: v for k, v in data.items() if k != "make_latest"}}
            assert data["draft"] is True and data["make_latest"] == "false"
            self.releases.append(result)
        elif path.startswith(f"{root}/releases/assets/") and method == "DELETE":
            id = int(path.rsplit("/", 1)[1])
            for assets in self.assets.values():
                assets[:] = [a for a in assets if a["id"] != id]
            result = None
        elif path.endswith("/assets?per_page=100"):
            id = int(path.split("/")[-2])
            result = [self.assets.get(id, [])[:3], self.assets.get(id, [])[3:]]
        else:
            id = int(path.rsplit("/", 1)[1])
            current = next(r for r in self.releases if r["id"] == id)
            if method == "DELETE":
                assert current["draft"] is False
                assert edge.edge_version(current["tag_name"]) is not None
                self.releases.remove(current)
                result = None
            elif method == "PATCH":
                assert data == {"draft": False, "prerelease": True, "make_latest": "false"}
                assert len(self.assets[id]) == 13, "published before complete upload"
                current.update({k: v for k, v in data.items() if k != "make_latest"})
                result = current
            else:
                assert method == "GET"
                result = current
        if "--paginate" in args:
            assert "--slurp" in args
        return subprocess.CompletedProcess(args, 0, stdout=json.dumps(result))

    def add_asset(self, release_id, name, content):
        assets = self.assets.setdefault(release_id, [])
        assert not any(a["name"] == name for a in assets)
        id = max(self.content.keys(), default=0) + 1
        self.content[id] = content
        asset = {"id": id, "name": name, "state": "uploaded", "size": len(content),
                 "digest": "sha256:" + hashlib.sha256(content).hexdigest()}
        assets.append(asset)
        return asset

    def mutations(self):
        return [(args, data) for args, data in self.calls
                if args[0] == "release" or ("--method" in args and args[args.index("--method") + 1] != "GET")]


class Versions(unittest.TestCase):
    def test_canonical_edge_parser_preserves_four_numbers(self):
        for tag, expected in [(TAG, (1, 2, 4, 42)), ("edge-0.0.0.0", (0, 0, 0, 0)),
                              ("edge-10.20.300.4000", (10, 20, 300, 4000))]:
            self.assertEqual(edge.edge_version(tag), expected)
        for tag in ("v" + TAG, "Edge-1.2.4.42", "edge-01.2.4.42", "edge-1.02.4.42",
                    "edge-1.2.04.42", "edge-1.2.4.042", "edge-1.2.4", "edge-1.2.4.42.0",
                    "edge-1.2.4.-1", "edge-1.2.4.42+build", "edge-1.2.4.42\n"):
            with self.subTest(tag=tag):
                self.assertIsNone(edge.edge_version(tag))

    def test_device_core_limits_include_exact_boundaries(self):
        maximum = "edge-999999.999999.999999." + "9" * 38
        self.assertEqual(len(maximum), 64)
        self.assertEqual(edge.edge_version(maximum), (999999, 999999, 999999, int("9" * 38)))
        for tag in (maximum + "9", "edge-1000000.0.0.1", "edge-0.1000000.0.1", "edge-0.0.1000000.1"):
            with self.subTest(tag=tag):
                self.assertIsNone(edge.edge_version(tag))

    def test_generation_rejects_patch_overflow_and_unsupported_core_or_length(self):
        self.assertEqual(edge.choose_version([release("v999999.999999.999998")], 42, SHA),
                         "edge-999999.999999.999999.42")
        prefix = "edge-0.0.1."
        run = int("9" * (64 - len(prefix)))
        self.assertEqual(len(edge.choose_version([release("v0.0.0")], run, SHA)), 64)
        for base, run in (("v0.0.0", run * 10), ("v1.2.999999", 42),
                          ("v1000000.0.0", 42), ("v0.1000000.0", 42)):
            with self.subTest(base=base, run=run):
                with self.assertRaisesRegex(ValueError, "support limits"):
                    edge.choose_version([release("v0.0.0"), release(base)], run, SHA)

    def test_largest_published_semver_core_includes_normal_prerelease(self):
        tags = ("v1.99.99", "v2.3.9", "v2.3.10-rc.2+build.9", "v2.3.10-beta",
                "v02.3.99", "v2.3.99-01", "2.3.99", "test", "edge-99.0.0.1", "v2.3.99junk")
        releases = [release(tag, id=i) for i, tag in enumerate(tags)]
        releases.append(release("v99.0.0", draft=True))
        self.assertEqual(edge.choose_version(releases, 42, SHA), "edge-2.3.11.42")

    def test_no_published_semver_aborts(self):
        for releases in ([], [release("v1.2.3", draft=True)], [release("test"), release(TAG)]):
            with self.assertRaisesRegex(ValueError, "no published"):
                edge.choose_version(releases, 42, SHA)

    def test_rerun_keeps_identity_after_stable_appears(self):
        for draft in (True, False):
            self.assertEqual(edge.choose_version([release("v9.0.0"), release(TAG, draft=draft)],
                                                 42, SHA), TAG)

    def test_run_collision_and_multiple_identities_fail(self):
        for draft in (True, False):
            with self.assertRaisesRegex(ValueError, "another commit"):
                edge.choose_version([release("v1.2.3"), release(TAG, draft=draft, sha="b" * 40)], 42, SHA)
        with self.assertRaisesRegex(ValueError, "multiple Edge"):
            edge.choose_version([release("v1.2.3"), release(TAG), release("edge-9.0.1.42")], 42, SHA)


class Publishing(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="nabos-edge-test-")
        self.addCleanup(self.tmp.cleanup)
        self.directory = Path(self.tmp.name) / "downloaded outputs"
        self.directory.mkdir()
        for target in edge.TARGETS:
            folder = self.directory / f"nabos-{target}"
            folder.mkdir()
            for name in edge.target_names(target):
                (folder / name).write_bytes(name.encode())
            (folder / f"build-{target}.json").write_text(json.dumps({
                "target": target, "version": TAG, "source_revision": SHA,
                "source_dirty": False, "development": False}))
            (folder / f"cache-roots-{target}").write_text("/nix/store/not-a-release-asset\n")
            (folder / f"cache-report-{target}.json").write_text("not checksum covered")
            self.rechecksum(target)
        self.fake = FakeGH([release("v1.2.3")])
        self.addCleanup(patch.stopall)
        patch.object(edge, "run_gh", side_effect=self.fake.run).start()
        self.gh = edge.GitHub(REPO)

    def rechecksum(self, target):
        folder = self.directory / f"nabos-{target}"
        names = (*edge.target_names(target), f"cache-roots-{target}")
        (folder / f"SHA256SUMS-{target}").write_text("".join(
            f"{edge.file_digest(folder / name)}  {name}\n" for name in names))

    def publish(self):
        edge.publish(self.gh, TAG, self.directory, SHA, 42)

    def published(self):
        return next(r for r in self.fake.releases if r["tag_name"] == TAG)

    def test_complete_outputs_then_publish_and_compatible_manifest(self):
        self.publish()
        current = self.published()
        self.assertFalse(current["draft"])
        assets = {a["name"]: a for a in self.fake.assets[current["id"]]}
        self.assertEqual(set(assets), {name for t in edge.TARGETS for name in edge.target_names(t)} | {"SHA256SUMS"})
        manifest = self.fake.content[assets["SHA256SUMS"]["id"]].decode()
        self.assertEqual(len(manifest.splitlines()), 12)
        for line in manifest.splitlines():
            digest, name = line.split("  ")
            self.assertEqual("sha256:" + digest, assets[name]["digest"])
        self.assertEqual(len([args for args, _ in self.fake.calls if "--paginate" in args and "releases?" in args[1]]), 2)

    def test_interrupted_draft_reused_without_replacing_assets(self):
        self.fake.fail_upload = 4
        with self.assertRaisesRegex(ValueError, "interrupted"):
            self.publish()
        current = self.published()
        self.assertTrue(current["draft"])
        self.assertEqual(len(self.fake.assets[current["id"]]), 3)
        first_ids = [a["id"] for a in self.fake.assets[current["id"]]]
        self.fake.fail_upload = None
        self.fake.releases.append(release("v9.0.0", id=3))
        self.publish()
        self.assertFalse(self.published()["draft"])
        self.assertEqual([a["id"] for a in self.fake.assets[current["id"]]][:3], first_ids)
        self.assertEqual(sum("POST" in args for args, _ in self.fake.calls), 1)

    def test_missing_uploaded_output_never_publishes_and_preserves_draft(self):
        self.fake.omit_upload = "SHA256SUMS"
        with self.assertRaisesRegex(ValueError, "missing assets"):
            self.publish()
        self.assertTrue(self.published()["draft"])
        self.fake.omit_upload = None
        self.publish()
        self.assertFalse(self.published()["draft"])

    def test_published_retry_is_read_only_even_without_api_digest(self):
        self.publish()
        snapshot = copy.deepcopy(self.fake.releases)
        assets = copy.deepcopy(self.fake.assets)
        self.fake.assets[self.published()["id"]][0]["digest"] = None
        self.fake.calls.clear()
        self.publish()
        self.assertEqual(self.fake.releases, snapshot)
        self.assertEqual(self.fake.mutations(), [])
        self.assertEqual([a["id"] for a in self.fake.assets[self.published()["id"]]],
                         [a["id"] for a in assets[self.published()["id"]]])
        self.assertTrue(any("-H" in args for args, _ in self.fake.calls))

    def test_download_fallback_checks_real_content_and_fails_closed(self):
        self.publish()
        asset = self.fake.assets[self.published()["id"]][0]
        asset.pop("digest")
        self.fake.content[asset["id"]] = b"corrupted downloaded content"
        self.fake.calls.clear()
        with self.assertRaisesRegex(ValueError, "existing asset differs"):
            self.publish()
        self.assertEqual(self.fake.mutations(), [])

    def test_published_incompatibilities_never_mutate(self):
        self.publish()
        snapshot = copy.deepcopy(self.fake.assets)
        metadata = copy.deepcopy(self.published())
        for defect in ("missing", "digest", "size", "state", "extra", "duplicate", "body", "target", "prerelease"):
            with self.subTest(defect=defect):
                self.fake.assets = copy.deepcopy(snapshot)
                self.published().update(metadata)
                assets = self.fake.assets[self.published()["id"]]
                if defect == "missing":
                    assets.pop()
                elif defect == "digest":
                    assets[0]["digest"] = "sha256:" + "0" * 64
                elif defect == "size":
                    assets[0]["size"] += 1
                elif defect == "state":
                    assets[0]["state"] = "starter"
                elif defect == "extra":
                    assets.append({"name": "unknown"})
                elif defect == "duplicate":
                    assets.append(assets[0])
                elif defect == "target":
                    self.published()["target_commitish"] = "b" * 40
                elif defect == "prerelease":
                    self.published()["prerelease"] = False
                else:
                    self.published()["body"] = "unexpected"
                self.fake.calls.clear()
                with self.assertRaises(ValueError):
                    self.publish()
                self.assertEqual(self.fake.mutations(), [])

    def test_draft_mismatched_asset_blocks_any_upload(self):
        self.fake.releases.append(release(TAG, draft=True, id=2))
        self.fake.add_asset(2, "SHA256SUMS", b"different")
        with self.assertRaisesRegex(ValueError, "existing asset differs"):
            self.publish()
        self.assertTrue(self.published()["draft"])
        self.assertEqual(self.fake.mutations(), [])

    def test_final_digest_mismatch_never_publishes(self):
        original = self.fake.add_asset

        def corrupt(id, name, content):
            asset = original(id, name, content)
            if name == "SHA256SUMS":
                asset["digest"] = "sha256:" + "0" * 64
            return asset

        with patch.object(self.fake, "add_asset", side_effect=corrupt):
            with self.assertRaisesRegex(ValueError, "existing asset differs"):
                self.publish()
        self.assertTrue(self.published()["draft"])
        self.assertFalse(any("PATCH" in args for args, _ in self.fake.calls))

    def test_empty_starter_asset_only_recovered_in_draft(self):
        self.fake.releases.append(release(TAG, draft=True, id=2))
        self.fake.add_asset(2, "SHA256SUMS", b"")["state"] = "starter"
        self.publish()
        self.assertFalse(self.published()["draft"])
        self.assertEqual(sum("DELETE" in args for args, _ in self.fake.calls), 1)

    def test_local_corruption_missing_files_and_unsafe_manifest_fail_before_creation(self):
        target = edge.TARGETS[0]
        folder = self.directory / f"nabos-{target}"
        manifest = folder / f"SHA256SUMS-{target}"
        original = manifest.read_text()
        for entry in (original.replace(".img.xz", ".missing"), original + original.splitlines()[0] + "\n",
                      original + "0" * 64 + "  ../outside\n", original.splitlines()[0] + "\n"):
            manifest.write_text(entry)
            with self.assertRaises(ValueError):
                self.publish()
            self.assertEqual(self.fake.calls, [])
        manifest.write_text(original)
        image = folder / f"nabos-{target}.img.xz"
        original_bytes = image.read_bytes()
        image.write_bytes(b"corrupted")
        with self.assertRaisesRegex(ValueError, "checksum mismatch"):
            self.publish()
        image.unlink()
        with self.assertRaisesRegex(ValueError, "missing regular"):
            self.publish()
        outside = self.directory / "outside"
        outside.write_bytes(original_bytes)
        image.symlink_to(outside)
        with self.assertRaisesRegex(ValueError, "missing regular"):
            self.publish()
        self.assertEqual(self.fake.calls, [])

    def test_build_report_exact_identity_and_false_booleans_required(self):
        target = edge.TARGETS[1]
        path = self.directory / f"nabos-{target}" / f"build-{target}.json"
        report = json.loads(path.read_text())
        for field, value in (("target", edge.TARGETS[0]), ("source_revision", "b" * 40),
                             ("version", "v" + TAG), ("source_dirty", True), ("source_dirty", 0),
                             ("development", True), ("development", 0), ("development", None)):
            path.write_text(json.dumps({**report, field: value}))
            self.rechecksum(target)
            with self.assertRaisesRegex(ValueError, "incompatible build"):
                self.publish()
            self.assertEqual(self.fake.calls, [])

    def test_bundle_limit_is_strict(self):
        path = self.directory / "nabos-zero-armv6" / "nabos-zero-armv6.raucb"
        with patch.object(edge, "BUNDLE_LIMIT", path.stat().st_size):
            with self.assertRaisesRegex(ValueError, "smaller than 2 GiB"):
                self.publish()
        self.assertEqual(self.fake.calls, [])

    def test_same_run_collision_and_wrong_tag_rejected(self):
        self.fake.releases.append(release("edge-9.0.1.42", draft=True, id=2))
        with self.assertRaisesRegex(ValueError, "differs from existing"):
            self.publish()
        self.assertEqual(self.fake.mutations(), [])
        for tag in ("v" + TAG, "edge-1.2.4.43"):
            with self.assertRaisesRegex(ValueError, "canonical Edge"):
                edge.publish(self.gh, tag, self.directory, SHA, 42)

    def test_preexisting_lightweight_and_annotated_tag_must_match_sha(self):
        self.fake.refs = [{"ref": "refs/tags/" + TAG, "object": {"type": "tag", "sha": "tag-sha"}},
                          {"ref": "refs/tags/" + TAG + "0", "object": {"type": "commit", "sha": "ignored"}}]
        self.fake.annotated["tag-sha"] = {"type": "commit", "sha": "b" * 40}
        with self.assertRaisesRegex(ValueError, "another commit"):
            self.publish()
        self.assertEqual(self.fake.mutations(), [])
        self.fake.annotated["tag-sha"]["sha"] = SHA
        self.publish()

    def test_lightweight_tag_collision_fails_before_release_creation(self):
        self.fake.refs = [{"ref": "refs/tags/" + TAG,
                           "object": {"type": "commit", "sha": "b" * 40}}]
        with self.assertRaisesRegex(ValueError, "another commit"):
            self.publish()
        self.assertEqual(self.fake.mutations(), [])

    def test_parent_positional_publish_cli(self):
        env = {"GITHUB_REPOSITORY": REPO, "GITHUB_SHA": SHA, "GITHUB_RUN_NUMBER": "42"}
        with patch.dict(os.environ, env), \
                patch.object(edge.sys, "argv", ["edge-release.py", "publish", TAG, str(self.directory)]), \
                patch.object(edge.sys, "stdout", new_callable=io.StringIO) as out:
            self.assertEqual(edge.main(), 0)
            self.assertEqual(out.getvalue(), TAG + "\n")
        self.assertFalse(self.published()["draft"])

    def test_retention_rechecks_identity_before_deletion(self):
        self.fake.releases.extend(release(f"edge-1.2.4.{i}", id=100 + i) for i in range(1, 32))
        original = self.fake.run

        def changed(args, **kwargs):
            if args[1] == f"repos/{REPO}/releases/102":
                next(r for r in self.fake.releases if r["id"] == 102)["draft"] = True
            return original(args, **kwargs)

        with patch.object(edge, "run_gh", side_effect=changed):
            with self.assertRaisesRegex(ValueError, "changed during retention"):
                self.publish()
        self.assertFalse(any("DELETE" in args for args, _ in self.fake.calls))

    def test_concurrent_retention_removal_before_read_or_delete_is_safe(self):
        for phase in ("GET", "DELETE"):
            with self.subTest(phase=phase):
                protected = [release("v1.2.3"), release("test", id=2),
                             release("edge-01.2.4.1", id=3), release("edge-1.2.4.99", draft=True, id=4),
                             release("edge-1000000.0.0.1", id=5), release("edge-0.0.0." + "9" * 54, id=6)]
                self.fake.releases = copy.deepcopy(protected) + [
                    release(f"edge-1.2.4.{i}", id=100 + i) for i in range(1, 32)]
                self.fake.calls.clear()
                original = self.fake.run

                def already_removed(args, **kwargs):
                    if args[1] == f"repos/{REPO}/releases/101" and args[args.index("--method") + 1] == phase:
                        self.fake.calls.append((args[:], kwargs.get("input")))
                        self.fake.releases[:] = [r for r in self.fake.releases if r["id"] != 101]
                        raise edge.GitHubError(1, 404)
                    return original(args, **kwargs)

                with patch.object(edge, "run_gh", side_effect=already_removed):
                    edge.prune(self.gh)
                self.assertEqual(len(self.fake.releases), len(protected) + 30)
                for current in protected:
                    self.assertIn(current, self.fake.releases)
                self.assertEqual([args[1] for args, _ in self.fake.mutations()],
                                 [f"repos/{REPO}/releases/101"] if phase == "DELETE" else [])

    def test_retention_only_ignores_404_on_candidate_reads_and_deletes(self):
        self.fake.releases.extend(release(f"edge-1.2.4.{i}", id=100 + i) for i in range(1, 32))
        original = self.fake.run
        for phase in ("GET", "DELETE"):
            for status in (None, 403, 500):
                with self.subTest(phase=phase, status=status):
                    def failed(args, **kwargs):
                        if args[1] == f"repos/{REPO}/releases/101" and args[args.index("--method") + 1] == phase:
                            raise edge.GitHubError(1, status)
                        return original(args, **kwargs)

                    with patch.object(edge, "run_gh", side_effect=failed):
                        with self.assertRaises(edge.GitHubError):
                            edge.prune(self.gh)
        with patch.object(edge, "run_gh", side_effect=edge.GitHubError(1, 404)):
            with self.assertRaises(edge.GitHubError):
                edge.prune(self.gh)
            with self.assertRaises(edge.GitHubError):
                self.publish()

    def test_retention_uses_numeric_four_triplet_order_not_time(self):
        others = [release(f"edge-1.2.4.{i}", id=100 + i) for i in range(1, 36)]
        others += [release("edge-2.0.0.1", id=201), release("edge-1.2.4.999", draft=True, id=202),
                   release("edge-01.2.4.999", id=203), release("test", id=204),
                   release("v3.0.0-rc.1", id=205)]
        for i, current in enumerate(others):
            current["published_at"] = f"2099-01-{99 - i}"
        self.fake.releases.extend(others)
        untouched = copy.deepcopy([r for r in self.fake.releases
                                   if r["draft"] or edge.edge_version(r["tag_name"]) is None])
        self.publish()
        published_edges = [r for r in self.fake.releases
                           if not r["draft"] and edge.edge_version(r["tag_name"]) is not None]
        self.assertEqual(len(published_edges), 30)
        self.assertEqual({r["tag_name"] for r in published_edges},
                         {TAG, "edge-2.0.0.1"} | {f"edge-1.2.4.{i}" for i in range(8, 36)})
        for current in untouched:
            self.assertIn(current, self.fake.releases)


class ProcessAndCLI(unittest.TestCase):
    def test_failed_gh_carries_only_status_and_never_echoes_diagnostics(self):
        for stderr, expected in (("secret gh: Not Found (HTTP 404)", 404),
                                 (b"secret gh: Forbidden (HTTP 403)", 403),
                                 ("secret gh: Server error (HTTP 500)", 500),
                                 ("secret asset 404 missing", None)):
            with self.subTest(stderr=stderr), patch.dict(os.environ, {"GH_TOKEN": "secret"}), \
                    patch.object(edge.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, stderr=stderr)):
                with self.assertRaises(edge.GitHubError) as error:
                    edge.run_gh(["api", "example"], stdout=subprocess.PIPE)
                self.assertEqual(error.exception.status, expected)
                self.assertEqual(str(error.exception), "gh command failed (exit 1)")

    def test_gh_runner_uses_only_inherited_token_and_suppresses_diagnostics(self):
        env = {"GH_TOKEN": "private-fixture", "GITHUB_TOKEN": "other", "GH_ENTERPRISE_TOKEN": "other",
               "GITHUB_ENTERPRISE_TOKEN": "other", "GH_DEBUG": "api"}
        with patch.dict(os.environ, env, clear=True), patch.object(edge.subprocess, "run") as runner:
            runner.return_value = subprocess.CompletedProcess([], 1, stderr="private-fixture")
            with self.assertRaisesRegex(ValueError, r"^gh command failed \(exit 1\)$"):
                edge.run_gh(["api", "example"], stdout=subprocess.PIPE)
            kwargs = runner.call_args.kwargs
            self.assertEqual(kwargs["env"]["GH_TOKEN"], "private-fixture")
            self.assertFalse(any(name in kwargs["env"] for name in env if name != "GH_TOKEN"))
            self.assertEqual(kwargs["env"]["GH_HOST"], "github.com")
        with patch.dict(os.environ, {}, clear=True), patch.object(edge.subprocess, "run") as runner:
            with self.assertRaisesRegex(ValueError, "GH_TOKEN is required"):
                edge.run_gh(["api", "example"])
            runner.assert_not_called()

    def test_cli_version_stdout_is_only_raw_tag_and_failure_is_nonzero(self):
        env = {"GITHUB_REPOSITORY": REPO, "GITHUB_SHA": SHA, "GITHUB_RUN_NUMBER": "42"}
        fake = FakeGH([release("v1.2.3")])
        with patch.dict(os.environ, env), patch.object(edge, "run_gh", side_effect=fake.run), \
                patch.object(edge.sys, "argv", ["edge-release.py", "version"]), \
                patch.object(edge.sys, "stdout", new_callable=io.StringIO) as out:
            self.assertEqual(edge.main(), 0)
            self.assertEqual(out.getvalue(), TAG + "\n")
        with patch.dict(os.environ, {**env, "GITHUB_SHA": "short"}), \
                patch.object(edge.sys, "argv", ["edge-release.py", "version"]), \
                patch.object(edge.sys, "stderr", new_callable=io.StringIO) as err:
            self.assertEqual(edge.main(), 1)
            self.assertIn("full commit SHA", err.getvalue())


if __name__ == "__main__":
    unittest.main()
