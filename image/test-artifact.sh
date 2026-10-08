#!/usr/bin/env bash
# Revalidate the exact downloaded outputs; testing never modifies the artifacts.
set -euo pipefail
[[ $# == 2 ]] || { echo 'Usage: bash image/test-artifact.sh TARGET ARTIFACTS' >&2; exit 2; }
target=$1
[[ $target == zero-armv6 || $target == zero2-arm64 ]] || exit 2
repo=$(cd "$(dirname "$0")/.." && pwd)
artifacts=$(realpath "$2")
mkdir -p "$repo/build/nix-tmp"
export TMPDIR="$repo/build/nix-tmp"
if [[ ${NABOS_NIX_SHELL:-} != 1 ]]; then
  exec nix --extra-experimental-features 'nix-command flakes' develop "$repo" \
    --command env NABOS_NIX_SHELL=1 bash "$0" "$target" "$artifacts"
fi
: "${EXPECTED_VERSION:?Expected version required}" "${EXPECTED_DEVELOPMENT:?Expected development flag required}"
report="$repo/build/nix-tmp/test-$target.json"
rm -f "$report"
SECONDS=0
(cd "$artifacts" && sha256sum --check --strict "SHA256SUMS-$target")
jq -e --arg target "$target" --arg revision "$(git -C "$repo" rev-parse HEAD)" \
  --arg version "$EXPECTED_VERSION" --argjson development "$EXPECTED_DEVELOPMENT" \
  --arg nixpkgs "$(jq -r '.nodes[.nodes.root.inputs.nixpkgs].locked.rev' "$repo/flake.lock")" \
  '.target == $target and .source_revision == $revision and .source_dirty == false and
   .version == $version and .development == $development and .nixpkgs_revision == $nixpkgs' \
  "$artifacts/build-$target.json" >/dev/null
cmp "$repo/flake.lock" "$artifacts/flake-$target.lock"
cmp "$repo/image/boot/boot.cmd" "$artifacts/boot-$target.cmd"
certificate=$artifacts/ca-$target.cert.pem
if [[ $EXPECTED_DEVELOPMENT == false ]]; then
  : "${EXPECTED_RAUC_CERT:?Production validation requires the expected public trust anchor}"
  cmp "$EXPECTED_RAUC_CERT" "$certificate"
fi
test "$(stat -c %s "$artifacts/nabos-$target.raucb")" -le 2147483648
work=$(mktemp -d "$TMPDIR/artifact-$target.XXXXXX")
trap 'rm -rf -- "$work"' EXIT
xz --decompress --stdout "$artifacts/nabos-$target.img.xz" |
  dd of="$work/sdcard.img" bs=4M conv=sparse status=none
dd if="$work/sdcard.img" of="$work/rootfs.ext4" bs=1M skip=516 count=6144 conv=sparse status=none
dd if="$work/sdcard.img" of="$work/boot.vfat" bs=1M skip=4 count=256 conv=sparse status=none
dd if="$work/sdcard.img" of="$work/data.ext4" bs=1M skip=12804 count=1024 conv=sparse status=none
dd if="$work/sdcard.img" of="$work/uboot.env" bs=64K skip=16 count=1 status=none
chmod a-w "$work/"{sdcard.img,rootfs.ext4,boot.vfat,data.ext4,uboot.env}
bash "$repo/image/test.sh" "$target" "$work/sdcard.img" "$work/rootfs.ext4" \
  "$work/boot.vfat" "$artifacts/nabos-$target.raucb" "$certificate"
mkdir "$work/boot" "$work/overlays"
mcopy -i "$work/boot.vfat" -s '::*' "$work/boot/"
debugfs -R "dump /boot/overlays/tagtagtag-sound.dtbo $work/overlays/tagtagtag-sound.dtbo" "$work/rootfs.ext4"
sandbox=$(nix --extra-experimental-features 'nix-command flakes' build "$repo#uboot-sandbox" --no-link --print-out-paths)
(cd "$sandbox" && sha256sum --check --strict SHA256SUMS)
export NABOS_UBOOT_SANDBOX="$sandbox" NABOS_IMAGE_TARGET="$target" \
  NABOS_IMAGE_BOOT="$work/boot" NABOS_IMAGE_ENV="$work/uboot.env" \
  NABOS_IMAGE_DISK="$work/sdcard.img" NABOS_IMAGE_OVERLAYS="$work/overlays" \
  NABOS_VENDOR_DTBS="$work/boot"
(cd "$repo/services" && go test -count=1 ./tests/image)
printf '%s\n' "$sandbox" > "$repo/build/nix-tmp/uboot-cache-root-$target"
bash "$repo/image/test-runtime.sh" "$target" "$work/rootfs.ext4" "$work/data.ext4"
jq -n --arg target "$target" --arg version "$EXPECTED_VERSION" \
  --arg revision "$(git -C "$repo" rev-parse HEAD)" \
  --argjson seconds "$SECONDS" \
  '{target:$target,version:$version,source_revision:$revision,durations_seconds:{tests:$seconds}}' > "$report"
echo "Artifact test measurements: $report"
