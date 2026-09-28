#!/bin/bash
# Link on the host against a small, locked Raspberry Pi OS-compatible sysroot.
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
target=${1:?Usage: image/build-rust.sh TARGET OUTPUT_DIR [INPUTS_DIR]}
out=$(realpath -m "${2:?output directory required}")
inputs=${3:-}
case $target in
  zero-armv6) triple=arm-linux-gnueabihf; cpu=(-mcpu=arm1176jzf-s -mfpu=vfp -mfloat-abi=hard) ;;
  zero2-arm64) triple=aarch64-linux-gnu; cpu=(-mcpu=cortex-a53) ;;
  *) exit 2 ;;
esac
lock=$repo/image/sources.lock.json
[[ $(rustc --version | cut -d' ' -f2) == "$(jq -r .tools.rust "$lock")" ]] || { echo 'Rust toolchain mismatch' >&2; exit 1; }
rust_target=$(jq -r --arg t "$target" '.targets[$t].rust_target' "$lock")
mkdir -p "$out/inputs/rust-sysroot"
if [[ -n $inputs ]]; then
  inputs=$(realpath "$inputs")
  cmp "$repo/core/Cargo.lock" "$inputs/Cargo.lock"
  cmp "$repo/image/rust-sysroots.lock.json" "$inputs/rust-sysroots.lock.json"
  cp -a "$inputs/rust-sysroot/." "$out/inputs/rust-sysroot/"
  cp -a "$inputs/cargo-vendor" "$out/inputs/"
else
  cargo vendor --locked --manifest-path "$repo/core/Cargo.toml" "$out/inputs/cargo-vendor" > /dev/null
fi
sysroot=$repo/build/sysroot/$target
rm -rf "$sysroot"
mkdir -p "$sysroot/usr/lib"
ln -s usr/lib "$sysroot/lib"
while IFS=$'\t' read -r name url hash; do
  deb=$out/inputs/rust-sysroot/$name.deb
  if ! printf '%s  %s\n' "$hash" "$deb" | sha256sum --check --status; then
    [[ -z $inputs ]] || { echo "Missing or corrupt replay sysroot package: $name" >&2; exit 1; }
    curl --fail --location --retry 3 "$url" -o "$deb.part"
    printf '%s  %s\n' "$hash" "$deb.part" | sha256sum --check
    mv "$deb.part" "$deb"
  fi
  dpkg-deb --extract "$deb" "$sysroot"
done < <(jq -r --arg t "$target" '.[$t] | to_entries[] | [.key, .value.url, .value.sha256] | @tsv' "$repo/image/rust-sysroots.lock.json")
# dpkg packages can contain absolute links; keep all lookup inside the sysroot.
while IFS= read -r -d '' link; do
  destination=$(readlink "$link")
  ln -sfn "$(realpath -m --relative-to="$(dirname "$link")" "$sysroot$destination")" "$link"
done < <(find "$sysroot" -type l -lname '/*' -print0)
linker=$sysroot/target-cc-$(sha256sum "$repo/image/rust-sysroots.lock.json" | cut -c1-16)
printf '#!/bin/bash\nexec %s"$@"\n' "$(printf '%q ' clang "--target=$triple" "--sysroot=$sysroot" "--gcc-toolchain=$sysroot/usr" -fuse-ld=lld "${cpu[@]}")" > "$linker"
chmod 755 "$linker"
linker_key=CARGO_TARGET_$(tr '[:lower:]-' '[:upper:]_' <<< "$rust_target")_LINKER
export CARGO_TARGET_DIR=${CARGO_TARGET_DIR:-$repo/core/target}
env "$linker_key=$linker" cargo --config 'source.crates-io.replace-with="vendored-sources"' \
  --config "source.vendored-sources.directory=\"$out/inputs/cargo-vendor\"" \
  build --locked --offline --release --manifest-path "$repo/core/Cargo.toml" --target "$rust_target"
install -m755 "$CARGO_TARGET_DIR/$rust_target/release/nab-core" "$out/nab-core"
cp "$repo/core/Cargo.lock" "$repo/image/rust-sysroots.lock.json" "$out/inputs/"
printf '%s\n' "$target" "$(git -C "$repo" rev-parse HEAD)" > "$out/build-info"
