#!/usr/bin/env bash
# Build only the runtime cache roots; SD images/signatures are deliberately outside this timer.
set -euo pipefail
if [[ $# != 3 ]]; then
  echo "Usage: bash image/benchmark.sh cold|warm|version|nixpkgs TARGET VERSION" >&2
  exit 2
fi
phase=$1 target=$2 version=$3
case "$phase" in cold|warm|version|nixpkgs) ;; *) exit 2 ;; esac
case "$target" in zero-armv6|zero2-arm64) ;; *) exit 2 ;; esac
[[ "$version" =~ ^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$ ]] || exit 2
cd "$(dirname "$0")/.."
export TMPDIR="$PWD/build/nix-tmp"
mkdir -p "$TMPDIR"
out="$PWD/dist/measurements/$target/$phase"
mkdir -p "$out"
export NABOS_FLAKE="git+file://$PWD" NABOS_TARGET="$target" NABOS_VERSION="$version"
nix_cmd=(nix --extra-experimental-features 'nix-command flakes')
build_system=$("${nix_cmd[@]}" eval --impure --raw --expr builtins.currentSystem)
export NABOS_BUILD_SYSTEM="$build_system"
expr='let f = builtins.getFlake (builtins.getEnv "NABOS_FLAKE");
  p = f.lib.mkImage {
    buildSystem = builtins.getEnv "NABOS_BUILD_SYSTEM";
    target = builtins.getEnv "NABOS_TARGET";
    version = builtins.getEnv "NABOS_VERSION";
  }; in { system = p.system.config.system.build.toplevel; uboot = p.packages.uboot; }'
cache_options=()
if [[ "$phase" == cold ]]; then
  # Cold means no NabOS substitutions; official upstream binaries remain enabled.
  cache_options=(--option substituters https://cache.nixos.org --option extra-substituters '')
fi
SECONDS=0
"${nix_cmd[@]}" build --impure --no-update-lock-file --no-link --print-build-logs \
  "${cache_options[@]}" --json --expr "$expr" system uboot \
  > "$out/outputs.json" 2> >(tee "$out/build.log" >&2)
elapsed=$SECONDS
python3 - "$out" "$phase" "$target" "$version" "$build_system" "$elapsed" <<'PY'
import json, pathlib, sys
out, phase, target, version, system, seconds = sys.argv[1:]
out = pathlib.Path(out)
outputs = json.loads((out / 'outputs.json').read_text())
roots = [result['outputs']['out'] for result in outputs]
assert len(roots) == 2 and all(p.startswith('/nix/store/') for p in roots), roots
(out / 'cache-roots.txt').write_text('\n'.join(roots) + '\n')
lock = json.loads(pathlib.Path('flake.lock').read_text())
metrics = dict(phase=phase, target=target, version=version, build_system=system,
               build_seconds=int(seconds), nixpkgs_revision=lock['nodes']['nixpkgs']['locked']['rev'],
               scope='system-and-uboot-runtime-closures', hardware_validated=False)
(out / ('build-' + target + '.json')).write_text(json.dumps(metrics, indent=2) + '\n')
(out / 'flake.lock').write_text(pathlib.Path('flake.lock').read_text())
PY
