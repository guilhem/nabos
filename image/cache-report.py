#!/usr/bin/env python3
"""Measure runtime closures and public cache metadata; never inspect a token."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import json
from pathlib import Path
import re
import subprocess
from urllib.error import HTTPError
from urllib.request import Request, urlopen

UPSTREAM = "https://cache.nixos.org"
CACHIX = "https://nabos.cachix.org"
STORE_PATH = re.compile(r"/nix/store/[0-9abcdfghijklmnpqrsvwxyz]{32}-[^/\s]+")


def store_path(value):
    if not isinstance(value, str) or not STORE_PATH.fullmatch(value):
        raise ValueError(f"invalid store path: {value!r}")
    return value


def natural(value):
    if isinstance(value, bool) or not isinstance(value, int) or value < 0:
        raise ValueError(f"invalid size: {value!r}")
    return value


def parse_narinfo(text, expected_path):
    fields = {}
    for line in text.splitlines():
        key, separator, value = line.partition(": ")
        if not separator:
            raise ValueError("malformed narinfo line")
        if key in {"StorePath", "URL", "FileSize", "NarSize"}:
            if key in fields:
                raise ValueError(f"duplicate narinfo field: {key}")
            fields[key] = value
    if fields.get("StorePath") != store_path(expected_path):
        raise ValueError("narinfo StorePath does not match the requested path")
    url = fields.get("URL", "")
    if not re.fullmatch(r"nar/[A-Za-z0-9][A-Za-z0-9._-]*", url):
        raise ValueError(f"unexpected narinfo URL: {url!r}")
    for key in ("FileSize", "NarSize"):
        if not re.fullmatch(r"[0-9]+", fields.get(key, "")):
            raise ValueError(f"invalid narinfo {key}")
    return {"url": url, "file_size": int(fields["FileSize"]),
            "nar_size": int(fields["NarSize"])}


def fetch_narinfo(cache, path):
    name = Path(store_path(path)).name.split("-", 1)[0]
    request = Request(f"{cache}/{name}.narinfo",
                      headers={"User-Agent": "nabos-cache-report/1.0"})
    try:
        with urlopen(request, timeout=30) as response:
            return parse_narinfo(response.read(1024 * 1024).decode(), path)
    except HTTPError as error:
        if error.code == 404:
            return None
        raise


def closure_entries(data):
    if isinstance(data, dict):
        entries = [{**entry, "path": path} for path, entry in data.items()]
    elif isinstance(data, list):
        entries = data
    else:
        raise ValueError("nix path-info JSON must be an object or list")
    result = {}
    for entry in entries:
        path, size = store_path(entry["path"]), natural(entry["narSize"])
        if path in result and result[path] != size:
            raise ValueError("conflicting duplicate closure path")
        result[path] = size
    if not result:
        raise ValueError("empty runtime closure")
    return result


def summarize(entries):
    paths, files = {}, {}
    for entry in entries:
        path = store_path(entry["path"])
        natural(entry["nar_size"])
        if type(entry["upstream"]) is not bool:
            raise ValueError("upstream presence must be boolean")
        if path in paths and paths[path] != entry:
            raise ValueError("conflicting duplicate report path")
        paths[path] = entry
        cached = entry["cachix"]
        if cached is not None:
            if not isinstance(cached, dict):
                raise ValueError("cachix presence must be metadata or null")
            url = cached["url"]
            if not re.fullmatch(r"nar/[A-Za-z0-9][A-Za-z0-9._-]*", url):
                raise ValueError("unexpected cached NAR URL")
            size = natural(cached["file_size"])
            if natural(cached["nar_size"]) != entry["nar_size"]:
                raise ValueError("cached NAR size differs from local closure")
            if url in files and files[url] != size:
                raise ValueError("conflicting duplicate NAR file size")
            files[url] = size
    missing = sorted(p for p, e in paths.items()
                     if not e["upstream"] and e["cachix"] is None)
    return {"schema": 1, "upstream": UPSTREAM, "cachix": CACHIX,
            "closure_path_count": len(paths),
            "closure_nar_bytes": sum(e["nar_size"] for e in paths.values()),
            "upstream_missing_path_count": sum(not e["upstream"] for e in paths.values()),
            "cachix_unique_file_count": len(files),
            "cachix_compressed_bytes": sum(files.values()),
            "unpublished_paths": missing,
            "publication_complete": not missing,
            "paths": [paths[p] for p in sorted(paths)]}


def self_test():
    path = "/nix/store/" + "0" * 32 + "-system"
    other = "/nix/store/" + "1" * 32 + "-uboot"
    text = f"StorePath: {path}\nURL: nar/shared.nar.zst\nFileSize: 7\nNarSize: 20\n"
    cached = parse_narinfo(text, path)
    entry = {"path": path, "nar_size": 20, "upstream": False, "cachix": cached}
    report = summarize([entry, entry, {**entry, "path": other}])
    assert report["closure_nar_bytes"] == 40
    assert report["cachix_compressed_bytes"] == 7
    assert report["publication_complete"]
    missing = summarize([{**entry, "cachix": None}])
    assert missing["cachix_compressed_bytes"] == 0
    assert missing["unpublished_paths"] == [path]
    assert closure_entries([{ "path": path, "narSize": 20 }] * 2) == {path: 20}
    for action in (
        lambda: parse_narinfo(text + "FileSize: 8\n", path),
        lambda: parse_narinfo(text.replace("nar/shared.nar.zst", "https://evil/nar"), path),
        lambda: parse_narinfo(text.replace("nar/shared.nar.zst", "nar/../bad"), path),
        lambda: parse_narinfo(text, other),
        lambda: summarize([{**entry, "cachix": False}]),
        lambda: summarize([{**entry, "upstream": None}]),
        lambda: closure_entries([{ "path": path, "narSize": 20 },
                                { "path": path, "narSize": 21 }]),
    ):
        try:
            action()
        except (ValueError, TypeError):
            pass
        else:
            raise AssertionError("invalid metadata accepted")
    print("nix-cache-report self-test passed")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("roots", nargs="?", type=Path, help="system/U-Boot roots, one store path per line")
    parser.add_argument("--output", type=Path)
    parser.add_argument("--merge", nargs="+", type=Path, help="union reports from both targets or versions")
    parser.add_argument("--require-published", action="store_true")
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    if args.self_test:
        self_test()
        return
    if bool(args.roots) == bool(args.merge):
        parser.error("provide either roots or --merge")
    previous_cached = set()
    if args.merge:
        entries = []
        for filename in args.merge:
            report = json.loads(filename.read_text())
            if report["upstream"] != UPSTREAM or report["cachix"] != CACHIX:
                raise ValueError("cannot merge different caches")
            entries.extend(report["paths"])
        previous_cached = {e["path"] for e in entries if e["cachix"] is not None}
        closure = closure_entries([{"path": e["path"], "narSize": e["nar_size"]}
                                   for e in entries])
    else:
        roots = list(dict.fromkeys(store_path(p) for p in args.roots.read_text().splitlines()))
        if not roots:
            raise ValueError("empty cache roots file")
        raw = subprocess.check_output([
            "nix", "--extra-experimental-features", "nix-command flakes",
            "path-info", "--recursive", "--json", *roots], text=True)
        closure = closure_entries(json.loads(raw))

    def probe(item):
        path, size = item
        upstream = fetch_narinfo(UPSTREAM, path)
        cached = fetch_narinfo(CACHIX, path)
        for metadata in (upstream, cached):
            if metadata is not None and metadata["nar_size"] != size:
                raise ValueError("remote and local NAR sizes differ")
        return {"path": path, "nar_size": size,
                "upstream": upstream is not None, "cachix": cached}

    with ThreadPoolExecutor(max_workers=8) as pool:
        entries = list(pool.map(probe, closure.items()))
    report = summarize(entries)
    report["previously_cached_missing_paths"] = sorted(
        e["path"] for e in entries if e["path"] in previous_cached and e["cachix"] is None)
    content = json.dumps(report, indent=2) + "\n"
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(content)
    else:
        print(content, end="")
    if args.require_published and not report["publication_complete"]:
        parser.exit(1, "runtime closure missing from both public caches; see unpublished_paths\n")


if __name__ == "__main__":
    main()
