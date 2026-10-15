package image

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

type recoveryFixture struct {
	dir, library, disk string
	fake               fakeCommands
}

func newRecoveryFixture(t *testing.T) recoveryFixture {
	t.Helper()
	f := recoveryFixture{dir: t.TempDir()}
	f.disk = filepath.Join(f.dir, "mmcblk0")
	f.library = filepath.Join(f.dir, "recovery.sh")
	for _, p := range []string{"work", "proc/self", "sys/mmcblk0/queue", "sys/mmcblk0p4/holders"} {
		if err := os.MkdirAll(filepath.Join(f.dir, p), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(f.dir, "proc/self/mountinfo"), "1 0 0:1 / / rw - rootfs rootfs rw\n")
	write(t, filepath.Join(f.dir, "sys/mmcblk0/size"), "28319744\n")
	write(t, filepath.Join(f.dir, "sys/mmcblk0/dev"), "179:0\n")
	write(t, filepath.Join(f.dir, "sys/mmcblk0/queue/logical_block_size"), "512\n")
	table := "label: dos\nlabel-id: 0x12345678\ndevice: " + f.disk + "\nunit: sectors\nsector-size: 512\n\n"
	write(t, f.disk, "fixture disk; never a host device\n")
	for n, g := range [][2]int{{8192, 524288}, {1056768, 12582912}, {13639680, 12582912}, {26222592, 2097152}} {
		part := fmt.Sprintf("mmcblk0p%d", n+1)
		sys := filepath.Join(f.dir, "sys", part)
		if err := os.MkdirAll(sys, 0700); err != nil {
			t.Fatal(err)
		}
		for key, value := range map[string]int{"start": g[0], "size": g[1], "partition": n + 1} {
			write(t, filepath.Join(sys, key), fmt.Sprintf("%d\n", value))
		}
		write(t, filepath.Join(sys, "dev"), fmt.Sprintf("179:%d\n", n+1))
		write(t, filepath.Join(f.dir, part), "unreadable old filesystem\n")
		kind, bootable := "83", ""
		if n == 0 {
			kind, bootable = "c", ", bootable"
		}
		table += fmt.Sprintf("%s : start=%d, size=%d, type=%s%s\n", filepath.Join(f.dir, part), g[0], g[1], kind, bootable)
	}
	write(t, filepath.Join(f.dir, "table"), table)
	write(t, filepath.Join(f.dir, "fw_env.config"), read(t, filepath.Join(rootfsDir, "etc/fw_env.config")))
	write(t, filepath.Join(f.dir, "env"), "BOOT_ORDER=B A\nBOOT_A_LEFT=0\nBOOT_B_LEFT=1\nNABOS_RESET_SLOT=A\nnabos_recovery_action=reset\nKEEP=unchanged\n")
	// Tests only redirect an explicitly checked regular fixture file. No block
	// device, loop device, host mount, or host environment writer is ever used.
	library := strings.ReplaceAll(read(t, filepath.Join(repo, "nix/runtime/recovery.sh")), "/bin/nabos-recovery-led", filepath.Join(f.dir, "bin/nabos-recovery-led"))
	write(t, f.library, `. "`+filepath.Join(imageDir, "layout.sh")+`"
`+library+`
RECOVERY_DISK="$FIXTURE/mmcblk0"
RECOVERY_SYS="$FIXTURE/sys"
RECOVERY_PROC="$FIXTURE/proc"
RECOVERY_WORK="$FIXTURE/work"
RECOVERY_ENV_CONFIG="$FIXTURE/fw_env.config"
recovery_block_device() {
    case $1 in "$FIXTURE"/mmcblk0|"$FIXTURE"/mmcblk0p[1-4]) [ -f "$1" ] ;; *) return 1 ;; esac
}
`)
	f.fake = newFakes(t, f.dir, map[string]string{
		"nabos-recovery-led": `case $1 in red|redblink) exit 0 ;; *) exit 90 ;; esac`,
		"sfdisk": `[ "$1" = --dump ] && [ "$2" = "$FIXTURE/mmcblk0" ] || exit 90
cat "$FIXTURE/table"`,
		"fw_printenv": `[ "$1 $2" = "-c $FIXTURE/fw_env.config" ] || exit 90
[ "${FAIL_STAGE:-}" != read-initial ] || { cat "$FIXTURE/env"; exit 1; }
[ "${FAIL_STAGE:-}" != read-defaults ] || { cat "$FIXTURE/env"; echo 'Warning: Bad CRC, using default environment' >&2; exit 0; }
if [ -f "$FIXTURE/completed" ] && [ ! -f "$FIXTURE/read-failed" ]; then
    case ${FAIL_STAGE:-} in verify-read|verify-wrong)
        touch "$FIXTURE/read-failed"
        if [ "$FAIL_STAGE" = verify-read ]; then exit 1; fi
        sed 's/BOOT_ORDER=A B/BOOT_ORDER=B A/' "$FIXTURE/env"; exit 0 ;;
    esac
fi
[ "${FAIL_STAGE:-}" != reauthorize-unreadable ] || [ ! -f "$FIXTURE/completed" ] || exit 1
cat "$FIXTURE/env"`,
		"fw_setenv": `[ "$1 $2 $3" = "-c $FIXTURE/fw_env.config -s" ] || exit 90
case $4 in
"$FIXTURE/work/complete")
    touch "$FIXTURE/completed"
    [ "${FAIL_STAGE:-}" != write-before ] || exit 1 ;;
"$FIXTURE/work/reauthorize")
    [ "${FAIL_STAGE:-}" != reauthorize-fails ] || exit 1 ;;
*) exit 90 ;;
esac
awk 'NR==FNR { k=$1; v=$0; sub(/^[^ \t]*[ \t]*/, "", v); update[k]=v; next }
     { split($0,p,"="); if (!(p[1] in update)) print }
     END { for(k in update) if(update[k]!="") print k "=" update[k] }' \
    "$4" "$FIXTURE/env" > "$FIXTURE/env.next" && mv "$FIXTURE/env.next" "$FIXTURE/env" || exit 1
case ${FAIL_STAGE:-}:$4 in
    write-after:"$FIXTURE/work/complete"|reauthorize-fails:"$FIXTURE/work/complete") exit 1 ;;
    write-warning:"$FIXTURE/work/complete"|reauthorize-warning:"$FIXTURE/work/complete"|reauthorize-warning:"$FIXTURE/work/reauthorize") echo "fsync failed on SD environment" >&2; exit 0 ;;
esac`,
		"mkfs.ext4": `[ "$*" = "-F -L nabos-data -E lazy_itable_init=0,lazy_journal_init=0 $FIXTURE/mmcblk0p4" ] || exit 90
if [ "${FAIL_STAGE:-}" = format ]; then
    echo 'interrupted filesystem construction' > "$FIXTURE/mmcblk0p4"; exit 1
fi
[ "${SLOW_FORMAT:-}" != yes ] || sleep 3
echo 'fresh filesystem' > "$FIXTURE/mmcblk0p4"`,
		"e2fsck": `[ "$*" = "-fn $FIXTURE/mmcblk0p4" ] && [ "${FAIL_STAGE:-}" != fsck ]`,
		"blkid": `[ "$1 $2 $4 $5 $6" = "-p -s -o value $FIXTURE/mmcblk0p4" ] || exit 90
case $3 in
TYPE) [ "${FAIL_STAGE:-}" != fs-type ] || { echo ext2; exit 0; }; echo ext4 ;;
LABEL) [ "${FAIL_STAGE:-}" != fs-label ] || { echo wrong-data; exit 0; }; echo nabos-data ;;
*) exit 90 ;;
esac`,
		"sync":   `[ "${FAIL_STAGE:-}" != sync ] && { [ "${FAIL_STAGE:-}" != sync-after ] || [ ! -f "$FIXTURE/completed" ]; }`,
		"reboot": `exit 0`,
	})
	return f
}

func (f recoveryFixture) reset(t *testing.T, extra ...string) result {
	t.Helper()
	return execute(t, "", f.fake.env(append(extra, "FIXTURE="+f.dir)...), "sh", "-c", `. "$1"; recovery_reset`, "recovery-test", f.library)
}

func (f recoveryFixture) env(t *testing.T) map[string]string {
	t.Helper()
	values := map[string]string{}
	for _, line := range strings.Split(read(t, filepath.Join(f.dir, "env")), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			values[key] = value
		}
	}
	return values
}

