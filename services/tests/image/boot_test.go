package image

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const baseDTS = `/dts-v1/;
/ {
	compatible = "raspberrypi,model-zero-w", "brcm,bcm2835";
	#address-cells = <1>;
	#size-cells = <1>;
	soc {
		#address-cells = <1>;
		#size-cells = <1>;
		gpio: gpio@7e200000 { reg = <0x7e200000 0xb4>; gpio-controller; #gpio-cells = <2>; };
		i2s: i2s@7e203000 { reg = <0x7e203000 0x24>; #sound-dai-cells = <0>; status = "disabled"; };
		i2c1: i2c@7e804000 { reg = <0x7e804000 0x1000>; #address-cells = <1>; #size-cells = <0>; status = "disabled"; };
	};
	sound: sound { status = "disabled"; };
	vdd_5v0_reg: fixedregulator_5v0 { compatible = "regulator-fixed"; };
	vdd_3v3_reg: fixedregulator_3v3 { compatible = "regulator-fixed"; };
};
`

const miniOverlay = `/dts-v1/;
/plugin/;
/ { fragment@0 { target = <&%s>; __overlay__ { status = "okay"; }; }; };
`

// Exercise the actual wdt commands using U-Boot's emulated driver, named
// like the firmware's hardware device on both Raspberry Pis.
const controlDTS = `/dts-v1/;
/ {
    binman {};
    reset { compatible = "sandbox,reset"; };
    watchdog@7e100000 { compatible = "sandbox,wdt"; };
};
`

const (
	bootOffset   = 4 * MiB
	bootHalfSize = 64 * MiB // Smallest FAT32 image mkfs.vfat accepts without a cluster warning.
	rootPartSize = 8 * MiB
)

// bootFixture runs boot.cmd under the pinned U-Boot's own hush parser, with
// a stored environment made only of uboot.env (env import -d wipes
// everything else, like a real stored environment does).
type bootFixture struct {
	t        *testing.T
	tmp      string
	overlays map[string]string
}

func copyFile(t *testing.T, from, to string) {
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *bootFixture) slotTree(name, target string, brokenOverlay bool) string {
	tree := filepath.Join(f.tmp, name)
	os.RemoveAll(tree)
	os.MkdirAll(filepath.Join(tree, "boot/dtb"), 0o755)
	os.MkdirAll(filepath.Join(tree, "boot/overlays"), 0o755)
	write(f.t, filepath.Join(tree, "boot/kernel"), strings.Repeat("not a real kernel", 64))
	dtb := filepath.Join(vendorDTBs, dtbs[target])
	if _, err := os.Stat(dtb); err != nil {
		if os.Getenv("NABOS_IMAGE_BOOT") != "" {
			f.t.Fatal(err)
		}
		dtb = filepath.Join(f.tmp, "base.dtb")
	}
	copyFile(f.t, dtb, filepath.Join(tree, "boot/dtb", dtbs[target]))
	for overlay, path := range f.overlays {
		copyFile(f.t, path, filepath.Join(tree, "boot/overlays", overlay+".dtbo"))
	}
	if brokenOverlay {
		write(f.t, filepath.Join(tree, "boot/overlays/tagtagtag-sound.dtbo"), "garbage")
	}
	return tree
}

