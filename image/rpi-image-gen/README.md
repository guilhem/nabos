# rpi-image-gen package-base prototype

This experiment builds an ARM64 Debian Trixie root filesystem directly from Debian
and Raspberry Pi packages, without downloading or modifying a Raspberry Pi OS disk
image. It evaluates the package-base generation part of a possible replacement for
`image/build.sh` and `image/prepare.sh`.

The output is **a package-base archive, not a bootable NabOS SD image**. It contains
the Raspberry Pi v8 kernel and Zero 2 W DTB, Wi-Fi firmware, systemd, NetworkManager,
PipeWire/WirePlumber and RAUC. It does not contain NabOS/device-core binaries,
TagTagTag drivers, Linux Voice Assistant, the service-account/configuration overlay,
U-Boot, the MBR A/B layout or a signed RAUC bundle. Production builds remain separate.

## Run

Use a native ARM64 Debian/Ubuntu host with user namespaces and at least 8 GiB of
free space in both the workspace and `/var/tmp`. The upstream supported hosts are
Debian Bookworm/Trixie ARM64; the prototype workflow exercises Ubuntu 24.04 ARM64,
also used by upstream CI.

```bash
bash image/rpi-image-gen/build.sh check
sudo build/rpi-image-gen/upstream/install_deps.sh
bash image/rpi-image-gen/build.sh
```

The first command needs Python 3 with PyYAML, python-debian and jsonschema, plus
dpkg-dev. On Ubuntu these are `python3-yaml python3-debian python3-jsonschema dpkg-dev`.
The pinned builder is downloaded into the ignored `build/rpi-image-gen/upstream`.
The build runs as the ordinary user; upstream uses its own namespace tooling.
The CLI uses `build/rpi-image-gen/tmp` for its temporary files. Upstream
mmdebstrap builds the rootfs under `/var/tmp`, which needs separate free space.

The dedicated **rpi-image-gen prototype** workflow runs on this prototype branch
and can also be started manually. It uploads `nabos-rpi-image-gen-base-arm64` for
seven days. It neither publishes releases nor changes the production workflow.

## Evidence and limits

`dist/rpi-image-gen/` contains the rootfs archive, actual installed package versions,
the resolved builder configuration, source revisions, checksums and build resource
measurements. Checks inspect the actual archive: ARM64/all packages, required system
components, kernel modules and the Zero 2 W DTB, absence of development packages,
and absence of shared machine identities or generated SSH host keys.

The builder revision is pinned, but APT repositories are rolling. The package
manifest records one build's resolution; this does not establish byte-for-byte
reproducibility or offline replay. No release signing secrets are used.

Successful generation and archive inspection do not prove that services start with
NabOS's read-only root, persisted `/data` and systemd restrictions. Those checks,
the two board boots, hardware drivers and RAUC install/rollback are still required
before integrating this rootfs into the production assembler. ARMv6 needs its own
Raspbian/kernel configuration and is not exercised by this first prototype.
