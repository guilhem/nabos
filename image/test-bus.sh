#!/bin/bash
# A private test bus with ProcessFD support; never install into the host or image.
set -euo pipefail
[[ $# == 2 ]] || { echo 'Usage: image/test-bus.sh CACHE OUTPUT' >&2; exit 2; }
repo=$(cd "$(dirname "$0")/.." && pwd)
cache=$(realpath -m "$1")
out=$(realpath -m "$2")
case $(uname -m) in
  x86_64) host=amd64; multiarch=x86_64-linux-gnu ;;
  aarch64) host=arm64; multiarch=aarch64-linux-gnu ;;
  *) echo 'Test bus requires an x86_64 or ARM64 host' >&2; exit 2 ;;
esac
mkdir -p "$cache" "$out"
# Archive both hosts so image replay is independent of the first builder's CPU.
for arch in amd64 arm64; do
  mkdir -p "$cache/$arch"
  while IFS=$'\t' read -r name url hash; do
    deb=$cache/$arch/$name.deb
    if ! printf '%s  %s\n' "$hash" "$deb" | sha256sum --check --status; then
      curl --fail --location --retry 3 "$url" -o "$deb.part"
      printf '%s  %s\n' "$hash" "$deb.part" | sha256sum --check >&2
      mv "$deb.part" "$deb"
    fi
    if [[ $arch == "$host" ]]; then dpkg-deb --extract "$deb" "$out"; fi
  done < <(jq -r --arg a "$arch" '.test_bus[$a] | to_entries[] | [.key, .value.url, .value.sha256] | @tsv' "$repo/image/sources.lock.json")
done
printf '#!/bin/bash\nexec env LD_LIBRARY_PATH=%q %q "$@"\n' \
  "$out/usr/lib/$multiarch" "$out/usr/bin/dbus-daemon" > "$out/dbus-daemon"
chmod 755 "$out/dbus-daemon"
printf '%s\n' "$out"