func recoveryCalled(calls []string, command string) bool {
	return slices.ContainsFunc(calls, func(call string) bool { return strings.HasPrefix(call, command+" ") })
}

func TestRecoveryDeviceWaitIsBounded(t *testing.T) {
	f := newRecoveryFixture(t)
	path := filepath.Join(f.fake.bin, "sleep")
	write(t, path, "#!/bin/sh\necho \"sleep $*\" >> \"$FIXTURE/calls\"\n")
	if err := os.Chmod(path, 0755); err != nil {
		t.Fatal(err)
	}
	script := `. "$1"; . "$2"; RECOVERY_DISK="$FIXTURE/mmcblk0"; recovery_wait_devices`
	// The production guard must reject the regular fixture as a block device.
	r := execute(t, "", f.fake.env("FIXTURE="+f.dir), "sh", "-c", script, "wait-test", filepath.Join(imageDir, "layout.sh"), filepath.Join(repo, "nix/runtime/recovery.sh"))
	calls := f.fake.calls(t)
	if r.code == 0 || len(calls) != 100 {
		t.Fatal("unbounded or unsafe device wait", r, calls)
	}
	for _, call := range calls {
		if call != "sleep 0.1" {
			t.Fatal("unexpected operation before device ready", call)
		}
	}
}

