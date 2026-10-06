#!/usr/bin/env bash
# Prototype builder: private signing material never enters a Nix derivation.
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
target=${1:?Usage: image/nix-build.sh TARGET VERSION [--development] [--defer-tests]}
version=${2:?Version required}
shift 2
[[ $target == zero-armv6 || $target == zero2-arm64 ]] || exit 2
[[ $version =~ ^[a-zA-Z0-9][a-zA-Z0-9.+_-]{0,63}$ ]] || exit 2
development=false
defer_tests=false
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
export NABOS_REPO="$repo" NABOS_TARGET="$target" NABOS_VERSION="$version"
out="$repo/dist/nixos/$target"
mkdir -p "$out" "$repo/build/nixos"
work=$(mktemp -d "$repo/build/nixos/$target.XXXXXX")
cleanup() { rm -rf "$work/signing"; }
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
started=$(date +%s)
payload=$(nix --extra-experimental-features 'nix-command flakes' build --impure \
  --no-link --print-out-paths \
  --option extra-substituters https://nabos.cachix.org \
  --option extra-trusted-public-keys 'nabos.cachix.org-1:jLoce+DvPr6ejhFfvmEKXznQLVKxZ6zCP5N7dirR/JQ=' \
  --expr 'let f = builtins.getFlake (builtins.getEnv "NABOS_REPO"); in
    (f.lib.mkPrototype { buildSystem = builtins.currentSystem;
      target = builtins.getEnv "NABOS_TARGET";
      version = builtins.getEnv "NABOS_VERSION"; }).payload')
seconds=$(( $(date +%s) - started ))
mkdir -p "$work/images" "$work/data/rauc" "$work/bundle" "$work/empty"
if $development; then
  mkdir -m700 "$work/signing"
  (umask 077; openssl req -x509 -newkey rsa:3072 -nodes -days 7 \
    -subj '/CN=NabOS NixOS development only/' -keyout "$work/signing/key.pem" \
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
genimage --config "$repo/image/genimage.cfg" --rootpath "$work/empty" \
  --inputpath "$work/images" --outputpath "$work/images" --tmppath "$work/genimage-tmp"
cp --reflink=auto "$work/images/rootfs.ext4" "$work/bundle/"
cp --reflink=auto "$work/images/boot.vfat" "$work/bundle/"
sed -e "s/@COMPATIBLE@/nabos-$target/g" -e "s/@VERSION@/$version/g" \
  "$repo/image/manifest.raucm.in" > "$work/bundle/manifest.raucm"
rm -f "$out/nabos-$target.raucb"
rauc bundle --mksquashfs-args='-comp zstd -Xcompression-level 15' \
  --cert="$signing_cert" --key="$signing_key" "$work/bundle" "$out/nabos-$target.raucb"
install -m644 "$signing_cert" "$out/ca.cert.pem"
chmod a-w "$work/images/"{sdcard.img,rootfs.ext4,boot.vfat}
if ! $defer_tests; then
  bash "$repo/image/nix-test.sh" "$target" "$work/images/sdcard.img" \
    "$work/images/rootfs.ext4" "$work/images/boot.vfat" "$out/nabos-$target.raucb" "$out/ca.cert.pem"
fi
xz -T0 -6 --stdout "$work/images/sdcard.img" > "$out/nabos-$target.img.xz"
cp "$payload/cache-roots" "$out/cache-roots"
cp "$repo/flake.lock" "$out/flake.lock"
cp "$payload/boot.cmd" "$out/boot.cmd"
jq --arg revision "$(git -C "$repo" rev-parse HEAD)" --argjson seconds "$seconds" \
  --argjson development "$development" --arg nixpkgs "$(jq -r '.nodes[.nodes.root.inputs.nixpkgs].locked.rev' "$repo/flake.lock")" \
  '. + {source_revision:$revision,nixpkgs_revision:$nixpkgs,build_seconds:$seconds,development:$development}' \
  "$payload/build.json" > "$out/build-$target.json"
(cd "$out"; sha256sum "nabos-$target.img.xz" "nabos-$target.raucb" ca.cert.pem \
  "build-$target.json" flake.lock boot.cmd cache-roots > "SHA256SUMS-$target")
echo "Built $version for $target in $out; workspace: $work"
