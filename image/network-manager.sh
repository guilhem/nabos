#!/bin/bash
# Rebuild the vendor packages so Polkit can authenticate NM's system-bus clients.
set -euo pipefail
phase=${1:?fetch or build}
image=$(cd "$(dirname "$0")" && pwd)
lock=$image/network-manager.lock.json
case $phase in
fetch)
  directory=${2:?source input directory}
  mkdir -p "$directory"
  offline=${3:-}
  [[ -z $offline || $offline == --offline ]] || exit 2
  if [[ $offline == --offline ]]; then cmp "$lock" "$directory/source-identity.json"; fi
  while IFS=$'\t' read -r name url digest; do
    archive=$directory/$name
    if ! printf '%s  %s\n' "$digest" "$archive" | sha256sum --check --status; then
      [[ $offline != --offline ]] || { echo "Missing or corrupt replay NM input: $name" >&2; exit 1; }
      curl --fail --location --retry 3 --proto '=https' --proto-redir '=https' "$url" -o "$archive.part"
      printf '%s  %s\n' "$digest" "$archive.part" | sha256sum --check --strict
      mv "$archive.part" "$archive"
    fi
  done < <(python3 - "$lock" <<'PY'
import json, sys
for item in json.load(open(sys.argv[1]))['files']:
    print(item['name'], item['url'], item['sha256'], sep='\t')
PY
  )
  cp "$lock" "$directory/source-identity.json"
  ;;
build)
  [[ -f /etc/rpi-issue && $image == /nabos-build/image ]] || { echo 'Target chroot required' >&2; exit 1; }
  inputs=/nabos-build/inputs
  work=/nabos-build/src/network-manager-build
  cmp "$lock" "$inputs/network-manager/source-identity.json"
  python3 - "$lock" "$inputs/network-manager" <<'PY'
import hashlib, json, pathlib, sys
for item in json.load(open(sys.argv[1]))['files']:
    assert hashlib.sha256((pathlib.Path(sys.argv[2])/item['name']).read_bytes()).hexdigest() == item['sha256'], item['name']
PY
  mapfile -t values < <(python3 - "$lock" <<'PY'
import json, sys
lock = json.load(open(sys.argv[1]))
for key in ('source_version', 'version', 'source_date_epoch'):
    print(lock[key])
PY
  )
  source_version=${values[0]}; version=${values[1]}
  export SOURCE_DATE_EPOCH=${values[2]}
  export DEB_BUILD_OPTIONS='nocheck parallel=2' DEB_BUILD_PROFILES=nocheck
  rm -rf "$work"
  mkdir -p "$work"
  dpkg-source --no-check -x "$inputs/network-manager/network-manager_$source_version.dsc" "$work/source"
  [[ $(dpkg-parsechangelog -l "$work/source/debian/changelog" -S Version) == "$source_version" ]]
  install -m644 "$image/patches/network-manager-polkit-subject.patch" "$work/source/debian/patches/nabos-polkit-subject.patch"
  printf '\nnabos-polkit-subject.patch\n' >> "$work/source/debian/patches/series"
  cat > "$work/source/debian/changelog.nabos" <<CHANGELOG
network-manager ($version) trixie; urgency=medium

  * Preserve authenticated system-bus subjects for Polkit unit checks.

 -- NabOS maintainers <nabos@users.noreply.github.com>  Sat, 03 Oct 2026 00:00:00 +0000

CHANGELOG
  cat "$work/source/debian/changelog" >> "$work/source/debian/changelog.nabos"
  mv "$work/source/debian/changelog.nabos" "$work/source/debian/changelog"
  (cd "$work/source" && dpkg-buildpackage -b --no-sign --build-profiles=nocheck)
  # Vendor packaging skips its tests with Netplan enabled. Run our production
  # serializer test explicitly, including the synthetic private-peer subject.
  mapfile -t tests < <(find "$work/source" -type f -path '*/src/nabos-auth-subject-test')
  (( ${#tests[@]} == 1 )) || { echo 'Missing NM authorization regression binary' >&2; exit 1; }
  "${tests[0]}"
  runtime_debs=()
  for package in network-manager libnm0 network-manager-l10n gir1.2-nm-1.0; do
    matches=()
    for deb in "$work"/*.deb; do
      if [[ $(dpkg-deb -f "$deb" Package) == "$package" ]]; then matches+=("$deb"); fi
    done
    (( ${#matches[@]} == 1 )) || { echo "Missing rebuilt package: $package" >&2; exit 1; }
    [[ $(dpkg-deb -f "${matches[0]}" Version) == "$version" ]]
    install -m644 "${matches[0]}" "$inputs/debs/"
    runtime_debs+=("${matches[0]}")
  done
  dpkg -i "${runtime_debs[@]}"
  # A later upstream version in the fresh APT resolution must not undo this
  # explicit source pin during the runtime image's archived-package upgrade.
  for deb in "$inputs/debs"/*.deb; do
    case $(dpkg-deb -f "$deb" Package) in
      network-manager|libnm0|network-manager-l10n|gir1.2-nm-1.0)
        if [[ $(dpkg-deb -f "$deb" Version) != "$version" ]]; then rm "$deb"; fi
        ;;
    esac
  done
  dpkg-query -W -f='${Package}\t${Version}\t${Architecture}\n' > "$inputs/debs/manifest.tsv"
  cp "$inputs/debs/manifest.tsv" /nabos-build/builder-packages.tsv
  (cd "$inputs/debs" && dpkg-scanpackages --multiversion . /dev/null | gzip -n > Packages.gz
    sha256sum ./*.deb Packages.gz > SHA256SUMS)
  # Keep source/build provenance in the image; packaged files remain dpkg-owned.
  mkdir -p /nabos-build/runtime/usr/share/nabos
  cp "$lock" /nabos-build/runtime/usr/share/nabos/network-manager-source.json
  install -Dm644 "$image/patches/network-manager-polkit-subject.patch" \
    /nabos-build/runtime/usr/share/doc/network-manager/nabos-polkit-subject.patch
  { sha256sum /usr/sbin/NetworkManager
    printf '%s  /usr/share/doc/network-manager/nabos-polkit-subject.patch\n' \
      "$(sha256sum "$image/patches/network-manager-polkit-subject.patch" | cut -d' ' -f1)"
  } > /nabos-build/runtime/usr/share/nabos/network-manager-build.sha256
  ;;
*) echo "Unknown NetworkManager build phase: $phase" >&2; exit 2 ;;
esac
