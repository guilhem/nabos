#!/usr/bin/env bash
# Verify the assembled NixOS image and its signed update without mounting it.
set -euo pipefail
[[ $# == 6 ]] || { echo 'Usage: bash image/test.sh TARGET SD_IMAGE ROOTFS BOOT BUNDLE CERTIFICATE' >&2; exit 2; }
target=$1 disk=$2 rootfs=$3 boot=$4 bundle=$5 certificate=$6
[[ $target == zero-armv6 || $target == zero2-arm64 ]] || exit 2
before=$(sha256sum "$disk" "$rootfs" "$boot")
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
python3 - "$disk" "$rootfs" "$boot" <<'PY'
import os, struct, sys
disk, rootfs, boot = sys.argv[1:]
mib = 1024 * 1024
assert os.path.getsize(rootfs) == 6144 * mib
assert os.path.getsize(boot) == 256 * mib
assert os.path.getsize(disk) == 13828 * mib
with open(disk, 'rb') as f:
    mbr = f.read(512)
    assert mbr[510:] == b'\x55\xaa', 'invalid MBR signature'
    entries = [struct.unpack_from('<B3sB3sII', mbr, 446 + 16 * i) for i in range(4)]
    for entry, start, size, kind in zip(entries, [4, 516, 6660, 12804], [256, 6144, 6144, 1024], [12, 131, 131, 131]):
        assert (entry[4] * 512, entry[5] * 512, entry[2]) == (start * mib, size * mib, kind), entry
    f.seek(mib)
    environment = f.read(0x10000)
    f.seek(2 * mib)
    assert f.read(0x10000) == environment, 'redundant environment differs'
    for start, path in [(4 * mib, boot), (260 * mib, boot), (516 * mib, rootfs)]:
        f.seek(start)
        with open(path, 'rb') as component:
            while chunk := component.read(4 * mib):
                assert f.read(len(chunk)) == chunk, f'SD content differs from {path}'
PY
e2fsck -fn "$rootfs"
debugfs -R 'cat /boot/init' "$rootfs" 2>/dev/null | grep -Eq '^nabos_init=/nix/store/.+/init$'
debugfs -R 'stat /boot/initrd' "$rootfs" 2>/dev/null | grep -Eq 'Size: [1-9][0-9]+'
debugfs -R 'stat /nix/store' "$rootfs" 2>/dev/null | grep -Eq 'User:[[:space:]]+0[[:space:]]+Group:[[:space:]]+0'
mdir -i "$boot" ::/boot.scr >/dev/null
dd if="$disk" of="$work/data.ext4" bs=1M skip=12804 count=1024 conv=sparse status=none
e2fsck -fn "$work/data.ext4"
debugfs -R 'cat /rauc/ca.cert.pem' "$work/data.ext4" 2>/dev/null | cmp - "$certificate"
for path in /rauc /rauc/ca.cert.pem; do
  stat=$(debugfs -R "stat $path" "$work/data.ext4" 2>/dev/null)
  grep -Eq 'User:[[:space:]]+0[[:space:]]+Group:[[:space:]]+0' <<< "$stat"
  mode=0600; [[ $path != /rauc ]] || mode=0700
  grep -Eq "Mode:[[:space:]]+$mode" <<< "$stat"
done
rauc info --keyring="$certificate" --output-format=json "$bundle" |
  jq -e --arg compatible "nabos-$target" --arg version "${EXPECTED_VERSION:-}" \
    --arg rootfs "$(sha256sum "$rootfs" | cut -d' ' -f1)" \
    --arg boot "$(sha256sum "$boot" | cut -d' ' -f1)" \
    '.compatible == $compatible and ($version == "" or .version == $version) and
     (.images | length) == 2 and .images[0].rootfs.checksum == $rootfs and
     .images[1].bootloader.checksum == $boot'
[[ "$before" == "$(sha256sum "$disk" "$rootfs" "$boot")" ]]
echo 'Verified SD partitions, redundant boot copies, NixOS payload and signed RAUC bundle; originals unchanged'
