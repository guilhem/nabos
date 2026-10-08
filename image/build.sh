#!/usr/bin/env bash
# NixOS image builder: private signing material never enters a Nix derivation.
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
target=${1:?Usage: image/build.sh TARGET VERSION [--development] [--defer-tests]}
version=${2:?Version required}
shift 2
[[ $target == zero-armv6 || $target == zero2-arm64 ]] || exit 2
[[ $version =~ ^[a-zA-Z0-9][a-zA-Z0-9.+_-]{0,63}$ ]] || exit 2
development=false
defer_tests=false
xz_level=3
zstd_level=6
for option in "$@"; do
  case $option in
    --development) development=true ;;
    --defer-tests) defer_tests=true ;;
    *) echo "Unknown option: $option" >&2; exit 2 ;;
  esac
done
mkdir -p "$repo/build/nix-tmp"
export TMPDIR="$repo/build/nix-tmp"
if [[ ${NABOS_NIX_SHELL:-} != 1 ]]; then
  exec nix --extra-experimental-features 'nix-command flakes' develop "$repo" \
    --command env NABOS_NIX_SHELL=1 bash "$0" "$target" "$version" "$@"
fi
export NABOS_FLAKE="git+file://$repo" NABOS_TARGET="$target" NABOS_VERSION="$version"
out="$repo/dist/$target"
mkdir -p "$out" "$repo/build/nixos"
work=$(mktemp -d "$repo/build/nixos/$target.XXXXXX")
cleanup() { rm -rf "$work/signing"; }
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
payload_expr='let f = builtins.getFlake (builtins.getEnv "NABOS_FLAKE"); in
  (f.lib.mkImage { buildSystem = builtins.currentSystem;
    target = builtins.getEnv "NABOS_TARGET";
    version = builtins.getEnv "NABOS_VERSION"; }).payload'
# Keep selected roots available even when building or packaging the payload fails.
rm -f "$out/cache-roots-$target"
nix --extra-experimental-features 'nix-command flakes' eval --impure --raw \
  --apply 'roots: builtins.concatStringsSep "\n" (map builtins.toString roots) + "\n"' \
  --expr "($payload_expr).cacheRoots" > "$work/cache-roots"
install -m644 "$work/cache-roots" "$out/cache-roots-$target"
SECONDS=0
payload=$(nix --extra-experimental-features 'nix-command flakes' build --impure \
  --no-link --print-out-paths \
  --option extra-substituters https://nabos.cachix.org \
  --option extra-trusted-public-keys 'nabos.cachix.org-1:jLoce+DvPr6ejhFfvmEKXznQLVKxZ6zCP5N7dirR/JQ=' \
  --expr "$payload_expr")
seconds=$SECONDS
SECONDS=0
mkdir -p "$work/images" "$work/data/rauc" "$work/bundle" "$work/empty"
if $development; then
  mkdir -m700 "$work/signing"
  (umask 077; openssl req -x509 -newkey rsa:3072 -nodes -days 7 \
    -subj '/CN=NabOS development only/' -keyout "$work/signing/key.pem" \
    -out "$work/signing/cert.pem" 2>/dev/null)
  signing_key="$work/signing/key.pem"
  signing_cert="$work/signing/cert.pem"
else
  signing_key=${RAUC_KEY:?RAUC_KEY must point to a PEM signing key}
  signing_cert=${RAUC_CERT:?RAUC_CERT must point to its trusted public certificate}
fi
install -m600 "$signing_cert" "$work/data/rauc/ca.cert.pem"
chmod 700 "$work/data/rauc"
cp --reflink=auto --sparse=always "$payload"/{rootfs.ext4,boot.vfat,uboot.env} "$work/images/"
truncate -s 1G "$work/images/data.ext4"
# Expanded by the child shell, with paths passed as positional arguments.
# shellcheck disable=SC2016
fakeroot bash -c 'chown -R 0:0 "$1"; exec mkfs.ext4 -q -F -L nabos-data -d "$1" "$2"' \
  -- "$work/data" "$work/images/data.ext4"
GENIMAGE_SHELL="$(command -v bash)" genimage --config "$repo/image/genimage.cfg" --rootpath "$work/empty" \
  --inputpath "$work/images" --outputpath "$work/images" --tmppath "$work/genimage-tmp"
sd_seconds=$SECONDS
SECONDS=0
ln "$work/images/rootfs.ext4" "$work/images/boot.vfat" "$work/bundle/"
compatible=$(jq -er --arg target "$target" '.targets[$target].compatible' "$repo/image/sources.lock.json")
sed -e "s/@COMPATIBLE@/$compatible/g" -e "s/@VERSION@/$version/g" \
  "$repo/image/manifest.raucm.in" > "$work/bundle/manifest.raucm"
rm -f "$out/nabos-$target.raucb"
rauc bundle --mksquashfs-args="-comp zstd -Xcompression-level $zstd_level" \
  --cert="$signing_cert" --key="$signing_key" "$work/bundle" "$out/nabos-$target.raucb"
test "$(stat -c %s "$out/nabos-$target.raucb")" -le 2147483648
install -m644 "$signing_cert" "$out/ca-$target.cert.pem"
rauc_seconds=$SECONDS
chmod a-w "$work/images/"{sdcard.img,rootfs.ext4,boot.vfat}
test_seconds=null
if ! $defer_tests; then
  SECONDS=0
  EXPECTED_VERSION="$version" bash "$repo/image/test.sh" "$target" "$work/images/sdcard.img" \
    "$work/images/rootfs.ext4" "$work/images/boot.vfat" "$out/nabos-$target.raucb" "$out/ca-$target.cert.pem"
  test_seconds=$SECONDS
fi
SECONDS=0
xz -T0 "-$xz_level" --stdout "$work/images/sdcard.img" > "$out/nabos-$target.img.xz"
xz_seconds=$SECONDS
cp "$repo/flake.lock" "$out/flake-$target.lock"
install -m644 "$payload/boot.cmd" "$out/boot-$target.cmd"
dirty=false
[[ -z $(git -C "$repo" status --porcelain --untracked-files=no) ]] || dirty=true
jq --arg revision "$(git -C "$repo" rev-parse HEAD)" --argjson seconds "$seconds" \
  --argjson sd_seconds "$sd_seconds" --argjson rauc_seconds "$rauc_seconds" \
  --argjson xz_seconds "$xz_seconds" --argjson test_seconds "$test_seconds" \
  --argjson xz_level "$xz_level" --argjson zstd_level "$zstd_level" \
  --argjson development "$development" --argjson dirty "$dirty" --arg nixpkgs "$(jq -r '.nodes[.nodes.root.inputs.nixpkgs].locked.rev' "$repo/flake.lock")" \
  '. + {source_revision:$revision,nixpkgs_revision:$nixpkgs,build_seconds:$seconds,development:$development,source_dirty:$dirty,
    durations_seconds:{nix_build:$seconds,sd_assembly:$sd_seconds,rauc_bundle:$rauc_seconds,xz:$xz_seconds,tests:$test_seconds},
    compression:{xz:$xz_level,rauc_zstd:$zstd_level}}' \
  "$payload/build.json" > "$out/build-$target.json"
(cd "$out"; sha256sum "nabos-$target.img.xz" "nabos-$target.raucb" "ca-$target.cert.pem" \
  "build-$target.json" "flake-$target.lock" "boot-$target.cmd" "cache-roots-$target" > "SHA256SUMS-$target")
echo "Built $version for $target in $out; workspace: $work"