func TestRecoveryAuthorization(t *testing.T) {
	for _, pending := range []string{"", "NABOS_RESET_SLOT=\n", "NABOS_RESET_SLOT=C\n", "NABOS_RESET_SLOT=A B\n", "NABOS_RESET_SLOT=A\nNABOS_RESET_SLOT=B\n"} {
		t.Run(fmt.Sprintf("reject-%q", pending), func(t *testing.T) {
			f := newRecoveryFixture(t)
			write(t, filepath.Join(f.dir, "env"), "BOOT_A_LEFT=3\n"+pending)
			if r := f.reset(t); r.code == 0 {
				t.Fatal("invalid authorization accepted")
			}
			calls := f.fake.calls(t)
			if recoveryCalled(calls, "mkfs.ext4") || recoveryCalled(calls, "fw_setenv") {
				t.Fatal(calls)
			}
		})
	}
	for _, stage := range []string{"read-initial", "read-defaults"} {
		t.Run("CRC failure never accepts printed defaults-"+stage, func(t *testing.T) {
			f := newRecoveryFixture(t)
			if r := f.reset(t, "FAIL_STAGE="+stage); r.code == 0 || recoveryCalled(f.fake.calls(t), "mkfs.ext4") {
				t.Fatal(r, f.fake.calls(t))
			}
		})
	}
	for _, slot := range []string{"A", "B"} {
		t.Run(slot, func(t *testing.T) {
			f := newRecoveryFixture(t)
			write(t, filepath.Join(f.dir, "env"), "BOOT_ORDER=A B\nBOOT_A_LEFT=0\nBOOT_B_LEFT=1\nNABOS_RESET_SLOT="+slot+"\nnabos_recovery_action=reset\nKEEP=unchanged\n")
			if r := f.reset(t); r.code != 0 {
				t.Fatal(r.stderr)
			}
			got := f.env(t)
			order, other, left := "A B", "B", "1"
			if slot == "B" {
				order, other, left = "B A", "A", "0"
			}
			if got["BOOT_"+slot+"_LEFT"] != "3" || got["BOOT_ORDER"] != order || got["BOOT_"+other+"_LEFT"] != left || got["KEEP"] != "unchanged" {
				t.Fatal(got)
			}
			for _, key := range []string{"NABOS_RESET_SLOT", "nabos_recovery_action"} {
				if _, ok := got[key]; ok {
					t.Fatal("pending key retained", got)
				}
			}
			calls := f.fake.calls(t)
			if count := strings.Count(strings.Join(calls, "\n"), "fw_setenv "); count != 1 {
				t.Fatal("completion must be one transaction", calls)
			}
			if !strings.HasPrefix(calls[len(calls)-1], "fw_printenv ") {
				t.Fatal("completion was not read back", calls)
			}
			// Running a recovery image again without a newly authorized reset cannot format.
			if r := f.reset(t); r.code == 0 {
				t.Fatal("completed authorization reused")
			}
		})
	}
}

