#!/usr/bin/env python3
"""Build the locked driver patches and overlays; optionally build kernel modules."""

import argparse
import json
import re
import subprocess
import tempfile
from pathlib import Path

from fetch import digest, unpack


IMAGE = Path(__file__).resolve().parent
DRIVERS = {
    "ears": ("tagtagtag-ears", ("tagtagtag-ears",)),
    "sound": ("tagtagtag-sound", ("snd-soc-wm8960", "snd-soc-max9759", "snd-soc-volume-gpio")),
    "cr14": ("cr14", ("cr14",)),
    "nfc": ("st25r391x", ("st25r391x",)),
}


def run(*args, **kwargs):
    subprocess.run(args, check=True, **kwargs)


def kernel_build(value):
    path = Path(value)
    if not path.is_dir():
        path = Path("/lib/modules") / value / "build"
    header = path / "include/generated/utsrelease.h"
    match = re.search(r'#define UTS_RELEASE "([^"]+)"', header.read_text())
    if not match:
        raise ValueError(f"No UTS_RELEASE in {header}")
    release = match.group(1)
    if path == Path("/lib/modules") / value / "build" and release != value:
        raise ValueError(f"Kernel release mismatch: {release} != {value}")
    return path, release


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--archives", type=Path, required=True,
                        help="directory containing the locked ears/sound/cr14/nfc tar.gz files")
    parser.add_argument("--kernel", help="KERNELRELEASE or its headers build directory")
    args = parser.parse_args()
    lock = json.loads((IMAGE / "sources.lock.json").read_text())["sources"]
    kernel, release = kernel_build(args.kernel) if args.kernel else (None, None)

    with tempfile.TemporaryDirectory(prefix="nabos-drivers-") as temporary:
        work = Path(temporary)
        for name, (overlay, modules) in DRIVERS.items():
            archive = args.archives / f"{name}.tar.gz"
            if digest(archive) != lock[name]["sha256"]:
                raise ValueError(f"Locked archive hash mismatch: {archive}")
            source = unpack(archive, work / name)
            run("patch", "--batch", "--fuzz=0", "-d", str(source), "-p1",
                "-i", str(IMAGE / "patches" / f"{name}.patch"))

            run("make", "-C", str(source), f"{overlay}.dtbo")
            dtbo = source / f"{overlay}.dtbo"
            decoded = subprocess.check_output(
                ("dtc", "-I", "dtb", "-O", "dts", str(dtbo)),
                text=True, stderr=subprocess.DEVNULL)
            if "__fixups__ {" not in decoded:
                raise ValueError(f"No external fixups in {dtbo}")

            if kernel:
                run("make", "-C", str(kernel), f"M={source}", "modules")
                for module in modules:
                    ko = source / f"{module}.ko"
                    vermagic = subprocess.check_output(("modinfo", "-F", "vermagic", str(ko)), text=True)
                    if vermagic.split()[0] != release:
                        raise ValueError(f"Wrong vermagic in {ko}: {vermagic.strip()}")
            print(f"{name}: patch + DTBO" + (" + modules" if kernel else ""), flush=True)


if __name__ == "__main__":
    main()