// disk reserves two boot halves before fixed p2 A, p3 B and p4 data.
// Only the first boot half appears in the MBR until the test changes p1.
func (f *bootFixture) disk(target string, slotA, slotB, brokenOverlay bool) string {
	t := f.t
	boot := filepath.Join(f.tmp, "p1")
	os.RemoveAll(boot)
	os.Mkdir(boot, 0o755)
	copyFile(t, filepath.Join(f.tmp, "boot.scr"), filepath.Join(boot, "boot.scr"))
	copyFile(t, filepath.Join(f.tmp, "uboot.env.bin"), filepath.Join(boot, "uboot.env"))
	env := strings.NewReplacer("@TARGET@", target, "@DTB@", dtbs[target]).Replace(read(t, filepath.Join(bootDir, "boot.env.in")))
	if boot := os.Getenv("NABOS_IMAGE_BOOT"); boot != "" && target == os.Getenv("NABOS_IMAGE_TARGET") {
		env = read(t, filepath.Join(boot, "boot.env"))
	}
	write(t, filepath.Join(boot, "boot.env"), env)
	parts := []string{"", "", ""}
	if slotA {
		parts[0] = f.slotTree("a", target, brokenOverlay)
	}
	if slotB {
		parts[1] = f.slotTree("b", target, false)
	}
	const firstRoot = bootOffset + 2*bootHalfSize
	disk := filepath.Join(f.tmp, "disk.img")
	os.Remove(disk)
	if err := os.WriteFile(disk, nil, 0o644); err != nil || os.Truncate(disk, firstRoot+3*rootPartSize) != nil {
		t.Fatal("cannot create disk image")
	}
	table := "label: dos\n"
	for i := range 4 {
		kind := "83"
		if i == 0 {
			kind = "c"
		}
		start := firstRoot + (i-1)*rootPartSize
		partSize := rootPartSize
		if i == 0 {
			start = bootOffset
			partSize = bootHalfSize
		}
		table += fmt.Sprintf("start=%d,size=%d,type=%s\n", start/512, partSize/512, kind)
	}
	run(t, table, "sfdisk", "-q", disk)
	out, err := os.OpenFile(disk, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	envImage, err := os.ReadFile(filepath.Join(f.tmp, "uboot.env.bin"))
	if err != nil {
		t.Fatal(err)
	}
	for _, offset := range []int64{MiB, 2 * MiB} {
		if _, err := out.WriteAt(envImage, offset); err != nil {
			t.Fatal(err)
		}
	}
	bootImage := filepath.Join(f.tmp, "boot.fat")
	os.Remove(bootImage)
	if err := os.WriteFile(bootImage, nil, 0o644); err != nil || os.Truncate(bootImage, bootHalfSize) != nil {
		t.Fatal("cannot create FAT image")
	}
	run(t, "", "mkfs.vfat", "-F", "32", "-n", "NABOSBOOT", bootImage)
	bootFiles, err := filepath.Glob(filepath.Join(boot, "*"))
	if err != nil {
		t.Fatal(err)
	}
	copyArgs := append([]string{"-i", bootImage}, bootFiles...)
	run(t, "", "mcopy", append(copyArgs, "::")...)
	bootData, err := os.ReadFile(bootImage)
	if err != nil {
		t.Fatal(err)
	}
	for _, offset := range []int64{bootOffset, bootOffset + bootHalfSize} {
		if _, err := out.WriteAt(bootData, offset); err != nil {
			t.Fatal(err)
		}
	}
	for i, tree := range parts {
		if tree == "" {
			continue
		}
		part := filepath.Join(f.tmp, fmt.Sprintf("part%d.ext4", i+2))
		os.Remove(part)
		run(t, "", "mkfs.ext4", "-q", "-F", "-d", tree, part, fmt.Sprintf("%dk", rootPartSize/1024))
		data, err := os.ReadFile(part)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := out.WriteAt(data, int64(firstRoot+i*rootPartSize)); err != nil {
			t.Fatal(err)
		}
	}
	return disk
}

func (f *bootFixture) boot(disk, envChanges string, watchdog bool) ([]string, string) {
	t := f.t
	// env import -d -c: the CRC-checked redundant binary replaces the whole
	// environment, exactly like a valid stored environment on the card.
	commands := "host bind 0 " + disk + "; " +
		"load host 0:1 0x100000 uboot.env && env import -d -c 0x100000 0x10000; " +
		"setenv devtype host; setenv board_revision 0x9000C1; " +
		envChanges + " load host 0:1 ${scriptaddr} boot.scr; source ${scriptaddr}"
	var args []string
	if watchdog {
		args = []string{"-d", filepath.Join(f.tmp, "control.dtb")}
	}
	cmd := exec.Command(filepath.Join(sandbox, "u-boot"), append(args, "-c", commands)...)
	cmd.Dir = f.tmp // saveenv and sandbox state belong to this fixture.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	timer := time.AfterFunc(60*time.Second, func() { cmd.Process.Kill() })
	// The sandbox relaunches itself on "reset": stop at the second banner.
	var output []string
	banners := 0
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "U-Boot 20") {
			if banners++; banners == 2 {
				output = append(output, "<reset>")
				break
			}
		}
		output = append(output, line)
	}
	cmd.Process.Kill()
	go io.Copy(io.Discard, r)
	cmd.Wait()
	r.Close()
	timer.Stop()
	text := strings.Join(output, "\n")
	for _, bad := range []string{"syntax error", "Unknown command", "Stopping watchdog timer failed"} {
		if strings.Contains(text, bad) {
			t.Fatalf("%q in U-Boot output:\n%s", bad, text)
		}
	}
	var lines []string
	for _, line := range output {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "nabos:") {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		t.Fatalf("no nabos: output:\n%s", text)
	}
	return lines, text
}