func TestRecoveryGeometry(t *testing.T) {
	changes := map[string][3]string{
		"gpt":           {"table", "label: dos", "label: gpt"},
		"4k-sector":     {"table", "sector-size: 512", "sector-size: 4096"},
		"wrong-unit":    {"table", "unit: sectors", "unit: bytes"},
		"boot-type":     {"table", "type=c", "type=83"},
		"root-type":     {"table", "type=83", "type=c"},
		"boot-start":    {"table", "start=8192", "start=1"},
		"boot-size":     {"table", "size=524288", "size=524289"},
		"root-start":    {"table", "start=1056768", "start=1056769"},
		"root-size":     {"table", "size=12582912", "size=12582911"},
		"data-start":    {"table", "start=26222592", "start=26222593"},
		"data-small":    {"table", "size=2097152", "size=2097151"},
		"outside-disk":  {"sys/mmcblk0/size", "28319744", "28319743"},
		"kernel-start":  {"sys/mmcblk0p4/start", "26222592", "26222593"},
		"kernel-size":   {"sys/mmcblk0p4/size", "2097152", "2097153"},
		"kernel-number": {"sys/mmcblk0p4/partition", "4", "3"},
		"kernel-device": {"sys/mmcblk0p4/dev", "179:4", "179:3"},
		"kernel-sector": {"sys/mmcblk0/queue/logical_block_size", "512", "4096"},
		"bind-mount":    {"proc/self/mountinfo", "0:1", "179:4"},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			f := newRecoveryFixture(t)
			path := filepath.Join(f.dir, change[0])
			write(t, path, strings.ReplaceAll(read(t, path), change[1], change[2]))
			if r := f.reset(t); r.code == 0 || recoveryCalled(f.fake.calls(t), "mkfs.ext4") || recoveryCalled(f.fake.calls(t), "fw_setenv") {
				t.Fatal("unsafe geometry accepted", r, f.fake.calls(t))
			}
		})
	}
	for _, failure := range []string{"extra-entry", "missing-entry", "holder", "missing-sysfs"} {
		t.Run(failure, func(t *testing.T) {
			f := newRecoveryFixture(t)
			switch failure {
			case "extra-entry":
				write(t, filepath.Join(f.dir, "table"), read(t, filepath.Join(f.dir, "table"))+f.disk+"p5 : start=1, size=1, type=83\n")
			case "missing-entry":
				write(t, filepath.Join(f.dir, "table"), strings.ReplaceAll(read(t, filepath.Join(f.dir, "table")), f.disk+"p4 : start=26222592, size=2097152, type=83\n", ""))
			case "holder":
				write(t, filepath.Join(f.dir, "sys/mmcblk0p4/holders/dm-0"), "")
			case "missing-sysfs":
				if err := os.Remove(filepath.Join(f.dir, "sys/mmcblk0p4/start")); err != nil {
					t.Fatal(err)
				}
			}
			if r := f.reset(t); r.code == 0 || recoveryCalled(f.fake.calls(t), "mkfs.ext4") {
				t.Fatal(r, f.fake.calls(t))
			}
		})
	}
	t.Run("switched FAT and grown data accepted", func(t *testing.T) {
		f := newRecoveryFixture(t)
		table := strings.NewReplacer("start=8192", "start=532480", "size=2097152", "size=4194304").Replace(read(t, filepath.Join(f.dir, "table")))
		write(t, filepath.Join(f.dir, "table"), table)
		write(t, filepath.Join(f.dir, "sys/mmcblk0p1/start"), "532480\n")
		write(t, filepath.Join(f.dir, "sys/mmcblk0p4/size"), "4194304\n")
		write(t, filepath.Join(f.dir, "sys/mmcblk0/size"), "30416896\n")
		if r := f.reset(t); r.code != 0 {
			t.Fatal(r.stderr)
		}
	})
}

