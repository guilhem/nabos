#!/bin/bash
# Cross-compile the service without a target root filesystem.
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
target=${1:?Usage: image/build-go.sh TARGET VERSION OUTPUT_DIR [INPUTS_DIR]}
version=${2:?version required}
out=$(realpath -m "${3:?output directory required}")
inputs=${4:-}
[[ $target == zero-armv6 || $target == zero2-arm64 ]] || exit 2
[[ $version =~ ^[a-zA-Z0-9][a-zA-Z0-9.+_-]{0,63}$ ]] || exit 2
lock=$repo/image/sources.lock.json
[[ $(go env GOVERSION) == "go$(jq -r .tools.go "$lock")" ]] || { echo 'Go toolchain mismatch' >&2; exit 1; }
mkdir -p "$out/inputs/go-modcache/cache"
if [[ -n $inputs ]]; then
  inputs=$(realpath "$inputs")
  cmp "$repo/services/go.sum" "$inputs/go.sum"
  export GOMODCACHE="$inputs/go-modcache" GOPROXY=off
fi
cd "$repo/services"
go mod download
CGO_ENABLED=0 GOOS=linux GOARCH=$(jq -r --arg t "$target" '.targets[$t].goarch' "$lock") \
  GOARM=$(jq -r --arg t "$target" '.targets[$t].goarm' "$lock") \
  go build -trimpath -ldflags="-s -w -X main.version=$version" -o "$out/nab-service" ./cmd/nab-service
# Keep setup-go's module cache in place; archive the inputs needed for replay.
cp -a "$(go env GOMODCACHE)/cache/download" "$out/inputs/go-modcache/cache/"
cp go.sum "$out/inputs/go.sum"
printf '%s\n' "$target" "$(git rev-parse HEAD)" > "$out/build-info"
printf '%s\n' "$version" > "$out/version"
