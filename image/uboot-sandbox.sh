#!/bin/bash
# Build host-native verification tools, with an optional caller-managed ccache.
set -euo pipefail
[[ $# == 2 ]] || { echo 'Usage: image/uboot-sandbox.sh SOURCE OUTPUT' >&2; exit 2; }
source=$(realpath "$1")
output=$(realpath -m "$2")
repo=$(cd "$(dirname "$0")/.." && pwd)
cc=${CC:-gcc}
mkdir -p "$output"

build=(make -C "$source" O="$output" CROSS_COMPILE= CC="$cc" HOSTCC=gcc
  KCFLAGS="-fdebug-prefix-map=$(dirname "$source")=.")
"${build[@]}" sandbox_defconfig
"$source/scripts/config" --file "$output/.config" \
  -d SANDBOX_SDL -d TOOLS_MKEFICAPSULE -d UNIT_TEST -d EFI_CAPSULE_AUTHENTICATE \
  -d EFI_CAPSULE_ON_DISK -d CMD_UPL -d UPL
"${build[@]}" olddefconfig
if [[ $cc == *ccache* ]]; then
  CCACHE_NAMESPACE=$({
    sha256sum "$repo/image/sources.lock.json" "$repo/image/uboot-sandbox.sh" \
      "$output/.config" | cut -d' ' -f1
    uname -m
  } | sha256sum | cut -d' ' -f1)
  export CCACHE_NAMESPACE
fi
"${build[@]}" -j"$(nproc)" CONFIG_PYLIBFDT= u-boot tools
for binary in u-boot scripts/dtc/dtc tools/mkimage tools/mkenvimage; do
  test -x "$output/$binary"
done
(cd "$output" && sha256sum u-boot scripts/dtc/dtc tools/mkimage tools/mkenvimage > SHA256SUMS)