func newBootFixture(t *testing.T) *bootFixture {
	if _, err := os.Stat(filepath.Join(sandbox, "u-boot")); err != nil {
		t.Skip("U-Boot sandbox build not available")
	}
	f := &bootFixture{t: t, tmp: t.TempDir(), overlays: map[string]string{}}
	dtc := filepath.Join(sandbox, "scripts/dtc/dtc")
	if _, err := os.Stat(dtc); err != nil {
		dtc = "dtc"
	}
	run(t, baseDTS, dtc, "-@", "-I", "dts", "-O", "dtb", "-o", filepath.Join(f.tmp, "base.dtb"), "-")
	run(t, controlDTS, dtc, "-I", "dts", "-O", "dtb", "-o", filepath.Join(f.tmp, "control.dtb"), "-")
	for name, repo := range map[string]string{"tagtagtag-sound": "sound"} {
		if overlays := os.Getenv("NABOS_IMAGE_OVERLAYS"); overlays != "" {
			f.overlays[name] = filepath.Join(overlays, name+".dtbo")
			continue
		}
		var dts string
		if real := find(filepath.Join(sources, repo), name+"-overlay.dts"); len(real) > 0 {
			// The pinned driver overlays, preprocessed like image/prepare.sh does.
			dts = run(t, "", "cpp", "-nostdinc", "-undef", "-D__DTS__", "-x", "assembler-with-cpp", "-P", real[0])
		} else {
			dts = fmt.Sprintf(miniOverlay, "i2s")
		}
		out := filepath.Join(f.tmp, name+".dtbo")
		run(t, dts, dtc, "-@", "-I", "dts", "-O", "dtb", "-o", out, "-")
		f.overlays[name] = out
	}
	if boot := os.Getenv("NABOS_IMAGE_BOOT"); boot != "" {
		copyFile(t, filepath.Join(boot, "boot.scr"), filepath.Join(f.tmp, "boot.scr"))
		copyFile(t, os.Getenv("NABOS_IMAGE_ENV"), filepath.Join(f.tmp, "uboot.env.bin"))
	} else {
		run(t, "", filepath.Join(sandbox, "tools/mkimage"), "-A", "arm", "-T", "script", "-C", "none",
			"-d", filepath.Join(bootDir, "boot.cmd"), filepath.Join(f.tmp, "boot.scr"))
		// Same packing as image/build.sh; comment lines must be dropped by mkenvimage.
		run(t, "", filepath.Join(sandbox, "tools/mkenvimage"), "-r", "-s", "0x10000",
			"-o", filepath.Join(f.tmp, "uboot.env.bin"), filepath.Join(bootDir, "uboot.env"))
	}
	return f
}

func booting(t *testing.T, lines []string, slot string) string {
	for _, line := range lines {
		if strings.HasPrefix(line, "nabos: booting slot "+slot+":") {
			return line
		}
	}
	t.Fatalf("slot %s never booted: %q", slot, lines)
	return ""
}

func containsAll(t *testing.T, lines []string, want ...string) {
	for _, w := range want {
		if !slices.Contains(lines, w) {
			t.Errorf("missing %q in %q", w, lines)
		}
	}
}

func lower(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = strings.ToLower(l)
	}
	return out
}

