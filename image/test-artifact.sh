#!/bin/bash
# Verify the exact image artifact produced by a previous CI job.
set -euo pipefail
[[ $# == 3 ]] || { echo 'Usage: image/test-artifact.sh TARGET ARTIFACTS COMPONENTS' >&2; exit 2; }
target=$1
[[ $target == zero-armv6 || $target == zero2-arm64 ]] || exit 2
repo=$(cd "$(dirname "$0")/.." && pwd)
artifacts=$(realpath "$2")
components=$(realpath "$3")
GO=${GO:-go}
revision=$(git -C "$repo" rev-parse HEAD)
(cd "$artifacts" && sha256sum --check --strict "SHA256SUMS-$target")
jq -e --arg target "$target" --arg revision "$revision" \
  '.target == $target and .source_revision == $revision and .source_dirty == false' \
  "$artifacts/build-$target.json" >/dev/null
mkdir -p "$repo/build/iot"
nab_image=$repo/build/iot/nab-image
(cd "$repo/services" && GOTOOLCHAIN=local CGO_ENABLED=0 "$GO" build -o "$nab_image" ./cmd/nab-image)
work=$(mktemp -d "$repo/build/iot/artifact-$target.XXXXXX")
trap 'rm -rf -- "$work"' EXIT
payload=$work/payload
mkdir -p "$payload/inputs" "$payload/src"
"$nab_image" extract "$artifacts/build-inputs-$target.tar.xz" "$payload/inputs"
cmp "$repo/image/sources.lock.json" "$payload/inputs/sources.lock.json"
cmp "$repo/services/go.sum" "$payload/inputs/go.sum"
"$nab_image" unpack "$payload/inputs/sources.lock.json" "$payload/inputs" "$payload/src"
"$nab_image" extract "$components/uboot-$target.tar" "$work/uboot"
printf '%s\n' "$target" "$revision" | cmp - "$work/uboot/build-info"
"$nab_image" extract "$components/uboot-sandbox-$target.tar" "$work/sandbox"
# Keep zero-filled partitions sparse when expanding the 13.5 GiB SD image.
xz --decompress --stdout "$artifacts/nabos-$target.img.xz" |
  dd of="$work/sdcard.img" bs=4M conv=sparse status=none
chmod a-w "$work/sdcard.img"
NABOS_UBOOT_SANDBOX="$work/sandbox" bash "$repo/image/test.sh" \
  "$target" "$work/sdcard.img" "$payload" "$work/uboot/u-boot.bin"
