#!/usr/bin/env python3
"""Publish exact image-test outputs. Workflow owns validation ordering and rollout gate.

Environment: GH_TOKEN, GITHUB_REPOSITORY, GITHUB_SHA, GITHUB_RUN_NUMBER.
python3 image/edge-release.py version
python3 image/edge-release.py publish edge-A.B.C.RUN dist
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile


NUMBER = r"(?:0|[1-9][0-9]*)"
EDGE = re.compile(r"edge-" + r"\.".join([f"({NUMBER})"] * 4))
PRE = r"(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)"
SEMVER = re.compile(
    "v" + r"\.".join([f"({NUMBER})"] * 3)
    + rf"(?:-{PRE}(?:\.{PRE})*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?"
)
TARGETS = ("zero-armv6", "zero2-arm64")
BUNDLE_LIMIT = 2 * 1024**3


def edge_version(tag):
    if len(tag) > 64:
        return None
    match = EDGE.fullmatch(tag)
    if not match:
        return None
    version = tuple(map(int, match.groups()))
    return version if all(number <= 999999 for number in version[:3]) else None


class GitHubError(ValueError):
    def __init__(self, returncode, status=None):
        self.status = status
        super().__init__(f"gh command failed (exit {returncode})")


def run_gh(args, **kwargs):
    env = os.environ.copy()
    if not env.get("GH_TOKEN"):
        raise ValueError("GH_TOKEN is required")
    for name in ("GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GH_DEBUG"):
        env.pop(name, None)
    env.update(GH_HOST="github.com", GH_PROMPT_DISABLED="1")
    result = subprocess.run(["gh", *args], env=env, stderr=subprocess.PIPE, **kwargs)
    if result.returncode:
        # Do not echo gh diagnostics, which can contain authentication details.
        stderr = result.stderr or ""
        if isinstance(stderr, bytes):
            stderr = stderr.decode("utf-8", errors="replace")
        status = re.search(r"\(HTTP ([0-9]{3})\)", stderr)
        raise GitHubError(result.returncode, int(status[1]) if status else None)
    return result


class GitHub:
    def __init__(self, repo):
        self.repo = repo
        self.root = f"repos/{repo}"

    def api(self, path, method="GET", data=None, paginate=False):
        args = ["api", path, "--method", method]
        if paginate:
            args += ["--paginate", "--slurp"]
        payload = None
        if data is not None:
            args += ["--input", "-"]
            payload = json.dumps(data)
        raw = run_gh(args, input=payload, stdout=subprocess.PIPE, text=True).stdout
        result = json.loads(raw) if raw.strip() else None
        return [item for page in result for item in page] if paginate else result

    def releases(self):
        return self.api(f"{self.root}/releases?per_page=100", paginate=True)

    def assets(self, release):
        return self.api(f"{self.root}/releases/{release['id']}/assets?per_page=100", paginate=True)

    def check_tag(self, tag, sha):
        refs = self.api(f"{self.root}/git/matching-refs/tags/{tag}?per_page=100", paginate=True)
        for ref in refs:
            if ref["ref"] != f"refs/tags/{tag}":
                continue
            obj = ref["object"]
            seen = set()
            while obj["type"] == "tag":
                if obj["sha"] in seen:
                    raise ValueError("cyclic annotated tag")
                seen.add(obj["sha"])
                obj = self.api(f"{self.root}/git/tags/{obj['sha']}")["object"]
            if obj["type"] != "commit" or obj["sha"] != sha:
                raise ValueError(f"tag {tag} points to another commit")

    def upload(self, tag, path):
        run_gh(["release", "upload", tag, str(path), "--repo", self.repo], stdout=subprocess.PIPE)

    def asset_digest(self, asset):
        digest = asset.get("digest")
        if digest is not None:
            if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
                raise ValueError(f"unsupported digest for {asset['name']}")
            return digest[7:]
        # Older assets may have no API digest; hash a streamed download instead.
        with tempfile.TemporaryFile() as output:
            run_gh(["api", f"{self.root}/releases/assets/{asset['id']}",
                    "-H", "Accept: application/octet-stream"], stdout=output)
            output.seek(0)
            return hash_file(output)


def context():
    repo = os.environ.get("GITHUB_REPOSITORY", "")
    sha = os.environ.get("GITHUB_SHA", "")
    run = os.environ.get("GITHUB_RUN_NUMBER", "")
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo):
        raise ValueError("GITHUB_REPOSITORY must be owner/repo")
    if not re.fullmatch(r"[0-9a-f]{40}", sha):
        raise ValueError("GITHUB_SHA must be a full commit SHA")
    if not re.fullmatch(r"[1-9][0-9]*", run):
        raise ValueError("GITHUB_RUN_NUMBER must be a positive canonical integer")
    return repo, sha, int(run)


def same_run(releases, run, sha):
    matches = []
    for release in releases:
        version = edge_version(release["tag_name"])
        if version is not None and version[3] == run:
            if release["target_commitish"] != sha:
                raise ValueError(f"Edge run {run} already belongs to another commit")
            matches.append(release)
    if len(matches) > 1:
        raise ValueError(f"multiple Edge identities for run {run}")
    return matches[0] if matches else None


def choose_version(releases, run, sha):
    bases = [tuple(map(int, match.groups())) for release in releases
             if release["draft"] is False
             and (match := SEMVER.fullmatch(release["tag_name"]))]
    if not bases:
        raise ValueError("no published v-prefixed SemVer release")
    existing = same_run(releases, run, sha)
    if existing:
        return existing["tag_name"]
    major, minor, patch = max(bases)
    tag = f"edge-{major}.{minor}.{patch + 1}.{run}"
    if edge_version(tag) is None:
        raise ValueError("generated Edge tag exceeds device-core support limits")
    return tag


def hash_file(file):
    digest = hashlib.sha256()
    for block in iter(lambda: file.read(1024 * 1024), b""):
        digest.update(block)
    return digest.hexdigest()


def file_digest(path):
    with path.open("rb") as file:
        return hash_file(file)


def regular_file(path):
    if path.is_symlink() or not path.is_file():
        raise ValueError(f"missing regular artifact: {path.name}")


def target_names(target):
    return (f"nabos-{target}.img.xz", f"nabos-{target}.raucb", f"ca-{target}.cert.pem",
            f"build-{target}.json", f"flake-{target}.lock", f"boot-{target}.cmd")


def artifacts(directory, tag, sha):
    outputs = {}
    for target in TARGETS:
        folder = directory / f"nabos-{target}"
        if folder.is_symlink() or not folder.is_dir():
            raise ValueError(f"missing artifact directory: {folder.name}")
        manifest = folder / f"SHA256SUMS-{target}"
        regular_file(manifest)
        required = set(target_names(target))
        allowed = required | {f"cache-roots-{target}"}
        checksums = {}
        for line in manifest.read_text().splitlines():
            match = re.fullmatch(r"([0-9a-f]{64}) [ *](?:\./)?([A-Za-z0-9_.-]+)", line)
            if not match or match[2] not in allowed or match[2] in checksums:
                raise ValueError(f"invalid checksum entry in {manifest.name}")
            checksums[match[2]] = match[1]
        if not required <= checksums.keys():
            raise ValueError(f"incomplete checksum manifest: {manifest.name}")
        for name, digest in checksums.items():
            path = folder / name
            regular_file(path)
            if file_digest(path) != digest:
                raise ValueError(f"checksum mismatch: {name}")
            if name in required:
                if path.stat().st_size == 0:
                    raise ValueError(f"empty artifact: {name}")
                outputs[name] = (path, digest)
        report = json.loads((folder / f"build-{target}.json").read_text())
        if (not isinstance(report, dict) or report.get("target") != target or report.get("source_revision") != sha
                or report.get("version") != tag or report.get("source_dirty") is not False
                or report.get("development") is not False):
            raise ValueError(f"incompatible build report: {target}")
        if (folder / f"nabos-{target}.raucb").stat().st_size >= BUNDLE_LIMIT:
            raise ValueError(f"bundle must be smaller than 2 GiB: {target}")
    return outputs


def release_metadata(tag, sha):
    return {"tag_name": tag, "target_commitish": sha, "name": tag,
            "body": f"NabOS Edge build {tag} ({sha}).", "prerelease": True}


def check_metadata(release, tag, sha):
    if any(release.get(key) != value for key, value in release_metadata(tag, sha).items()):
        raise ValueError("existing Edge release metadata differs")
    if type(release.get("draft")) is not bool:
        raise ValueError("invalid release draft state")


def check_asset(gh, asset, path, digest):
    if (asset.get("state") != "uploaded" or asset.get("size") != path.stat().st_size
            or gh.asset_digest(asset) != digest):
        raise ValueError(f"existing asset differs: {asset['name']}")


def asset_map(assets, outputs):
    result = {}
    for asset in assets:
        name = asset["name"]
        if name not in outputs or name in result:
            raise ValueError(f"unexpected or duplicate release asset: {name}")
        result[name] = asset
    return result


def prune(gh):
    edges = [release for release in gh.releases()
             if release["draft"] is False and edge_version(release["tag_name"]) is not None]
    edges.sort(key=lambda release: edge_version(release["tag_name"]), reverse=True)
    for release in edges[30:]:
        # Recheck before deletion: never delete a release that became a draft or changed identity.
        path = f"{gh.root}/releases/{release['id']}"
        try:
            current = gh.api(path)
            if current["draft"] is not False or current["tag_name"] != release["tag_name"]:
                raise ValueError("release changed during retention")
            gh.api(path, method="DELETE")
        except GitHubError as error:
            if error.status != 404:
                raise


def publish(gh, tag, directory, sha, run):
    version = edge_version(tag)
    if version is None or version[3] != run:
        raise ValueError("tag must be canonical Edge with this GITHUB_RUN_NUMBER")
    outputs = artifacts(directory, tag, sha)
    with tempfile.TemporaryDirectory(prefix="nabos-edge-") as tmp:
        manifest = Path(tmp) / "SHA256SUMS"
        manifest.write_text("".join(f"{digest}  {name}\n"
                                    for name, (_, digest) in sorted(outputs.items())))
        outputs[manifest.name] = (manifest, file_digest(manifest))
        existing = same_run(gh.releases(), run, sha)
        if existing and existing["tag_name"] != tag:
            raise ValueError("tag differs from existing Edge run identity")
        gh.check_tag(tag, sha)
        if existing is None:
            existing = gh.api(f"{gh.root}/releases", method="POST",
                              data={**release_metadata(tag, sha), "draft": True, "make_latest": "false"})
        path = f"{gh.root}/releases/{existing['id']}"
        release = gh.api(path)
        check_metadata(release, tag, sha)
        remote = asset_map(gh.assets(release), outputs)
        # Check every existing asset before making any upload or recovery deletion.
        for name, asset in remote.items():
            if release["draft"] and asset.get("state") == "starter" and asset.get("size") == 0:
                continue
            check_asset(gh, asset, *outputs[name])
        if not release["draft"]:
            if remote.keys() != outputs.keys():
                raise ValueError("published Edge release has missing assets")
        else:
            for name, (file, _) in outputs.items():
                if name in remote:
                    if remote[name].get("state") != "starter":
                        continue
                    gh.api(f"{gh.root}/releases/assets/{remote[name]['id']}", method="DELETE")
                gh.upload(tag, file)
            release = gh.api(path)
            check_metadata(release, tag, sha)
            complete = asset_map(gh.assets(release), outputs)
            if complete.keys() != outputs.keys():
                raise ValueError("Edge release has missing assets; kept draft")
            for name, asset in complete.items():
                check_asset(gh, asset, *outputs[name])
            gh.check_tag(tag, sha)
            if release["draft"]:
                published = gh.api(path, method="PATCH", data={"draft": False, "prerelease": True,
                                                              "make_latest": "false"})
                check_metadata(published, tag, sha)
                if published["draft"]:
                    raise ValueError("Edge release is still a draft")
    prune(gh)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("version")
    command = commands.add_parser("publish")
    command.add_argument("tag")
    command.add_argument("artifacts_dir", type=Path)
    args = parser.parse_args()
    try:
        repo, sha, run = context()
        gh = GitHub(repo)
        if args.command == "version":
            print(choose_version(gh.releases(), run, sha))
        else:
            publish(gh, args.tag, args.artifacts_dir, sha, run)
            print(args.tag)
    except (ValueError, OSError, KeyError, TypeError) as error:
        print(f"edge-release: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
