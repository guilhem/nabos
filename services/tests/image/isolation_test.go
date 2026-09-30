package image

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Run the real verifier with real sparse copying and checksums. Only privileged
// operations, target execution and expensive builds/tests are replaced.
func TestImageIsolation(t *testing.T) {
	cp := must(exec.LookPath("cp"))
	checksum := must(exec.LookPath("sha256sum"))
	dtDir := t.TempDir()
	pristine := filepath.Join(dtDir, "pristine.dtb")
	profile := filepath.Join(dtDir, "nabos.dtbo")
	slotDTB := filepath.Join(dtDir, "slot.dtb")
	run(t, baseDTS+`/ {
		soc {
			uart0: serial@7e201000 { reg = <0x7e201000 0x1000>; };
			fb: fb {};
			vchiq: mailbox@7e00b840 { reg = <0x7e00b840 0x3c>; };
			usb: usb@7e980000 { reg = <0x7e980000 0x10000>; };
			watchdog@7e100000 { reg = <0x7e100000 0x28>; };
		};
		cam1_reg: cam1_regulator {};
		cam_dummy_reg: cam_dummy_reg {};
	};`, "dtc", "-@", "-I", "dts", "-O", "dtb", "-o", pristine, "-")
	run(t, "", "dtc", "-@", "-I", "dts", "-O", "dtb", "-o", profile, filepath.Join(imageDir, "nabos-overlay.dts"))
	run(t, "", "fdtoverlay", "-i", pristine, "-o", slotDTB, profile)
	for _, scenario := range []struct {
		name, target  string
		fail, corrupt bool
		code          int
	}{
		{"test failure", "zero-armv6", true, false, 23},
		{"success", "zero2-arm64", false, false, 0},
		{"changed original", "zero-armv6", false, true, 1},
		{"changed original on failure", "zero2-arm64", true, true, 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			tmp := t.TempDir()
			checkout := filepath.Join(tmp, "repo")
			payload := filepath.Join(tmp, "payload")
			fixture := filepath.Join(tmp, "root")
			boot := filepath.Join(tmp, "boot")
			kernel := "6.12-rpi-v6"
			if scenario.target == "zero2-arm64" {
				kernel = "6.12-rpi-v8"
			}
			kernelDTBs := filepath.Join(fixture, "usr/lib/modules", kernel, "dtb")
			if scenario.target == "zero2-arm64" {
				kernelDTBs = filepath.Join(kernelDTBs, "broadcom")
			}
			for _, dir := range []string{filepath.Join(checkout, "image"), filepath.Join(checkout, "services"),
				filepath.Join(checkout, "build/iot"), filepath.Join(payload, "inputs/go-modcache"),
				filepath.Join(fixture, "data"), filepath.Join(fixture, "boot/dtb"), kernelDTBs,
				filepath.Join(payload, "src/uboot/scripts"), filepath.Join(fixture, "usr/bin"),
				filepath.Join(fixture, "usr/lib/aarch64-linux-gnu"), filepath.Join(fixture, "usr/share/nabos/sounds"),
				filepath.Join(fixture, "usr/share/nabos/choreographies"), filepath.Join(fixture, "lib/modules", kernel, "updates/nabos"), boot} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink("modules/"+kernel+"/dtb", filepath.Join(fixture, "usr/lib/linux-image-"+kernel)); err != nil {
				t.Fatal(err)
			}
			copyFile(t, filepath.Join(imageDir, "test.sh"), filepath.Join(checkout, "image/test.sh"))
			write(t, filepath.Join(checkout, "image/test-bus.sh"), `set -eu
[ "$1" = "$TEST_PAYLOAD/inputs/test-bus" ]
[ "$2" = "$(dirname "$(cat "$STATE/copy")")/test-bus" ]
mkdir -p "$1" "$2"
printf '%s\n' "$2"
`)
			write(t, filepath.Join(checkout, "build/iot/keep"), "another build")
			for name, script := range map[string]string{
				filepath.Join(payload, "src/uboot/scripts/config"):                        "exit 0",
				filepath.Join(fixture, "usr/bin/nab-core"):                                "echo shipped-core",
				filepath.Join(fixture, "usr/bin/device-core"):                             "echo shipped-device-core",
				filepath.Join(fixture, "usr/bin/nab-service"):                             "echo shipped-service",
				filepath.Join(fixture, "usr/bin/dtoverlay"):                               "exit 0",
				filepath.Join(fixture, "usr/lib/aarch64-linux-gnu/ld-linux-aarch64.so.1"): "set -eu\n[ \"$1\" = --library-path ]\nshift 2\nexec \"$@\"",
			} {
				if err := os.WriteFile(name, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			write(t, filepath.Join(fixture, "lib/modules", kernel, "updates/nabos/ears.ko"), "shipped module")
			write(t, filepath.Join(boot, "u-boot.bin"), "shipped U-Boot")
			write(t, filepath.Join(boot, "boot.env"), "nabos_dtb="+dtbs[scenario.target]+"\n")
			copyFile(t, pristine, filepath.Join(boot, dtbs[scenario.target]))
			copyFile(t, pristine, filepath.Join(kernelDTBs, dtbs[scenario.target]))
			copyFile(t, slotDTB, filepath.Join(fixture, "boot/dtb", dtbs[scenario.target]))
			expected := filepath.Join(tmp, "expected-uboot.bin")
			copyFile(t, filepath.Join(boot, "u-boot.bin"), expected)
			original := filepath.Join(tmp, "production image.img")
			write(t, original, "production image")
			if err := os.Truncate(original, 3*MiB); err != nil {
				t.Fatal(err)
			}
			before := sha256.Sum256([]byte(read(t, original)))
			fake := newFakes(t, tmp, map[string]string{
				"cp":        fmt.Sprintf("exec %q \"$@\"", cp),
				"sha256sum": fmt.Sprintf("exec %q \"$@\"", checksum),
				"sudo":      "case $1 in --preserve-env|-n) shift ;; esac\nexec \"$@\"",
				"unshare":   "set -eu\n[ \"$1 $2 $3\" = '--mount --propagation private' ]\nshift 3\nexec \"$@\"",
				"setpriv":   "shift 3\nexec \"$@\"",
				"uname":     "echo aarch64",
				"udevadm":   "exit 0",
				"losetup": `set -eu
if [ "$1" = --detach ]; then
  [ "$2" = /dev/loop-nabos-test ]
else
  [ "$1 $2 $3" = '--find --show --partscan' ]
  [ "$4" != "$ORIGINAL" ]
  cmp "$4" "$ORIGINAL"
  printf '%s\n' "$4" > "$STATE/copy"
  printf 'test copy changed' >> "$4"
  echo /dev/loop-nabos-test
fi`,
				"mount": `set -eu
if [ "$1" = --rbind ]; then
  [ "$2 $3" = "/dev $(dirname "$(cat "$STATE/copy")")/root/dev" ]
  exit 0
elif [ "$1" = --make-rslave ]; then
  [ "$2" = "$(dirname "$(cat "$STATE/copy")")/root/dev" ]
  exit 0
fi
if [ "$1" = -t ]; then
  case "$2" in
    tmpfs)
      [ "$3 $4 $5" = '-o nosuid,nodev,mode=0755 tmpfs' ]
      [ "$6" = "$(dirname "$(cat "$STATE/copy")")/root/run" ] ;;
    proc) [ "$3 $4" = "proc $(dirname "$(cat "$STATE/copy")")/root/proc" ] ;;
    *) exit 1 ;;
  esac
  exit 0
fi
if [ "$1 $2 $3" = '-o rw /dev/loop-nabos-testp4' ]; then
  [ "$4" = "$(dirname "$(cat "$STATE/copy")")/root/data" ]
  exit 0
fi
[ "$1 $2" = '-o ro' ]
case "$3" in
  /dev/loop-nabos-testp1) cp -a "$BOOT_FIXTURE/." "$4/" ;;
  /dev/loop-nabos-testp2) cp -a "$ROOT_FIXTURE/." "$4/" ;;
  *) exit 1 ;;
esac
touch "$4.mounted"`,
				"mountpoint": "test -f \"$2.mounted\"",
				"umount":     "set -eu\n[ \"$1\" = --recursive ]\nrm \"$2.mounted\"",
				"chroot": `set -eu
[ "$1" = "$(dirname "$(cat "$STATE/copy")")/root" ]
case "$2" in
  /usr/bin/python3) [ "$3 $4" = '-B -' ]; cat > "$STATE/readonly-check" ;;
  /usr/sbin/sshd)
    [ "$3" = -G ]
    printf '%s\n' 'allowusers nabos' 'permitrootlogin no' 'authenticationmethods publickey' \
      'passwordauthentication no' 'kbdinteractiveauthentication no' 'usepam yes' \
      'strictmodes yes' 'authorizedkeysfile /data/device-core/ssh/authorized_keys' ;;
  getent)
    [ "$3 $4" = 'passwd nabos' ]
    echo 'nabos:x:1000:1000::/var/lib/nabos:/bin/bash' ;;
  /usr/sbin/visudo) [ "$3" = --check ] ;;
  /bin/sh) cat > "$STATE/lva-check" ;;
  *) exit 1 ;;
esac`,
				"modinfo": "set -eu\n[ \"$1 $2\" = '-F vermagic' ]\n[ -f \"$3\" ]\nprintf '%s SMP\n' \"$KERNEL\"",
				"qemu-arm-static": `set -eu
[ "$1 $2 $3" = '-cpu arm1176 -L' ]
shift 4
exec "$@"`,
				"make": `set -eu
for arg do
  case "$arg" in O=*) out=${arg#O=} ;; esac
done
[ "$out" = "$(dirname "$(cat "$STATE/copy")")/uboot-sandbox" ]
mkdir -p "$out"
touch "$out/u-boot"
chmod 755 "$out/u-boot"`,
				"go": `set -eu
work=$(dirname "$(cat "$STATE/copy")")
case "$PATH" in "$work/test-bus":*) ;; *) exit 1 ;; esac
[ "$GOCACHE" = "$work/go-cache" ]
[ "$GOMODCACHE" = "$work/go-modcache" ]
[ "$TMPDIR" = "$work/tmp" ]
case "$*" in
  *'./tests/integration ./tests/image')
    [ "$NABOS_TEST_ASSETS" = "$work/root/usr/share/nabos" ]
    [ "$("$NAB_CORE_BIN" --version)" = shipped-core ]
    [ "$("$NAB_SERVICE_BIN" --version)" = shipped-service ]
    [ "$("$DEVICE_CORE_BIN" --version)" = shipped-device-core ]
    [ "$NABOS_IMAGE_BOOT" = "$work/boot" ]
    [ "$NABOS_VENDOR_DTBS" = "$work/root/boot/dtb" ]
    [ "$NABOS_IMAGE_OVERLAYS" = "$work/root/boot/overlays" ]
    [ "$NABOS_IMAGE_ENV" = "$work/uboot.env" ]
    if [ "$CORRUPT" = true ]; then printf corrupted >> "$ORIGINAL"; fi
    if [ "$FAIL_TEST" = true ]; then exit 23; fi ;;
  *) exit 1 ;;
esac`,
			})
			r := execute(t, "", fake.env("GO=go", "NABOS_BUILD_NAMESPACE=", "ORIGINAL="+original, "STATE="+tmp,
				"BOOT_FIXTURE="+boot, "ROOT_FIXTURE="+fixture, "KERNEL="+kernel, "TEST_PAYLOAD="+payload,
				fmt.Sprintf("FAIL_TEST=%t", scenario.fail), fmt.Sprintf("CORRUPT=%t", scenario.corrupt)),
				"bash", filepath.Join(checkout, "image/test.sh"), scenario.target, original, payload, expected)
			calls := strings.Join(fake.calls(t), "\n")
			if r.code != scenario.code {
				t.Fatalf("exit %d, want %d\n%s%s\n%s", r.code, scenario.code, r.stdout, r.stderr, calls)
			}
			copy := strings.TrimSpace(read(t, filepath.Join(tmp, "copy")))
			work := filepath.Dir(copy)
			if filepath.Dir(work) != filepath.Join(checkout, "build/iot") {
				t.Fatalf("copy outside owned workspace: %s", copy)
			}
			for _, call := range []string{
				"unshare --mount --propagation private", "cp --reflink=auto --sparse=always -- " + original + " " + copy,
				"losetup --find --show --partscan " + copy, "go test -count=1 -timeout 20m -v ./tests/integration",
				"chroot " + work + "/root /usr/sbin/sshd -G", "chroot " + work + "/root getent passwd nabos",
				"chroot " + work + "/root /usr/sbin/visudo --check",
				"chroot " + work + "/root /usr/bin/python3 -B -",
				"mount --rbind /dev " + work + "/root/dev", "mount --make-rslave " + work + "/root/dev",
				"umount --recursive " + work + "/boot", "umount --recursive " + work + "/root", "losetup --detach /dev/loop-nabos-test",
			} {
				if !strings.Contains(calls, call) {
					t.Errorf("missing %q in\n%s", call, calls)
				}
			}
			if strings.Count(calls, "sha256sum -- "+original+"\n") != 1 || !strings.HasSuffix(calls, "sha256sum -- "+original) {
				t.Errorf("original not hashed before and after verification:\n%s", calls)
			}
			if _, err := os.Stat(work); !os.IsNotExist(err) {
				t.Errorf("copy/sandbox workspace not removed: %s (%v)", work, err)
			}
			if read(t, filepath.Join(checkout, "build/iot/keep")) != "another build" {
				t.Error("unrelated build changed")
			}
			after := sha256.Sum256([]byte(read(t, original)))
			if (before != after) != scenario.corrupt {
				t.Error("writing the copy changed the original")
			}
			if scenario.corrupt != strings.Contains(r.stderr, "Original SD image changed") {
				t.Errorf("checksum guard did not report modification correctly: %s", r.stderr)
			}
			if scenario.target == "zero2-arm64" && !strings.Contains(read(t, filepath.Join(tmp, "lva-check")), "from linux_voice_assistant import util") {
				t.Error("ARM64 did not run the copied-root LVA check")
			}
			if !strings.Contains(read(t, filepath.Join(tmp, "readonly-check")), "['dnsmasq', '--test'") {
				t.Error("image did not check the hotspot DNS configuration with a read-only root")
			}
		})
	}
}
