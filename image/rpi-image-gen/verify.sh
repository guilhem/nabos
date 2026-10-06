#!/bin/bash
# Read the generated archive; do not unpack its filesystem or change ownership.
set -euo pipefail
trap 'echo "Prototype verification failed at line $LINENO" >&2' ERR
archive=${1:?rootfs archive required}
out=${2:?report directory required}
mkdir -p "$out"
test -s "$archive"
# Inspect the generated archive without unpacking it or changing its ownership.
tar -xOf "$archive" ./var/lib/dpkg/status > "$out/status"
dpkg-query --admindir="$out" -W -f='${Package}\t${Version}\t${Architecture}\t${Status}\n' > "$out/packages.tsv"
awk -F '\t' '$4 != "install ok installed" || ($3 != "arm64" && $3 != "all") { print "Invalid package: " $0; bad=1 } END { exit bad }' "$out/packages.tsv"
for package in linux-image-rpi-v8 raspi-firmware systemd network-manager pipewire wireplumber rauc; do
  awk -F '\t' -v package="$package" '$1 == package { found=1 } END { exit !found }' "$out/packages.tsv"
done
if awk -F '\t' '$1 ~ /^((gcc|g\+\+|cpp)(-[0-9]+)?|make|build-essential|dpkg-dev|pkg-config|pkgconf|bison|flex|bc|python3-setuptools|python3-pyelftools|linux-headers-.*)$|(^|-)dev($|-)|-dbg$/ { found=1 } END { exit !found }' "$out/packages.tsv"; then
  echo 'Development packages leaked into the runtime base' >&2
  exit 1
fi
tar -tf "$archive" > "$out/archive-files.txt"
python3 - "$archive" <<'PY_PAYLOAD'
import gzip
import hashlib
import lzma
from pathlib import Path
import re
import struct
import subprocess
import sys
import tarfile
import tempfile

def require(condition, message):
    if not condition:
        raise ValueError(message)


with tarfile.open(sys.argv[1]) as archive, tempfile.TemporaryDirectory() as temporary:
    kernels = [entry for entry in archive if re.fullmatch(r"\./boot/vmlinuz-.+-rpi-v8", entry.name)]
    require(len(kernels) == 1, "Expected one Raspberry Pi v8 kernel image")
    kernel = kernels[0].name.removeprefix("./boot/vmlinuz-")
    modules = [entry for entry in archive if re.fullmatch(
        rf"\./usr/lib/modules/{re.escape(kernel)}/kernel/.+\.ko(?:\.xz)?", entry.name)]
    require(modules, "Missing kernel module payload")
    dtb = f"./usr/lib/modules/{kernel}/dtb/broadcom/bcm2710-rpi-zero-2-w.dtb"
    hashes = archive.extractfile(f"./var/lib/dpkg/info/linux-image-{kernel}.md5sums").read().decode()
    hashes = dict(line.split(maxsplit=1)[::-1] for line in hashes.splitlines())
    payloads = {}
    for name in (kernels[0].name, modules[0].name, dtb):
        entry = archive.getmember(name)
        require(entry.isfile() and entry.size > 0, f"Not a regular nonempty payload: {name}")
        data = archive.extractfile(entry).read()
        require(hashlib.md5(data).hexdigest() == hashes[name.removeprefix("./")], f"Package digest mismatch: {name}")
        payloads[name] = data
    image = payloads[kernels[0].name]
    if image.startswith(b"\x1f\x8b"):
        image = gzip.decompress(image)
    require(len(image) >= 64 and image[56:60] == b"ARM\x64", "Invalid ARM64 kernel image")
    module = payloads[modules[0].name]
    if modules[0].name.endswith(".xz"):
        module = lzma.decompress(module)
    require(module[:6] == b"\x7fELF\x02\x01" and struct.unpack_from("<HH", module, 16) == (1, 183), "Invalid ARM64 module")
    module_path = Path(temporary, "module.ko")
    module_path.write_bytes(module)
    vermagic = subprocess.check_output(["modinfo", "-F", "vermagic", str(module_path)], text=True)
    require(vermagic.startswith(kernel + " "), "Kernel/module release mismatch")
    dtb_path = Path(temporary, "device.dtb")
    dtb_path.write_bytes(payloads[dtb])
    subprocess.run(["dtc", "-q", "-I", "dtb", "-O", "dts", "-o", "/dev/null", str(dtb_path)], check=True)
    compatible = subprocess.check_output(["fdtget", str(dtb_path), "/", "compatible"], text=True)
    require("raspberrypi,model-zero-2-w" in compatible.split(), "Unexpected DTB board")
    print(f"Verified kernel, module and Zero 2 W DTB payloads: {kernel}")
PY_PAYLOAD
tar -xOf "$archive" ./usr/share/nabos-prototype/scope
[[ -z $(tar -xOf "$archive" ./etc/machine-id) ]]
if grep -Eq '^\./(etc/ssh/ssh_host_|var/lib/systemd/random-seed(/|$))' "$out/archive-files.txt"; then
  echo 'Shared SSH host keys or systemd random seed leaked into the base' >&2
  exit 1
fi
