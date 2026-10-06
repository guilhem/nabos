#!/usr/bin/env bash
# Validate immutable copies of actual outputs; no writable mount is required.
set -euo pipefail
target=${1:?Target required}
disk=${2:?SD image required}
rootfs=${3:?Rootfs required}
boot=${4:?Boot partition required}
bundle=${5:?Bundle required}
certificate=${6:?Public certificate required}
[[ $target == zero-armv6 || $target == zero2-arm64 ]] || exit 2
before=$(sha256sum "$disk" "$rootfs" "$boot")
python3 - "$disk" "$rootfs" "$boot" <<'PY'
import hashlib, os, struct, sys
disk, rootfs, boot = sys.argv[1:]
with open(disk, 'rb') as f:
    mbr = f.read(512)
    assert mbr[510:] == b'\x55\xaa', 'invalid MBR signature'
    entries = [struct.unpack_from('<B3sB3sII', mbr, 446 + 16 * i) for i in range(4)]
    mib = 1024 * 1024
    for entry, start, size, kind in zip(entries, [4, 516, 6660, 12804], [256, 6144, 6144, 1024], [12, 131, 131, 131]):
        assert (entry[4] * 512, entry[5] * 512, entry[2]) == (start * mib, size * mib, kind), entry
    for start, path in [(4 * mib, boot), (516 * mib, rootfs)]:
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
rauc info --keyring="$certificate" --output-format=json "$bundle" |
  jq -e --arg compatible "nabos-$target" \
    --arg rootfs "$(sha256sum "$rootfs" | cut -d' ' -f1)" \
    --arg boot "$(sha256sum "$boot" | cut -d' ' -f1)" \
    '.compatible == $compatible and (.images | length) == 2 and
     .images[0].rootfs.checksum == $rootfs and .images[1].bootloader.checksum == $boot'
[[ "$before" == "$(sha256sum "$disk" "$rootfs" "$boot")" ]]
echo 'NixOS image layout, boot payload, ownership and RAUC signature verified; originals unchanged'
