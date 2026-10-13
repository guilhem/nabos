#!/usr/bin/env bash
# Test the shipped failure policy in a diskless x86 VM, never on the host PID 1.
set -euo pipefail
[[ $# == 3 && -f $1 && -f $2 && -f $3 ]] || {
  echo 'Usage: bash image/test-initrd-failure.sh ARM_INITRD NATIVE_INITRD NATIVE_KERNEL' >&2; exit 2;
}
archive=$(realpath "$1")
native=$(realpath "$2")
kernel=$(realpath "$3")
work=$(mktemp -d)
trap 'chmod -R u+w "$work"; rm -rf -- "$work"' EXIT
mkdir "$work/arm" "$work/native"
(cd "$work/arm" && zstd -dc "$archive" | cpio -idm --quiet --no-absolute-filenames --no-preserve-owner)
(cd "$work/native" && zstd -dc "$native" | cpio -idm --quiet --no-absolute-filenames --no-preserve-owner)
mapfile -t units < <(find "$work/arm/nix/store" -name nabos-persist.service -type f)
[[ ${#units[@]} == 1 ]] || { echo 'Expected one shipped persistence unit' >&2; exit 1; }
policy=$(awk '/^FailureAction=/{print}' "${units[0]}")
mapfile -t binaries < <(find "$work/native/nix/store" -path '*-coreutils-*/bin/false')
[[ ${#binaries[@]} == 1 ]] || { echo 'Expected one native false executable' >&2; exit 1; }
probe_false=${binaries[0]#"$work/native"}
chmod u+w "$work/native/etc/systemd"
rm "$work/native/etc/systemd/system"
mkdir "$work/native/etc/systemd/system"
cat > "$work/native/etc/systemd/system/default.target" <<'UNIT'
[Unit]
DefaultDependencies=no
Requires=nabos-persist.service
After=nabos-persist.service
UNIT
cat > "$work/native/etc/systemd/system/nabos-persist.service" <<UNIT
[Unit]
DefaultDependencies=no
$policy
[Service]
Type=oneshot
ExecStart=$probe_false
UNIT
(cd "$work/native" && find . -print0 | cpio --quiet -o -H newc --null | zstd -q > "$work/probe.initrd")
timeout --kill-after=2s 30s qemu-system-x86_64 -accel tcg -m 512 -nodefaults -nographic \
  -serial stdio -monitor none -nic none -no-reboot -kernel "$kernel" -initrd "$work/probe.initrd" \
  -append 'rdinit=/init console=ttyS0 panic=-1 systemd.log_target=console systemd.log_level=info' \
  > "$work/console.log" 2>&1 || { cat "$work/console.log" >&2; exit 1; }
grep -q 'nabos-persist.service: Failed with result' "$work/console.log"
grep -q 'Syncing filesystems and block devices' "$work/console.log"
grep -q 'All filesystems unmounted' "$work/console.log"
grep -q 'Rebooting' "$work/console.log"
grep -q 'reboot: Restarting system' "$work/console.log"
grep -E 'nabos-persist.service: Failed with result|Syncing filesystems and block devices|All filesystems unmounted|reboot: Restarting system' "$work/console.log"
echo 'PASS: shipped persistence failure policy rebooted diskless PID 1 within 30 seconds'
