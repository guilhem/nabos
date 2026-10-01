#!/bin/bash
# Cache only the prepared OS, before application binaries, identity and signing.
set -euo pipefail
action=${1:?key, restore or save}
target=${2:?zero-armv6 or zero2-arm64}
[[ $target == zero-armv6 || $target == zero2-arm64 ]] || exit 2
repo=$(cd "$(dirname "$0")/.." && pwd)
identity=$(
  cd "$repo"
  { printf '%s\n' "$target" "${NABOS_BASE_CACHE_EPOCH:-$(date -u +%F)}"
    sha256sum image/base-cache.sh image/build.sh image/prepare.sh image/sources.lock.json \
      image/lva-requirements.lock image/patches/*.patch image/build-config/*
  } | sha256sum | cut -d' ' -f1
)
if [[ $action == key ]]; then printf '%s\n' "$identity"; exit 0; fi
work=$(realpath "${3:?build workspace required}")
cache=$repo/build/cache/base/$target
mkdir -p "$(dirname "$cache")"
# Shared local builds must not read an archive while another replaces it.
exec 9>"$cache.lock"
flock 9
temporary=
trap 'if [[ -n $temporary ]]; then rm -rf -- "$temporary"; fi' EXIT
case $action in
  restore)
    [[ -f $cache/identity && $(cat "$cache/identity") == "$identity" ]] || exit 1
    if ! (cd "$cache" && sha256sum --check --strict SHA256SUMS); then
      echo 'Prepared base cache is corrupt; rebuilding it' >&2
      exit 1
    fi
    temporary=$(mktemp -d "$work/cache-restore.XXXXXX")
    tar --extract --sparse --no-same-owner --file="$cache/base.tar" --directory="$temporary"
    [[ -f $temporary/payload/kernel-release && -f $temporary/payload/builder-packages.tsv && \
       -f $temporary/payload/inputs/debs/manifest.tsv ]]
    cp -a "$temporary/payload/." "$work/payload/"
    mv "$temporary/base.img" "$work/base.img"
    echo 'Restored prepared base image'
    ;;
  save)
    temporary=$(mktemp -d "$(dirname "$cache")/.${target}.XXXXXX")
    files=(base.img payload/kernel-release payload/builder-packages.tsv payload/inputs/debs)
    if [[ $target == zero2-arm64 ]]; then files+=(payload/inputs/wheels); fi
    # Preserve holes here: Actions' outer archive otherwise reads all 6 GiB.
    tar --create --sparse --file="$temporary/base.tar" --directory="$work" "${files[@]}"
    printf '%s\n' "$identity" > "$temporary/identity"
    (cd "$temporary" && sha256sum base.tar > SHA256SUMS)
    rm -rf -- "$cache"
    mv "$temporary" "$cache"
    echo 'Saved prepared base image'
    ;;
  *) echo "Unknown base cache action: $action" >&2; exit 2 ;;
esac
