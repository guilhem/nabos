#!/usr/bin/env bash
# Verify the assembled NixOS image and its signed update without mounting it.
set -euo pipefail
[[ $# == 7 ]] || { echo 'Usage: bash image/test.sh TARGET SD_IMAGE ROOTFS BOOT DATA BUNDLE CERTIFICATE' >&2; exit 2; }
target=$1 disk=$2 rootfs=$3 boot=$4 data=$5 bundle=$6 certificate=$7
repo=$(cd "$(dirname "$0")/.." && pwd)
[[ $target == zero-armv6 || $target == zero2-arm64 ]] || exit 2
before=$(sha256sum "$disk" "$rootfs" "$boot" "$data")
readarray -t hashes <<< "$before"
rootfs_hash=${hashes[1]%% *} boot_hash=${hashes[2]%% *}
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
python3 - "$disk" "$rootfs" "$boot" "$data" <<'PY'
import os, struct, sys
disk, rootfs, boot, data = sys.argv[1:]
mib = 1024 * 1024
assert os.path.getsize(rootfs) == 6144 * mib
assert os.path.getsize(boot) == 256 * mib
assert os.path.getsize(data) == 1024 * mib
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
    for start, path in [(4 * mib, boot), (260 * mib, boot), (516 * mib, rootfs), (12804 * mib, data)]:
        f.seek(start)
        with open(path, 'rb') as component:
            while chunk := component.read(4 * mib):
                assert f.read(len(chunk)) == chunk, f'SD content differs from {path}'
PY
e2fsck -fn "$rootfs"
debugfs -R 'cat /boot/init' "$rootfs" 2>/dev/null | grep -Eq '^nabos_init=/nix/store/.+/init$'
debugfs -R 'cat /boot/boot.scr' "$rootfs" 2>/dev/null | tail -c +73 | cmp - "$repo/image/boot/slot.cmd"
debugfs -R 'stat /boot/initrd' "$rootfs" 2>/dev/null | grep -Eq 'Size: [1-9][0-9]+'
debugfs -R "dump /boot/initrd $work/initrd" "$rootfs" 2>/dev/null
python3 "$repo/image/test-initrd.py" "$work/initrd"
debugfs -R 'stat /nix/store' "$rootfs" 2>/dev/null | grep -Eq 'User:[[:space:]]+0[[:space:]]+Group:[[:space:]]+0'
mdir -i "$boot" ::/boot.scr >/dev/null
mdir -i "$boot" ::/recovery/kernel >/dev/null
mdir -i "$boot" ::/recovery/initrd >/dev/null
mcopy -i "$boot" ::/recovery/initrd "$work/recovery-initrd"
python3 "$repo/image/test-initrd.py" --recovery "$work/recovery-initrd"
e2fsck -fn "$data"
debugfs -R 'cat /usr/share/nabos/ca.cert.pem' "$rootfs" 2>/dev/null | cmp - "$certificate"
stat=$(debugfs -R 'stat /usr/share/nabos/ca.cert.pem' "$rootfs" 2>/dev/null)
grep -Eq 'Type: regular' <<< "$stat"
grep -Eq 'User:[[:space:]]+0[[:space:]]+Group:[[:space:]]+0' <<< "$stat"
grep -Eq 'Mode:[[:space:]]+0644' <<< "$stat"
if debugfs -R 'stat /rauc' "$data" 2>/dev/null | grep -q 'Inode:'; then
  echo 'Fresh data partition contains RAUC state' >&2; exit 1
fi
compatible="nabos-native-rauc-$target"
rauc info --keyring="$certificate" --output-format=json "$bundle" |
  jq -e --arg compatible "$compatible" --arg version "${EXPECTED_VERSION:-}" \
    --arg rootfs "${rootfs_hash#\\}" \
    --arg boot "${boot_hash#\\}" \
    '.compatible == $compatible and ($version == "" or .version == $version) and
     (.images | length) == 2 and .images[0].rootfs.checksum == $rootfs and
     .images[1].bootloader.checksum == $boot'
unsquashfs -cat "$bundle" rauc-hook.sh | cmp - "$repo/image/rauc-hook.sh"
unsquashfs -cat "$bundle" layout.sh | cmp - "$repo/image/layout.sh"
unsquashfs -cat "$bundle" manifest.raucm > "$work/manifest.raucm"
grep -qx 'filename=rauc-hook.sh' "$work/manifest.raucm"
grep -Eq '^hooks=install-check;?$' "$work/manifest.raucm"
[[ "$before" == "$(sha256sum "$disk" "$rootfs" "$boot" "$data")" ]]
echo 'Verified SD partitions, redundant boot copies, NixOS payload and signed RAUC bundle; originals unchanged'
