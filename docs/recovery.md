# Recover NabOS with the head button

On a Raspberry Pi Zero W or Zero 2 W running a recovery-capable NabOS image,
hold the rabbit's head button while connecting power. Wait for steady orange
LEDs, then release. Orange appears after ten seconds of stable button detection
in U-Boot; firmware startup time comes before those ten seconds.

| Gesture | Result |
| --- | --- |
| Start with the button released, or release before orange | Normal boot |
| Keep holding after steady orange, including for 30 seconds | Stay armed; nothing is erased or switched |
| Release at steady orange and leave the button alone for five seconds | Roll back to the recorded previous system slot |
| Release at steady orange, then double-click during the five-second orange blink | Factory reset the data partition |
| Single click, third click, unfinished press, or uncertain input | Cancel recovery and boot normally |

Each confirmation press and the gap between clicks must be at most half a
second. The decision is made at the end of the whole five-second window.
Green acknowledges a persisted rollback selection. Red indicates reset;
blinking red indicates a recovery failure.

## After a factory reset

Factory reset formats **only p4, the shared `/data` partition**. It deletes
Wi-Fi profiles, accounts, authentication tokens, SSH authorization and host
keys, preferences, ear calibration, downloaded or uploaded media, update status,
and logs, including hidden files. It retains the preferred system version and
both system partitions. Formatting is a logical reset, not a secure overwrite
of every physical flash cell.

The normal boot creates fresh persistent defaults and a new machine ID.
Connect to the rabbit's onboarding Wi-Fi and configure the device again. Pair
Home Assistant and other integrations again and add your SSH public keys again.
The SSH host key changes, so remove the old saved host key after checking that
you are connecting to your reset rabbit.

The public RAUC update certificate is shipped in each immutable system image at
`/usr/share/nabos/ca.cert.pem`; it is not user data. No signing private key is
shipped. Reset therefore needs no certificate backup or exception in `/data`.
The firmware, U-Boot, raw environments, boot FAT regions, and A/B filesystems
are not formatted or resized.

## Interrupted resets and limits

The reset authorization is saved and reread from the redundant raw U-Boot
environment before recovery starts. The independent recovery kernel, board DTB,
and self-contained initramfs are shipped under `/recovery/` in both boot FAT
images and execute from RAM. They do not use a system slot's boot files,
`/nix/store`, or persistence initrd.

After a power cut, pending authorization takes priority over a new gesture and
normal boot. Recovery checks the partition geometry and starts the format again.
It clears authorization only after filesystem synchronization and verification,
restores three attempts to the recorded preferred slot, and verifies the final
environment update before rebooting. A recovery error blocks normal boot.

Rollback requires a recorded different slot and loadable boot artifacts. The
signed install-check records the actually running `rauc.slot` before RAUC
overwrites the inactive slot. A freshly flashed card has no previous installation
and cannot roll back. Existing fallback and health confirmation still apply.

This format requires reinstallation of existing cards, including earlier NixOS
images. Both systems and bundles use `nabos-native-rauc-zero-armv6` or
`nabos-native-rauc-zero2-arm64`. Older formats are rejected before installation
writes; there is no mixed Debian/NixOS boot or persistent-certificate migration.

Normal boot uses U-Boot's native RAUC bootmethod on SD card 0. A failing selected
slot consumes attempts across reboots before the other slot is selected. Any
return from bootflow dispatch shows failure and reboots; exhausted counters are
restored by the native bootmethod. A pending reset is handled before dispatch and
counter changes. Rollback validation and normal boot share the artifact loader,
and rollback also requires a valid slot boot script before promotion is saved.

Recovery requires working Raspberry Pi firmware and U-Boot. It cannot restore
two damaged system slots or repair a physically faulty card. Copy useful logs
before resetting because they will be erased.

## Hardware release qualification

Use disposable cards on **both** Pi models. Check the visible gesture, extended
holds, cancelled clicks, Linux takeover, fresh onboarding, and power cuts during
authorization, formatting, and completion. Measure the WS2812 2.4 MHz signal,
GRB order, 300 microsecond latch, and FIFO continuity on the actual hardware.
FIFO reliability is a release gate. Native tests, disk fixtures, and virtual
machines do not qualify the physical LEDs or Raspberry Pi boot.
