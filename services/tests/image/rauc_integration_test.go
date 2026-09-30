package image

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestRaucBootMBRIntegration needs a disposable loop device in a private CI namespace.
// It exercises the real RAUC 1.11.3 MBR handler, not an on-device boot.
func TestRaucBootMBRIntegration(t *testing.T) {
	if os.Getenv("NABOS_RAUC_INTEGRATION") != "1" {
		t.Skip("set NABOS_RAUC_INTEGRATION=1 for the privileged loop-device test")
	}
	if os.Geteuid() != 0 {
		t.Fatal("NABOS_RAUC_INTEGRATION=1 requires root")
	}
	for _, name := range []string{"rauc", "genimage", "losetup", "udevadm", "dbus-daemon", "dbus-send", "mkenvimage", "fw_printenv", "fw_setenv", "mkfs.vfat", "mkfs.ext4", "mcopy", "openssl", "sync"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Fatal(err)
		}
	}
	if version := run(t, "", "rauc", "--version"); !strings.Contains(version, "1.11.3") {
		t.Fatalf("this test requires RAUC 1.11.3, got %q", version)
	}
	dir := t.TempDir()
	images := filepath.Join(dir, "images")
	for _, name := range []string{images, filepath.Join(dir, "empty"), filepath.Join(dir, "genimage-tmp"), filepath.Join(dir, "data"), filepath.Join(dir, "mount"), filepath.Join(dir, "bin")} {
		if err := os.Mkdir(name, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rootA := filepath.Join(images, "rootfs.ext4")
	data := filepath.Join(images, "data.ext4")
	bootA := filepath.Join(images, "boot.vfat")
	raFile(t, rootA, 16*MiB)
	raFile(t, data, 16*MiB)
	raFile(t, bootA, 256*MiB)
	run(t, "", "mkfs.ext4", "-q", "-F", "-L", "nabos-root", rootA)
	settingsDir := filepath.Join(dir, "data-fixture", "nabos")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(settingsDir, "settings.json"), "{\"rauc_integration_sentinel\":true}\n")
	run(t, "", "mkfs.ext4", "-q", "-F", "-L", "nabos-data", "-d", filepath.Join(dir, "data-fixture"), data)
	raBoot(t, bootA, "initial")
	run(t, "", "mkenvimage", "-r", "-s", "0x10000", "-o", filepath.Join(images, "uboot.env"), filepath.Join(bootDir, "uboot.env"))
	run(t, "", "genimage", "--config", filepath.Join(imageDir, "genimage.cfg"), "--rootpath", filepath.Join(dir, "empty"), "--inputpath", images, "--outputpath", images, "--tmppath", filepath.Join(dir, "genimage-tmp"))
	disk := filepath.Join(images, "sdcard.img")
	loop := strings.TrimSpace(run(t, "", "losetup", "--find", "--show", "--partscan", disk))
	if !strings.HasPrefix(loop, "/dev/loop") {
		t.Fatalf("unexpected disposable loop device %q", loop)
	}
	t.Cleanup(func() {
		if r := execute(t, "", nil, "losetup", "--detach", loop); r.code != 0 {
			t.Errorf("detach %s: %s", loop, r.stderr)
		}
	})
	run(t, "", "udevadm", "settle", "--timeout=10")
	// Host udev can remove/recreate /dev loop nodes while probing the changed MBR.
	// Private nodes address the same kernel partitions throughout this test.
	slotDevices := make(map[string]string)
	for _, n := range []string{"p2", "p3"} {
		var stat syscall.Stat_t
		if err := syscall.Stat(loop+n, &stat); err != nil {
			t.Fatalf("loop partition %s: %v", loop+n, err)
		}
		node := filepath.Join(dir, "slot-"+n)
		if err := syscall.Mknod(node, syscall.S_IFBLK|0o600, int(stat.Rdev)); err != nil {
			t.Fatal(err)
		}
		slotDevices[n] = node
	}

	config := read(t, filepath.Join(rootfsDir, "etc/rauc/system.conf"))
	config = strings.ReplaceAll(config, "@COMPATIBLE@", "nabos-rauc-integration")
	config = strings.ReplaceAll(config, "/dev/mmcblk0p2", slotDevices["p2"])
	config = strings.ReplaceAll(config, "/dev/mmcblk0p3", slotDevices["p3"])
	config = strings.ReplaceAll(config, "/dev/mmcblk0", loop)
	config = strings.ReplaceAll(config, "/run/rauc", filepath.Join(dir, "mount"))
	config = strings.ReplaceAll(config, "/data/rauc", filepath.Join(dir, "data"))
	config = strings.ReplaceAll(config, "/etc/rauc/ca.cert.pem", filepath.Join(dir, "trusted.crt"))
	config = strings.ReplaceAll(config, "/usr/lib/nabos/rauc-post-install", filepath.Join(dir, "post-install"))
	conf := filepath.Join(dir, "system.conf")
	write(t, conf, config)
	post := strings.ReplaceAll(read(t, filepath.Join(rootfsDir, "usr/lib/nabos/rauc-post-install")), "/dev/mmcblk0", loop)
	raExecutable(t, filepath.Join(dir, "post-install"), post)
	fwConf := filepath.Join(dir, "fw_env.config")
	write(t, fwConf, fmt.Sprintf("%s 0x100000 0x10000\n%s 0x200000 0x10000\n", loop, loop))
	bin := filepath.Join(dir, "bin")
	for _, name := range []string{"fw_printenv", "fw_setenv"} {
		actual, _ := exec.LookPath(name)
		raExecutable(t, filepath.Join(bin, name), fmt.Sprintf("#!/bin/sh\nexec %q -c %q \"$@\"\n", actual, fwConf))
	}
	syncBin, _ := exec.LookPath("sync")
	failSync := filepath.Join(dir, "fail-sync")
	syncReached := filepath.Join(dir, "sync-reached")
	raExecutable(t, filepath.Join(bin, "sync"), fmt.Sprintf("#!/bin/sh\nif [ -e %q ]; then : > %q; exit 42; fi\nexec %q \"$@\"\n", failSync, syncReached, syncBin))
	pathEnv := "PATH=" + bin + ":" + os.Getenv("PATH")

	key := filepath.Join(dir, "trusted.key")
	cert := filepath.Join(dir, "trusted.crt")
	raCert(t, key, cert)
	badKey := filepath.Join(dir, "untrusted.key")
	badCert := filepath.Join(dir, "untrusted.crt")
	raCert(t, badKey, badCert)
	rootB := filepath.Join(dir, "root-b.ext4")
	raFile(t, rootB, 16*MiB)
	run(t, "", "mkfs.ext4", "-q", "-F", "-L", "nabos-next", rootB)
	bootB := filepath.Join(dir, "boot-b.vfat")
	raFile(t, bootB, 256*MiB)
	raBoot(t, bootB, "next")
	bootC := filepath.Join(dir, "boot-c.vfat")
	raFile(t, bootC, 256*MiB)
	raBoot(t, bootC, "return")
	bundleB := raBundle(t, dir, "b", "nabos-rauc-integration", rootB, bootB, key, cert)
	bundleA := raBundle(t, dir, "a", "nabos-rauc-integration", rootA, bootC, key, cert)
	badCompatible := raBundle(t, dir, "wrong", "wrong-compatible", rootB, bootB, key, cert)
	untrusted := raBundle(t, dir, "untrusted", "nabos-rauc-integration", rootB, bootB, badKey, badCert)

	bus := filepath.Join(dir, "bus")
	busLog := filepath.Join(dir, "bus.log")
	raStart(t, busLog, "dbus-daemon", []string{"--session", "--nofork", "--address=unix:path=" + bus}, nil)
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(bus); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(bus); err != nil {
		t.Fatalf("private D-Bus did not start: %v; %s", err, read(t, busLog))
	}
	busEnv := "DBUS_SYSTEM_BUS_ADDRESS=unix:path=" + bus
	serviceLog := filepath.Join(dir, "rauc.log")
	startService := func(slot string) func() {
		stop := raStart(t, serviceLog, "rauc", []string{"--conf=" + conf, "--override-boot-slot=" + slot, "service"}, []string{busEnv, pathEnv})
		for i := 0; i < 100; i++ {
			r := execute(t, "", []string{busEnv}, "dbus-send", "--system", "--print-reply", "--dest=org.freedesktop.DBus", "/", "org.freedesktop.DBus.NameHasOwner", "string:de.pengutronix.rauc")
			if r.code == 0 && strings.Contains(r.stdout, "boolean true") {
				return stop
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("RAUC service did not acquire private bus: %s", read(t, serviceLog))
		return nil
	}
	install := func(bundle string, wantSuccess bool) result {
		t.Helper()
		r := execute(t, "", []string{busEnv, pathEnv}, "rauc", "--conf="+conf, "install", bundle)
		if (r.code == 0) != wantSuccess {
			t.Fatalf("rauc install %s: exit %d\n%s%s\nservice:\n%s", bundle, r.code, r.stdout, r.stderr, read(t, serviceLog))
		}
		return r
	}
	assertEnv := func(order, active string) {
		t.Helper()
		// exec.Command resolves PATH before applying cmd.Env; call our wrapper
		// explicitly so this assertion never reads the host's fw_env.config.
		r := execute(t, "", nil, filepath.Join(bin, "fw_printenv"))
		if r.code != 0 || !strings.Contains(r.stdout, "BOOT_ORDER="+order) || !strings.Contains(r.stdout, "BOOT_"+active+"_LEFT=3") {
			t.Fatalf("stored redundant U-Boot environment: exit %d\n%s%s", r.code, r.stdout, r.stderr)
		}
	}
	_, parts := genimagePartitions(t)
	boot0 := int64(sizes(t, parts["boot"]["offset"]))
	boot1 := int64(sizes(t, parts["boot-redund"]["offset"]))
	root0 := int64(sizes(t, parts["rootfs-a"]["offset"]))
	root1 := int64(sizes(t, parts["rootfs-b"]["offset"]))
	dataOff := int64(sizes(t, parts["data"]["offset"]))
	bootSize := int64(256 * MiB)
	rootSize := int64(16 * MiB)
	initialMBR := raMBR(t, disk)
	if got := raP1Start(initialMBR); got != boot0 {
		t.Fatalf("initial p1 start %d, want %d", got, boot0)
	}
	dataHash := raRangeHash(t, disk, dataOff, rootSize)
	activeRootA := raRangeHash(t, disk, root0, rootSize)
	initialBoot := raRangeHash(t, disk, boot0, bootSize)
	if initialBoot != raRangeHash(t, disk, boot1, bootSize) {
		t.Fatal("genimage did not prefill both boot copies")
	}
	assertEnv("A B", "A")
	stopA := startService("A")
	install(bundleB, true)
	raOrder(t, read(t, serviceLog), "rootfs.1", "bootloader.0")
	raLayout(t, disk, initialMBR, boot1, root0, root1, dataOff, rootSize, bootSize, activeRootA, raFileHash(t, rootB), dataHash, initialBoot, raFileHash(t, bootB))
	assertEnv("B A", "B")
	stopA()
	stopB := startService("B")
	install(bundleA, true)
	raOrder(t, read(t, serviceLog), "rootfs.0", "bootloader.0")
	raLayout(t, disk, initialMBR, boot0, root1, root0, dataOff, rootSize, bootSize, raFileHash(t, rootB), raFileHash(t, rootA), dataHash, raFileHash(t, bootB), raFileHash(t, bootC))
	assertEnv("A B", "A")
	t.Log("RAUC A -> B -> A: both MBR switches, root images, boot copies and /data verified")

	// These fail during bundle verification, before any slot or environment write.
	before := raFileHash(t, disk)
	install(badCompatible, false)
	if after := raFileHash(t, disk); after != before {
		t.Fatal("incompatible signed bundle changed the disk")
	}
	install(untrusted, false)
	if after := raFileHash(t, disk); after != before {
		t.Fatal("untrusted bundle changed the disk")
	}
	oversized := filepath.Join(dir, "oversized.vfat")
	raFile(t, oversized, 257*MiB)
	r := execute(t, "", []string{busEnv, pathEnv}, "rauc", "--conf="+conf, "write-slot", "bootloader.0", oversized)
	if r.code == 0 || !strings.Contains(r.stdout+r.stderr+read(t, serviceLog), "does not fit") {
		t.Fatalf("oversized boot image was not rejected by boot-mbr-switch: %d %s%s", r.code, r.stdout, r.stderr)
	}
	if after := raFileHash(t, disk); after != before {
		t.Fatal("oversized boot image changed the disk")
	}
	t.Log("Wrong target, untrusted signature and oversized boot rejected without changing the disk")
	stopB()
	stopA = startService("A")
	write(t, failSync, "fail")
	r = install(bundleB, false)
	if _, err := os.Stat(syncReached); err != nil {
		t.Fatalf("post-install sync failure path was not reached: %v; %s", err, read(t, serviceLog))
	}
	if !strings.Contains(r.stdout+r.stderr+read(t, serviceLog), "Post-install handler error:") {
		t.Fatalf("sync failure did not propagate through RAUC post-install: %s%s\n%s", r.stdout, r.stderr, read(t, serviceLog))
	}
	t.Log("Post-install sync failure propagated by RAUC")
	stopA()
}

func raFile(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func raBoot(t *testing.T, path, marker string) {
	t.Helper()
	run(t, "", "mkfs.vfat", "-F", "32", "-n", "NABOSBOOT", path)
	mark := filepath.Join(t.TempDir(), "nabos-marker")
	// Empty filesystems compress below verity's minimum squashfs size (4 KiB).
	content := make([]byte, 16*1024)
	if _, err := rand.Read(content); err != nil {
		t.Fatal(err)
	}
	write(t, mark, marker+"\n"+string(content))
	run(t, "", "mcopy", "-i", path, mark, "::marker")
}

func raExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func raCert(t *testing.T, key, cert string) {
	t.Helper()
	run(t, "", "openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-subj", "/CN=NabOS RAUC integration/", "-keyout", key, "-out", cert)
}

func raBundle(t *testing.T, dir, name, compatible, root, boot, key, cert string) string {
	t.Helper()
	input := filepath.Join(dir, "bundle-"+name)
	if err := os.Mkdir(input, 0o755); err != nil {
		t.Fatal(err)
	}
	for target, source := range map[string]string{"rootfs.ext4": root, "boot.vfat": boot} {
		if err := os.Link(source, filepath.Join(input, target)); err != nil {
			t.Fatal(err)
		}
	}
	manifest := read(t, filepath.Join(imageDir, "manifest.raucm.in"))
	manifest = strings.ReplaceAll(manifest, "@COMPATIBLE@", compatible)
	manifest = strings.ReplaceAll(manifest, "@VERSION@", name)
	write(t, filepath.Join(input, "manifest.raucm"), manifest)
	bundle := filepath.Join(dir, name+".raucb")
	run(t, "", "rauc", "bundle", "--mksquashfs-args=-comp zstd -processors 2", "--cert="+cert, "--key="+key, input, bundle)
	return bundle
}

func raStart(t *testing.T, logPath, name string, args, env []string) func() {
	t.Helper()
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{}, 1)
		go func() { _ = cmd.Wait(); done <- struct{}{} }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Errorf("%s did not exit after SIGKILL", name)
			}
		}
		_ = log.Close()
	}
	t.Cleanup(stop)
	return stop
}