func TestBootScript(t *testing.T) {
	targets := []string{"zero-armv6", "zero2-arm64"}
	if target := os.Getenv("NABOS_IMAGE_TARGET"); target != "" {
		targets = []string{target}
	}
	for _, target := range targets {
		t.Run(target, func(t *testing.T) { testBootScript(t, target) })
	}
}

func testBootScript(t *testing.T, target string) {
	f := newBootFixture(t)
	bootFailure := "Unrecognized zImage" // bootz rejects the fixture's dummy kernel.
	if target == "zero2-arm64" {
		bootFailure = "booti_setup"
	}
	boot := func(t *testing.T, slotB, broken bool, env string, watchdog bool) ([]string, string) {
		f.t = t
		return f.boot(f.disk(target, true, slotB, broken), env, watchdog)
	}

	t.Run("stored environment is complete", func(t *testing.T) {
		_, text := boot(t, false, false, "printenv bootcmd bootdelay scriptaddr;", true)
		containsAll(t, strings.Split(text, "\n"), "bootcmd=load mmc 0:1 ${scriptaddr} boot.scr && source ${scriptaddr}",
			"bootdelay=-2", "scriptaddr=0x05400000")
	})

	t.Run("p1 switches between FAT boot halves without moving roots or raw env", func(t *testing.T) {
		f.t = t
		disk := f.disk(target, true, true, false)
		image, err := os.OpenFile(disk, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer image.Close()
		checkLayout := func(activeBoot uint32) {
			t.Helper()
			var mbr [512]byte
			if _, err := image.ReadAt(mbr[:], 0); err != nil {
				t.Fatal(err)
			}
			rootStart := uint32((bootOffset + 2*bootHalfSize) / 512)
			rootSize := uint32(rootPartSize / 512)
			for i, start := range []uint32{activeBoot, rootStart, rootStart + rootSize, rootStart + 2*rootSize} {
				entry := mbr[446+i*16:]
				if got := binary.LittleEndian.Uint32(entry[8:12]); got != start {
					t.Errorf("p%d starts at sector %d, want %d", i+1, got, start)
				}
				wantSize, wantType := rootSize, byte(0x83)
				if i == 0 {
					wantSize, wantType = uint32(bootHalfSize/512), 0x0c
				}
				if binary.LittleEndian.Uint32(entry[12:16]) != wantSize || entry[4] != wantType {
					t.Errorf("p%d size/type = %d/%#x, want %d/%#x", i+1,
						binary.LittleEndian.Uint32(entry[12:16]), entry[4], wantSize, wantType)
				}
			}
		}
		checkEnv := func() {
			t.Helper()
			want, err := os.ReadFile(filepath.Join(f.tmp, "uboot.env.bin"))
			if err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len(want))
			for _, offset := range []int64{MiB, 2 * MiB} {
				if _, err := image.ReadAt(got, offset); err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(got, want) {
					t.Errorf("raw environment differs at %d", offset)
				}
			}
		}
		checkLayout(bootOffset / 512)
		checkEnv()
		lines, _ := f.boot(disk, "", true)
		containsAll(t, strings.Fields(booting(t, lines, "A")), "root=/dev/mmcblk0p2", "rauc.slot=A")
		// The second boot must work even when the first FAT copy is unreadable.
		if _, err := image.WriteAt(make([]byte, 512), bootOffset); err != nil {
			t.Fatal(err)
		}
		// MBR entry 1 starts at byte 446; its start LBA is at byte 454.
		var start [4]byte
		binary.LittleEndian.PutUint32(start[:], uint32((bootOffset+bootHalfSize)/512))
		if _, err := image.WriteAt(start[:], 454); err != nil {
			t.Fatal(err)
		}
		checkLayout(uint32((bootOffset + bootHalfSize) / 512))
		checkEnv()
		lines, _ = f.boot(disk, "setenv BOOT_ORDER 'B A'; setenv BOOT_B_LEFT 1;", true)
		containsAll(t, strings.Fields(booting(t, lines, "B")), "root=/dev/mmcblk0p3", "rauc.slot=B")
		containsAll(t, strings.Fields(booting(t, lines, "A")), "root=/dev/mmcblk0p2", "rauc.slot=A")
	})

	t.Run("first boot uses slot A and never tries empty B", func(t *testing.T) {
		lines, output := boot(t, false, false, "", true)
		if lines[0] != "nabos: trying slot A, 2 attempts left after this one" {
			t.Errorf("first line %q", lines[0])
		}
		containsAll(t, lines, "nabos: overlay tagtagtag-sound applied")
		if strings.Contains(output, "tagtagtag-ears") {
			t.Error("obsolete ears overlay loaded")
		}
		containsAll(t, strings.Fields(booting(t, lines, "A")), "root=/dev/mmcblk0p2", "rauc.slot=A", "ro",
			"init=/usr/lib/nabos/boot-init", "watchdog.open_timeout=300", "panic=10")
		containsAll(t, lower(lines), "nabos: board revision 0x009000c1")
		if !strings.Contains(output, "Started watchdog@7e100000") {
			t.Error("watchdog not started")
		}
		if !strings.Contains(output, bootFailure) {
			t.Errorf("kernel boot command not used for %s", target)
		}
		// Only reached in the sandbox because the kernel boot command returned.
		want := []string{"nabos: slot A did not boot", "nabos: slot B has no attempts left",
			"nabos: no bootable slot, restoring boot attempts"}
		if !slices.Equal(lines[max(0, len(lines)-3):], want) {
			t.Errorf("last lines %q", lines)
		}
	})

	t.Run("primary B without kernel falls through to A in the same boot", func(t *testing.T) {
		lines, _ := boot(t, false, false, "setenv BOOT_ORDER 'B A'; setenv BOOT_B_LEFT 3;", true)
		want := []string{"nabos: trying slot B, 2 attempts left after this one", "nabos: slot B did not boot",
			"nabos: trying slot A, 2 attempts left after this one"}
		if !slices.Equal(lines[:min(3, len(lines))], want) {
			t.Errorf("lines %q", lines)
		}
		containsAll(t, strings.Fields(booting(t, lines, "A")), "root=/dev/mmcblk0p2")
	})

	t.Run("slot B boots from partition 3", func(t *testing.T) {
		lines, output := boot(t, true, false, "setenv BOOT_ORDER 'B A'; setenv BOOT_B_LEFT 1;", true)
		if lines[0] != "nabos: trying slot B, 0 attempts left after this one" {
			t.Errorf("first line %q", lines[0])
		}
		containsAll(t, strings.Fields(booting(t, lines, "B")), "root=/dev/mmcblk0p3", "rauc.slot=B")
		if n := strings.Count(output, "Started watchdog@7e100000"); n != 2 {
			t.Errorf("watchdog started %d times", n)
		}
	})

	t.Run("missing watchdog never hands control to kernel", func(t *testing.T) {
		lines, output := boot(t, false, false, "", false)
		containsAll(t, lines, "nabos: cannot arm watchdog")
		if strings.Contains(output, bootFailure) {
			t.Error("kernel started without watchdog")
		}
	})

	t.Run("exhausted slots restore attempts and reset", func(t *testing.T) {
		lines, output := boot(t, false, false, "setenv BOOT_A_LEFT 0;", true)
		want := []string{"nabos: slot A has no attempts left", "nabos: slot B has no attempts left",
			"nabos: no bootable slot, restoring boot attempts"}
		if !slices.Equal(lines, want) || !strings.Contains(output, "<reset>") {
			t.Errorf("lines %q", lines)
		}
	})

	t.Run("missing A/B state defaults to A", func(t *testing.T) {
		lines, _ := boot(t, false, false, "env delete BOOT_ORDER BOOT_A_LEFT BOOT_B_LEFT;", true)
		if lines[0] != "nabos: trying slot A, 2 attempts left after this one" {
			t.Errorf("first line %q", lines[0])
		}
		containsAll(t, lines, "nabos: slot B has no attempts left")
	})

	t.Run("broken overlay boots the plain DTB", func(t *testing.T) {
		lines, _ := boot(t, false, true, "", true)
		containsAll(t, lines, "nabos: overlay failed, using the plain DTB")
		booting(t, lines, "A")
	})
}