func TestRecoveryFailureAndResumption(t *testing.T) {
	for _, stage := range []string{"format", "fsck", "fs-type", "fs-label", "sync", "sync-after", "write-before", "write-after", "write-warning", "reauthorize-warning", "verify-read", "verify-wrong", "reauthorize-fails", "reauthorize-unreadable"} {
		t.Run(stage, func(t *testing.T) {
			f := newRecoveryFixture(t)
			if r := f.reset(t, "FAIL_STAGE="+stage); r.code == 0 {
				t.Fatal("failure permitted normal boot", stage)
			}
			values := f.env(t)
			if stage != "reauthorize-fails" && values["NABOS_RESET_SLOT"] != "A" {
				t.Fatal("pending authorization lost", values)
			}
			calls := f.fake.calls(t)
			if stage == "format" || stage == "fsck" || stage == "fs-type" || stage == "fs-label" || stage == "sync" {
				if recoveryCalled(calls, "fw_setenv") {
					t.Fatal("environment changed before filesystem verified", calls)
				}
			} else if !slices.Contains(calls, "fw_setenv -c "+filepath.Join(f.dir, "fw_env.config")+" -s "+filepath.Join(f.dir, "work/reauthorize")) {
				t.Fatal("uncertain final write was not corrected", calls)
			}
			if recoveryCalled(calls, "fw_setenv") && stage != "format" && stage != "fsck" && stage != "fs-type" && stage != "fs-label" && stage != "sync" {
				if got := read(t, filepath.Join(f.dir, "work/reauthorize")); got != "NABOS_RESET_SLOT A\n" {
					t.Fatal("correction must persist only the pending slot", got)
				}
			}
			if stage != "reauthorize-fails" {
				if r := f.reset(t); r.code != 0 {
					t.Fatal("reset could not resume after interruption", r.stderr)
				}
			}
		})
	}
}

func TestRecoveryInitFeedsWatchdogAndBlocks(t *testing.T) {
	for _, stage := range []string{"", "verify-read", "reauthorize-fails"} {
		t.Run("final-stage-"+stage, func(t *testing.T) {
			f := newRecoveryFixture(t)
			dev := filepath.Join(f.dir, "dev")
			if err := os.Mkdir(dev, 0700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"console", "watchdog0"} {
				write(t, filepath.Join(dev, name), "")
			}
			for name, body := range map[string]string{"mount": "exit 0", "modprobe": "exit 0"} {
				write(t, filepath.Join(f.fake.bin, name), "#!/bin/sh\n"+body+"\n")
				if err := os.Chmod(filepath.Join(f.fake.bin, name), 0755); err != nil {
					t.Fatal(err)
				}
			}
			init := read(t, filepath.Join(repo, "nix/runtime/recovery-init.sh"))
			init = strings.NewReplacer("PATH=/bin:/sbin", "PATH=\""+f.fake.bin+":"+os.Getenv("PATH")+"\"",
				"/lib/nabos-layout.sh", filepath.Join(imageDir, "layout.sh"),
				"/lib/nabos-recovery.sh", f.library, "-c /dev/watchdog0", "-f "+filepath.Join(dev, "watchdog0"),
				"/bin/nabos-recovery-led", filepath.Join(f.fake.bin, "nabos-recovery-led"),
				"/dev", dev, "/proc", filepath.Join(f.dir, "proc"), "/sys", filepath.Join(f.dir, "sys"), "/run", filepath.Join(f.dir, "run")).Replace(init)
			path := filepath.Join(f.dir, "init.sh")
			write(t, path, init)
			cmd := exec.Command("sh", path)
			cmd.Env = append(os.Environ(), f.fake.env("FIXTURE="+f.dir, "SLOW_FORMAT=yes", "FAIL_STAGE="+stage)...)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); cmd.Wait() })
			deadline := time.Now().Add(8 * time.Second)
			for time.Now().Before(deadline) {
				console := read(t, filepath.Join(dev, "console"))
				if strings.Contains(console, "normal boot blocked") {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			console := read(t, filepath.Join(dev, "console"))
			if !strings.Contains(console, "normal boot blocked") {
				t.Fatal("init did not reach blocked state", console, init, f.fake.calls(t))
			}
			pings := read(t, filepath.Join(dev, "watchdog0"))
			if len(pings) < 3 {
				t.Fatal("watchdog was not fed during long format", pings, console)
			}
			rebooted := recoveryCalled(f.fake.calls(t), "reboot")
			if rebooted != (stage == "") {
				t.Fatal("uncertain completion rebooted", console, f.fake.calls(t))
			}
			calls := f.fake.calls(t)
			redAt := slices.Index(calls, "nabos-recovery-led red")
			formatAt := slices.IndexFunc(calls, func(call string) bool { return strings.HasPrefix(call, "mkfs.ext4 ") })
			if redAt < 0 || formatAt < 0 || redAt > formatAt {
				t.Fatal("reset was not shown in red before formatting", calls)
			}
			before := len(pings)
			time.Sleep(1100 * time.Millisecond)
			if len(read(t, filepath.Join(dev, "watchdog0"))) <= before {
				t.Fatal("watchdog stopped in blocked state")
			}
			if count := strings.Count(strings.Join(f.fake.calls(t), "\n"), "nabos-recovery-led redblink"); count < 2 {
				t.Fatal("failure blinking was not repeated", f.fake.calls(t))
			}
		})
	}
}

