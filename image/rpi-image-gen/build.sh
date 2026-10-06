#!/bin/bash
# Standalone package-base experiment; production image/build.sh is unchanged.
set -euo pipefail
prototype=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$prototype/../.." && pwd)
revision=3e1041129c4eb87b8784c2875caa169ed45dbfce
upstream=$repo/build/rpi-image-gen/upstream
work=$repo/build/rpi-image-gen/work
out=$repo/dist/rpi-image-gen
mode=${1:-build}
[[ $mode == check || $mode == build ]] || { echo 'Usage: build.sh [check|build]' >&2; exit 2; }
mkdir -p "$repo/build/rpi-image-gen/tmp" "$work" "$out"
export TMPDIR=$repo/build/rpi-image-gen/tmp
if [[ ! -d $upstream/.git ]]; then
  git init "$upstream"
  git -C "$upstream" remote add origin https://github.com/raspberrypi/rpi-image-gen.git
  git -C "$upstream" fetch --depth 1 origin "$revision"
  git -C "$upstream" checkout --detach FETCH_HEAD
fi
[[ $(git -C "$upstream" rev-parse HEAD) == "$revision" ]] || { echo 'Unexpected rpi-image-gen revision' >&2; exit 1; }
git -C "$upstream" diff --exit-code HEAD
"$upstream/rpi-image-gen" metadata --lint "$prototype/layer/nabos-base-arm64.yaml"
"$upstream/rpi-image-gen" config "$prototype/config/base-arm64.yaml"
[[ $mode == build ]] || exit 0
export XZ_OPT='-T0 -6'
/usr/bin/time -v -o "$out/build-resources.txt"   "$upstream/rpi-image-gen" build -f -S "$prototype" -c base-arm64.yaml -B "$work"
archive=$work/nabos-base-arm64/rootfs.tar.xz
test -s "$archive"
# Inspect the generated archive without unpacking it or changing its ownership.
tar -xOf "$archive" ./var/lib/dpkg/status > "$out/status"
dpkg-query --admindir="$out" -W -f='${Package}\t${Version}\t${Architecture}\t${Status}\n' > "$out/packages.tsv"
awk -F '\t' '$4 != "install ok installed" || ($3 != "arm64" && $3 != "all") { print "Invalid package: " $0; bad=1 } END { exit bad }' "$out/packages.tsv"
for package in linux-image-rpi-v8 raspi-firmware systemd network-manager pipewire wireplumber rauc; do
  awk -F '\t' -v package="$package" '$1 == package { found=1 } END { exit !found }' "$out/packages.tsv"
done
if awk -F '\t' '$1 ~ /^((gcc|g\+\+|cpp)(-[0-9]+)?|make|build-essential|dpkg-dev|linux-headers-.*)$/ { found=1 } END { exit !found }' "$out/packages.tsv"; then
  echo 'Development packages leaked into the runtime base' >&2
  exit 1
fi
tar -tf "$archive" > "$out/archive-files.txt"
grep -Eq '^\./(usr/)?lib/modules/[^/]+-rpi-v8/' "$out/archive-files.txt"
grep -Eq '^\./(usr/)?lib/linux-image-[^/]+/broadcom/bcm2710-rpi-zero-2-w.dtb$' "$out/archive-files.txt"
tar -xOf "$archive" ./usr/share/nabos-prototype/scope
[[ -z $(tar -xOf "$archive" ./etc/machine-id) ]]
if grep -Eq '^\./etc/ssh/ssh_host_' "$out/archive-files.txt"; then
  echo 'Generated SSH host keys leaked into the base' >&2
  exit 1
fi
cp "$archive" "$out/rootfs.tar.xz"
cp "$work/nabos-base-arm64/config.yaml" "$out/resolved-config.yaml"
printf '{"scope":"base-generation-prototype","target":"zero2-arm64","rpi_image_gen_revision":"%s","nabos_revision":"%s","hardware_validated":false}\n'   "$revision" "$(git -C "$repo" rev-parse HEAD)" > "$out/build-info.json"
(cd "$out" && sha256sum rootfs.tar.xz packages.tsv resolved-config.yaml build-info.json > SHA256SUMS)
echo "Verified ARM64 package base: $out/rootfs.tar.xz"
