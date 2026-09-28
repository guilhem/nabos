#!/usr/bin/env python3
"""Fetch the reviewed, content-addressed image inputs. No build-time latest URLs."""

import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import tarfile
import time
import urllib.request

LOCK = Path(__file__).with_name("sources.lock.json")


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def download(url, expected, destination):
    if not url.startswith("https://") or not re.fullmatch(r"[0-9a-f]{64}", expected):
        raise ValueError("input requires HTTPS and a SHA-256 digest")
    destination = Path(destination)
    if destination.exists() and digest(destination) == expected:
        return
    destination.parent.mkdir(parents=True, exist_ok=True)
    temporary = destination.with_suffix(destination.suffix + ".part")
    for attempt in range(3):
        try:
            with urllib.request.urlopen(url, timeout=120) as source, temporary.open("wb") as output:
                while block := source.read(1024 * 1024):
                    output.write(block)
                output.flush()
                os.fsync(output.fileno())
            if digest(temporary) != expected:
                raise ValueError(f"checksum mismatch: {url}")
            temporary.replace(destination)
            return
        except (OSError, ValueError):
            temporary.unlink(missing_ok=True)
            if attempt == 2:
                raise
            time.sleep(attempt + 1)


def unpack(archive, directory):
    """All inputs have a single top-level source directory; reject escaping paths."""
    directory.mkdir(parents=True, exist_ok=True)
    with tarfile.open(archive, "r:gz") as source:
        members = source.getmembers()
        roots = {PurePosixPath(member.name).parts[0] for member in members if PurePosixPath(member.name).parts}
        if len(roots) != 1:
            raise ValueError("source archive must have exactly one root directory")
        name = roots.pop()
        if name in ("/", ".."):
            raise ValueError("source archive root must be relative")
        source.extractall(directory, filter="data")
        root = directory / name
        if not root.is_dir() or root.is_symlink():
            raise ValueError("source archive root must be a directory")
        return root


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("target", choices=("zero-armv6", "zero2-arm64"))
    parser.add_argument("directory", type=Path)
    parser.add_argument("--sources-only", action="store_true")
    args = parser.parse_args()
    lock = json.loads(LOCK.read_text())
    target = lock["targets"][args.target]
    args.directory.mkdir(parents=True, exist_ok=True)
    if not args.sources_only:
        download(target["image_url"], target["image_sha256"], args.directory / "raspios.img.xz")
    for name, spec in lock["sources"].items():
        if name == "lva" and args.target != "zero2-arm64":
            continue
        archive = args.directory / (name + ".tar.gz")
        download(spec["url"], spec["sha256"], archive)
    print(f"Verified inputs for {args.target}")


if __name__ == "__main__":
    main()
