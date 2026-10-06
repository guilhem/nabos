#!/usr/bin/env python3
"""Exercise the verifier against payloads from a real generated archive."""
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile

verifier = Path(__file__).with_name("verify.sh")
with tarfile.open(sys.argv[1]) as source:
    kernel_path = next(entry.name for entry in source if entry.name.startswith("./boot/vmlinuz-"))
    kernel = kernel_path.removeprefix("./boot/vmlinuz-")
    module_path = next(entry.name for entry in source if
                       entry.name.startswith(f"./usr/lib/modules/{kernel}/kernel/") and
                       entry.name.endswith(".ko.xz"))
    dtb_path = f"./usr/lib/modules/{kernel}/dtb/broadcom/bcm2710-rpi-zero-2-w.dtb"
    paths = [kernel_path, module_path, dtb_path,
             f"./var/lib/dpkg/info/linux-image-{kernel}.md5sums",
             "./var/lib/dpkg/status", "./etc/machine-id", "./usr/share/nabos-prototype/scope"]
    payloads = {name: source.extractfile(name).read() for name in paths}

with tempfile.TemporaryDirectory() as temporary:
    directory = Path(temporary)
    def check(label, changes=None, missing=None, symlink=None, succeeds=False):
        archive_path = directory / "case.tar"
        entries = payloads | (changes or {})
        with tarfile.open(archive_path, "w") as archive:
            for name, data in entries.items():
                if name == missing:
                    continue
                entry = tarfile.TarInfo(name)
                if name == symlink:
                    entry.type = tarfile.SYMTYPE
                    entry.linkname = kernel_path
                    archive.addfile(entry)
                else:
                    import io
                    entry.size = len(data)
                    archive.addfile(entry, io.BytesIO(data))
        result = subprocess.run(["bash", str(verifier), str(archive_path), str(directory / "reports")],
                                capture_output=True, text=True)
        assert (result.returncode == 0) == succeeds, f"{label}:\n{result.stdout}\n{result.stderr}"
        print(f"PASS: {label}")

    check("real package payloads, including runtime gcc-base", succeeds=True)
    for name in (kernel_path, module_path, dtb_path):
        check(f"missing {name}", missing=name)
        check(f"empty {name}", {name: b""})
        check(f"corrupt {name}", {name: b"invalid payload"})
        check(f"symlink {name}", symlink=name)
    for package in ("pkg-config", "pkgconf", "libssl-dev", "python3-dev", "bison", "flex", "bc",
                    "python3-setuptools", "python3-pyelftools", "linux-headers-rpi-v8", "gcc-14"):
        stanza = f"\nPackage: {package}\nStatus: install ok installed\nArchitecture: arm64\nVersion: 1\n\n"
        check(f"prohibited package {package}",
              {"./var/lib/dpkg/status": payloads["./var/lib/dpkg/status"] + stanza.encode()})
    check("shared systemd random seed", {"./var/lib/systemd/random-seed": b"cloned seed"})
print("Artifact verifier regression checks passed")