func TestRecoverySparseDiskKeepsBootAndRoots(t *testing.T) {
	for _, tool := range []string{"sfdisk", "mkfs.ext4", "e2fsck", "blkid", "dd", "openssl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " unavailable")
		}
	}
	f := newRecoveryFixture(t)
	disk, err := os.OpenFile(f.disk, os.O_RDWR|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	const dataStart = int64(26222592) * 512
	if err := disk.Truncate(dataStart + int64(1024)*MiB); err != nil {
		t.Fatal(err)
	}
	run(t, read(t, filepath.Join(f.dir, "table")), "sfdisk", f.disk)
	for _, offset := range []int64{4 * MiB, 260 * MiB, 516 * MiB, 6660 * MiB, dataStart - 4096} {
		if _, err := disk.WriteAt([]byte("boot/root sentinel"), offset); err != nil {
			t.Fatal(err)
		}
	}
	cert := filepath.Join(f.dir, "image-ca.pem")
	raCert(t, filepath.Join(f.dir, "image-ca.key"), cert)
	certificate := read(t, cert)
	// Opaque certificate bytes in each root region, independent of p4's old trust.
	certificateOffsets := []int64{516*MiB + 4096, 6660*MiB + 4096}
	for _, offset := range certificateOffsets {
		if _, err := disk.WriteAt([]byte(certificate), offset); err != nil {
			t.Fatal(err)
		}
	}
	hashPrefix := func() [32]byte {
		t.Helper()
		h := sha256.New()
		if _, err := io.Copy(h, io.NewSectionReader(disk, 0, dataStart)); err != nil {
			t.Fatal(err)
		}
		var sum [32]byte
		copy(sum[:], h.Sum(nil))
		return sum
	}
	before := hashPrefix()
	// Translate only the fixture partition into an offset in its owned disk.
	// No root privileges or loop/mount operations are needed for this check.
	realMkfs := must(exec.LookPath("mkfs.ext4"))
	realSfdisk := must(exec.LookPath("sfdisk"))
	realFsck := must(exec.LookPath("e2fsck"))
	realBlkid := must(exec.LookPath("blkid"))
	scripts := map[string]string{
		"sfdisk": `exec "` + realSfdisk + `" "$@"`,
		"mkfs.ext4": `[ "$*" = "-F -L nabos-data -E lazy_itable_init=0,lazy_journal_init=0 $FIXTURE/mmcblk0p4" ] || exit 90
"` + realMkfs + `" -F -b 4096 -L nabos-data -E offset=13425967104,lazy_itable_init=0,lazy_journal_init=0 "$FIXTURE/mmcblk0" 262144 || exit 1
dd if="$FIXTURE/mmcblk0" of="$FIXTURE/mmcblk0p4" bs=1M skip=12804 count=1024 conv=sparse status=none`,
		"e2fsck": `exec "` + realFsck + `" "$@"`,
		"blkid":  `exec "` + realBlkid + `" "$@"`,
	}
	for name, script := range scripts {
		write(t, filepath.Join(f.fake.bin, name), "#!/bin/sh\necho \""+name+" $*\" >> \"$FIXTURE/calls\"\n"+script+"\n")
	}
	// The old p4 deliberately has no readable filesystem or saved certificate.
	if r := f.reset(t); r.code != 0 {
		t.Fatal(r.stdout, r.stderr)
	}
	if after := hashPrefix(); after != before {
		t.Fatal("format changed MBR, environments, FAT, or root regions")
	}
	for _, offset := range certificateOffsets {
		got := make([]byte, len(certificate))
		if _, err := disk.ReadAt(got, offset); err != nil || string(got) != certificate {
			t.Fatalf("immutable certificate bytes changed at %d: %v", offset, err)
		}
	}
	if label := run(t, "", realBlkid, "-p", "-s", "LABEL", "-o", "value", f.disk+"p4"); label != "nabos-data\n" {
		t.Fatal(label)
	}
}
