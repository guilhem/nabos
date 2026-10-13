# Diagnose a failed NabOS boot

NixOS images save startup logs on the shared `/data` partition so they can be
read after an A/B rollback. Installing an image with this support is required;
it cannot recover journals lost during earlier boots.

## Read the logs after rollback

Connect to the working slot using the existing SSH account. The initrd logs are
plain text, including when the working slot is still Debian:

```sh
sudo cat /data/system/boot-logs/slot-A.initrd.log
sudo cat /data/system/boot-logs/slot-B.initrd.log
```

Each file describes the most recent captured attempt for that slot and includes
the boot ID and kernel command line. A file may be absent if that slot has not
booted with diagnostic support.

The main system uses the standard systemd journal:

```sh
sudo journalctl --directory=/data/system/journal --list-boots
```

Copy the failed boot's ID from that list, then replace `BOOT_ID` below:

```sh
sudo journalctl --directory=/data/system/journal --boot=BOOT_ID --no-pager
sudo journalctl --directory=/data/system/journal --boot=BOOT_ID --no-pager \
  -u wpa_supplicant -u NetworkManager -u device-core -u nab-hardware \
  -u nabos -u nabos-health -u user@1004
```

If the older Debian `journalctl` cannot read the new journal format, copy the
journal directory to a Linux computer with a recent systemd and use the same
`--directory` commands there. The text initrd logs do not need systemd.
Logs are private diagnostic data; inspect them before sharing them.

## Collection and recovery boundaries

The initrd collector starts asynchronously after the existing persistence code
has mounted writable `/data`. It saves the buffered early journal and follows
new entries. Its two files are capped at 4 MiB each and are replaced on the next
captured attempt of the same slot. The runtime journal targets 32 MiB, uses
4 MiB files, reserves 64 MiB of free space, and syncs every five seconds.
Journal size limits allow filesystem and active-file overhead.

Collection is optional: it does not add a boot-health condition or a required
service. U-Boot still consumes three unconfirmed attempts; only the existing
local health checks can mark a RAUC slot good. The persistence failure action
remains `reboot-force`. Logging does not change the partition layout, application
data, Wi-Fi profiles, SSH keys, or fallback slot.

Failures before `/data` becomes writable cannot be saved on that partition.
Volatile fallback data is not a durable log destination. An abrupt power cut or
forced reboot can lose the final messages; a full or faulty card can prevent
collection. Process timeouts cannot interrupt a kernel stalled on SD I/O.
The collector's size cap can also omit later initrd messages.

## Wi-Fi configuration

NetworkManager continues to own the saved Wi-Fi profiles and configures
`wpa_supplicant` through D-Bus. WPA uses the NixOS-generated configuration with
`update_config=0`, avoiding creation or modification of an imperative
configuration under immutable `/etc`. The upstream service confinement stays
enabled.

The VM checks exercise read-only filesystems and effective systemd units.
Physical Wi-Fi, SD failures and migration rollback on both Raspberry Pi models
still require hardware qualification.