func raMBR(t *testing.T, disk string) []byte {
	t.Helper()
	f, err := os.Open(disk)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := make([]byte, 512)
	if _, err := io.ReadFull(f, b); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b[510:], []byte{0x55, 0xaa}) {
		t.Fatal("invalid MBR signature")
	}
	return b
}

func raP1Start(mbr []byte) int64 {
	return int64(binary.LittleEndian.Uint32(mbr[454:458])) * 512
}

func raRangeHash(t *testing.T, path string, offset, length int64) [32]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyN(h, io.NewSectionReader(f, offset, length), length); err != nil {
		t.Fatal(err)
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

func raFileHash(t *testing.T, path string) [32]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

func raOrder(t *testing.T, log, root, boot string) {
	t.Helper()
	a := strings.LastIndex(log, "Updating slot "+root)
	b := strings.LastIndex(log, "Updating slot "+boot)
	if a < 0 || b < a {
		t.Fatalf("RAUC did not write %s before %s; service log:\n%s", root, boot, log)
	}
}

func raLayout(t *testing.T, disk string, initialMBR []byte, bootStart, activeRoot, updatedRoot, dataStart, rootSize, bootSize int64, activeRootHash, updatedRootHash, dataHash, oldBootHash, newBootHash [32]byte) {
	t.Helper()
	mbr := raMBR(t, disk)
	if raP1Start(mbr) != bootStart || !bytes.Equal(mbr[:446], initialMBR[:446]) || !bytes.Equal(mbr[462:], initialMBR[462:]) ||
		mbr[446] != initialMBR[446] || mbr[450] != initialMBR[450] || !bytes.Equal(mbr[458:462], initialMBR[458:462]) {
		t.Fatalf("unexpected MBR change; p1 start %d, want %d", raP1Start(mbr), bootStart)
	}
	for _, check := range []struct {
		name   string
		offset int64
		length int64
		want   [32]byte
	}{
		{"active root", activeRoot, rootSize, activeRootHash},
		{"updated root", updatedRoot, rootSize, updatedRootHash},
		{"data", dataStart, rootSize, dataHash},
		{"active boot", bootStart, bootSize, newBootHash},
		{"previous boot", 264*MiB - bootStart, bootSize, oldBootHash},
	} {
		if got := raRangeHash(t, disk, check.offset, check.length); got != check.want {
			t.Errorf("%s at offset %d changed unexpectedly: got %x, want %x", check.name, check.offset, got, check.want)
		}
	}
}
