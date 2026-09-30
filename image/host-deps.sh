#!/bin/bash
# Ubuntu 24.04 host dependencies, shared by local and GitHub Actions builds.
set -euo pipefail
. /etc/os-release
[[ $ID == ubuntu && $VERSION_ID == 24.04 ]] || { echo 'Ubuntu 24.04 required' >&2; exit 1; }
sudo apt-get update
# Mosquitto is host-only tooling for Home Assistant integration fixtures.
sudo apt-get install -y --no-install-recommends \
  genimage=17-2 rauc=1.11.3-2 \
  curl ca-certificates xz-utils python3 jq fdisk util-linux e2fsprogs dosfstools mtools \
  u-boot-tools libubootenv-tool squashfs-tools openssl clang lld patch kmod \
  qemu-user-static binfmt-support device-tree-compiler mosquitto mosquitto-clients \
  build-essential bison flex libssl-dev zlib1g-dev python3-dev python3-setuptools
