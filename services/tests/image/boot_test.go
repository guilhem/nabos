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
    aliases { mmc0 = &mmc; };
    config {
        u-boot,mmc-env-offset = <0x100000>;
        u-boot,mmc-env-offset-redundant = <0x200000>;
    };
    mmc: mmc { compatible = "sandbox,mmc"; filename = "disk.img"; };
    bootstd { compatible = "u-boot,boot-std"; filename-prefixes = "/", "/boot/"; };
    binman {};
    reset { compatible = "sandbox,reset"; };
    watchdog@7e100000 { compatible = "sandbox,wdt"; };
};
`

const (
	bootOffset   = 4 * MiB
	bootHalfSize = 16 * MiB // Compact FAT16 fixture; production geometry is checked separately.
	rootPartSize = 8 * MiB
)

// bootFixture exercises native RAUC and redundant MMC environments, without
// importing an environment through a second persistence backend.
type bootFixture struct {
	t               *testing.T
	tmp             string
	overlays        map[string]string
	scripts         map[string]string // Optional slot script overrides (compiled as legacy images).
	corrupt         map[string][]string
	kernel          []byte
	missing         map[string][]string
	initContents    map[string]string
	recovery        bool
	recoveryMissing []string
	shadow          bool
	failEnv         bool
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
	os.MkdirAll(filepath.Join(tree, "usr/share/nabos"), 0o755)
	write(f.t, filepath.Join(tree, "usr/share/nabos/ca.cert.pem"), "public image authority\n")
	os.MkdirAll(filepath.Join(tree, "boot/dtb"), 0o755)
	os.MkdirAll(filepath.Join(tree, "boot/overlays"), 0o755)
	kernel := make([]byte, 4096)
	if target == "zero-armv6" {
		binary.LittleEndian.PutUint32(kernel[0x24:], 0x016f2818)
		binary.LittleEndian.PutUint32(kernel[0x2c:], uint32(len(kernel)))
	} else {
		binary.LittleEndian.PutUint32(kernel[0x38:], 0x644d5241)
		binary.LittleEndian.PutUint64(kernel[0x10:], uint64(len(kernel)))
	}
	if err := os.WriteFile(filepath.Join(tree, "boot/kernel"), kernel, 0o644); err != nil {
		f.t.Fatal(err)
	}
	copyFile(f.t, filepath.Join(f.tmp, "slot.scr"), filepath.Join(tree, "boot/boot.scr"))
	if f.shadow {
		path := filepath.Join(f.tmp, "shadow.cmd")
		write(f.t, path, "echo nabos: SHADOW SCRIPT\n")
		for _, name := range []string{"boot.scr", "boot.scr.uimg"} {
			run(f.t, "", filepath.Join(sandbox, "bin/mkimage"), "-A", "arm", "-T", "script", "-C", "none", "-d", path, filepath.Join(tree, name))
		}
	}
	if contents, ok := f.scripts[name]; ok {
		path := filepath.Join(f.tmp, name+".cmd")
		write(f.t, path, contents)
		run(f.t, "", filepath.Join(sandbox, "bin/mkimage"), "-A", "arm", "-T", "script", "-C", "none", "-d", path, filepath.Join(tree, "boot/boot.scr"))
	}
	if f.kernel != nil {
		if err := os.WriteFile(filepath.Join(tree, "boot/kernel"), f.kernel, 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	write(f.t, filepath.Join(tree, "boot/initrd"), strings.Repeat("not a real initrd", 64))
	write(f.t, filepath.Join(tree, "boot/init"), "nabos_init=/nix/store/fixture-"+name+"/init\nnabos_kernel_params=ro rootwait rootfstype=ext4 fsck.mode=skip watchdog.open_timeout=300 panic=10 slot_fixture="+name+"\n")
	if init, override := f.initContents[name]; override {
		write(f.t, filepath.Join(tree, "boot/init"), init)
	}
	dtb := filepath.Join(vendorDTBs, dtbs[target])
	if _, err := os.Stat(dtb); err != nil {
		if actualImage() || os.Getenv("NABOS_VENDOR_DTBS") != "" {
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
	for _, file := range f.corrupt[name] {
		path := filepath.Join(tree, "boot", file)
		if file == "boot.scr" {
			data := []byte(read(f.t, path))
			data[len(data)-1] ^= 1
			if err := os.WriteFile(path, data, 0o644); err != nil {
				f.t.Fatal(err)
			}
		} else {
			write(f.t, path, "corrupt")
		}
	}
	for _, missing := range f.missing[name] {
		os.Remove(filepath.Join(tree, "boot", missing))
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
	if f.recovery {
		os.Mkdir(filepath.Join(boot, "recovery"), 0755)
		write(t, filepath.Join(boot, "recovery/kernel"), "broken recovery kernel")
		write(t, filepath.Join(boot, "recovery/initrd"), "dummy RAM recovery")
		copyFile(t, filepath.Join(f.tmp, "base.dtb"), filepath.Join(boot, "recovery", dtbs[target]))
		for _, missing := range f.recoveryMissing {
			os.Remove(filepath.Join(boot, "recovery", missing))
		}
	}
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
	run(t, "", "mkfs.vfat", "-F", "16", "-n", "NABOSBOOT", bootImage)
	bootFiles, err := filepath.Glob(filepath.Join(boot, "*"))
	if err != nil {
		t.Fatal(err)
	}
	copyArgs := append([]string{"-s", "-i", bootImage}, bootFiles...)
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
	if disk != filepath.Join(f.tmp, "disk.img") {
		f.t.Fatal("control DT must name the fixture card")
	}
	commands := "nabos_env_test prepare; setenv board_revision 0x9000C1; " + envChanges
	if f.failEnv {
		commands += " nabos_env_test fail;"
	}
	commands += " load mmc 0:1 ${scriptaddr} boot.scr && source ${scriptaddr}"
	return f.sandbox(commands, watchdog)
}

func (f *bootFixture) sandbox(commands string, watchdog bool) ([]string, string) {
	t := f.t
	dtb := "control.dtb"
	if !watchdog {
		dtb = "no-watchdog.dtb"
	}
	args := []string{"-d", filepath.Join(f.tmp, dtb)}
	cmd := exec.Command(filepath.Join(sandbox, "bin/u-boot"), append(args, "-c", commands)...)
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
	return lines, text
}

func newBootFixture(t *testing.T) *bootFixture {
	checkImageInputs(t)
	if _, err := os.Stat(filepath.Join(sandbox, "bin/u-boot")); err != nil {
		if _, set := os.LookupEnv("NABOS_UBOOT_SANDBOX"); set || actualImage() {
			t.Fatal(err)
		}
		t.Skip("Nix U-Boot sandbox not available")
	}
	f := &bootFixture{t: t, tmp: t.TempDir(), overlays: map[string]string{}}
	dtc := filepath.Join(sandbox, "bin/dtc")
	if _, err := os.Stat(dtc); err != nil {
		t.Fatal(err)
	}
	run(t, baseDTS, dtc, "-@", "-I", "dts", "-O", "dtb", "-o", filepath.Join(f.tmp, "base.dtb"), "-")
	run(t, controlDTS, dtc, "-I", "dts", "-O", "dtb", "-o", filepath.Join(f.tmp, "control.dtb"), "-")
	run(t, strings.ReplaceAll(controlDTS, `    watchdog@7e100000 { compatible = "sandbox,wdt"; };`, ""), dtc, "-I", "dts", "-O", "dtb", "-o", filepath.Join(f.tmp, "no-watchdog.dtb"), "-")
	for name, repo := range map[string]string{"tagtagtag-sound": "sound"} {
		if overlays := os.Getenv("NABOS_IMAGE_OVERLAYS"); overlays != "" {
			f.overlays[name] = filepath.Join(overlays, name+".dtbo")
			continue
		}
		var dts string
		if real := find(filepath.Join(sources, repo), name+"-overlay.dts"); len(real) > 0 {
			// Compile pinned overlays only for standalone source tests.
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
		run(t, "", filepath.Join(sandbox, "bin/mkimage"), "-A", "arm", "-T", "script", "-C", "none",
			"-d", filepath.Join(bootDir, "boot.cmd"), filepath.Join(f.tmp, "boot.scr"))
		write(t, filepath.Join(f.tmp, "uboot.env.txt"), read(t, filepath.Join(bootDir, "uboot.env"))+read(t, filepath.Join(bootDir, "layout.env")))
		// Same redundant packing as the Nix image payload.
		run(t, "", filepath.Join(sandbox, "bin/mkenvimage"), "-r", "-s", "0x10000",
			"-o", filepath.Join(f.tmp, "uboot.env.bin"), filepath.Join(f.tmp, "uboot.env.txt"))
	}
	run(t, "", filepath.Join(sandbox, "bin/mkimage"), "-A", "arm", "-T", "script", "-C", "none",
		"-d", filepath.Join(bootDir, "slot.cmd"), filepath.Join(f.tmp, "slot.scr"))
	// A script image has a 64-byte legacy header, then one size and a zero terminator.
	// Compare the shipped payload too, so source-only tests cannot hide a rewritten boot.cmd.
	script := []byte(read(t, filepath.Join(f.tmp, "boot.scr")))
	if len(script) < 72 || binary.BigEndian.Uint32(script[:4]) != 0x27051956 || script[30] != 6 ||
		binary.BigEndian.Uint32(script[64:68]) != uint32(len(script)-72) || binary.BigEndian.Uint32(script[68:72]) != 0 ||
		string(script[72:]) != read(t, filepath.Join(bootDir, "boot.cmd")) {
		t.Fatal("boot.scr must contain exactly the authoritative boot.cmd")
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

func (f *bootFixture) state(want ...string) {
	f.t.Helper()
	_, output := f.sandbox("printenv BOOT_ORDER BOOT_A_LEFT BOOT_B_LEFT NABOS_RESET_SLOT nabos_recovery_action", true)
	for _, value := range want {
		if !strings.Contains(output, value+"\n") {
			f.t.Errorf("missing durable %s:\n%s", value, output)
		}
	}
	if strings.Contains(output, "nabos_recovery_action=rollback") || strings.Contains(output, "nabos_recovery_action=reset") {
		f.t.Fatal("transient action persisted", output)
	}
}

func noBoot(t *testing.T, output string, slots ...string) {
	t.Helper()
	for _, slot := range slots {
		if strings.Contains(output, "nabos: booting slot "+slot+":") {
			t.Fatalf("unexpected slot %s handoff:\n%s", slot, output)
		}
	}
	if !strings.Contains(output, "<reset>") {
		t.Fatalf("failure did not reboot:\n%s", output)
	}
}

func bootTargets(t *testing.T, test func(*testing.T, string)) {
	for _, target := range []string{"zero-armv6", "zero2-arm64"} {
		if actual := os.Getenv("NABOS_IMAGE_TARGET"); actual != "" && target != actual {
			continue
		}
		t.Run(target, func(t *testing.T) { test(t, target) })
	}
}

func TestBootScript(t *testing.T) {
	bootTargets(t, func(t *testing.T, target string) {
		t.Run("native handoff and stored environment", func(t *testing.T) {
			f := newBootFixture(t)
			lines, output := f.boot(f.disk(target, true, false, false), "printenv bootcmd bootdelay scriptaddr;", true)
			containsAll(t, strings.Fields(booting(t, lines, "A")), "root=/dev/mmcblk0p2", "rauc.slot=A", "ro", "init=/nix/store/fixture-a/init", "slot_fixture=a", "watchdog.open_timeout=300", "panic=10")
			containsAll(t, lower(lines), "nabos: board revision 0x009000c1")
			containsAll(t, lines, "nabos: overlay tagtagtag-sound applied", "nabos: bootflow returned, rebooting")
			for _, required := range []string{"INFO: Booting slot A, 3 of 3 tries left", "bootcmd=load mmc 0:1 ${scriptaddr} boot.scr && source ${scriptaddr}", "bootdelay=-2", "scriptaddr=0x05400000", "Started watchdog@7e100000"} {
				if !strings.Contains(output, required) {
					t.Errorf("missing %s:\n%s", required, output)
				}
			}
			if target == "zero-armv6" && !strings.Contains(output, "sandbox: continuing, as we cannot run Linux") {
				t.Fatal("initrd/FDT handoff did not finish", output)
			}
			noBoot(t, output, "B")
			f.state("BOOT_A_LEFT=2", "BOOT_B_LEFT=0")
		})
		for _, primary := range []string{"A", "B"} {
			fallback := "B"
			if primary == "B" {
				fallback = "A"
			}
			for _, failure := range []string{"empty", "missing script", "corrupt script", "oversized script", "returning script"} {
				t.Run(primary+"/"+failure+" consumes attempts across restart", func(t *testing.T) {
					f := newBootFixture(t)
					name := strings.ToLower(primary)
					a, b := true, true
					switch failure {
					case "empty":
						a, b = primary != "A", primary != "B"
					case "missing script":
						f.missing = map[string][]string{name: {"boot.scr"}}
					case "corrupt script":
						f.corrupt = map[string][]string{name: {"boot.scr"}}
					case "oversized script":
						f.scripts = map[string]string{name: "#" + strings.Repeat("x", 65536) + "\n"}
					case "returning script":
						f.scripts = map[string]string{name: "echo nabos: returning script\n"}
					}
					disk := f.disk(target, a, b, false)
					for attempt := 3; attempt > 0; attempt-- {
						env := ""
						if attempt == 3 {
							env = fmt.Sprintf("setenv BOOT_ORDER '%s %s'; setenv BOOT_A_LEFT 3; setenv BOOT_B_LEFT 3;", primary, fallback)
						}
						_, output := f.boot(disk, env, true)
						noBoot(t, output, primary, fallback)
						f.state(fmt.Sprintf("BOOT_%s_LEFT=%d", primary, attempt-1))
					}
					lines, output := f.boot(disk, "", true)
					booting(t, lines, fallback)
					noBoot(t, output, primary)
				})
			}
		}
		t.Run("all exhausted counters restore natively", func(t *testing.T) {
			f := newBootFixture(t)
			disk := f.disk(target, true, true, false)
			_, output := f.boot(disk, "setenv BOOT_A_LEFT 0; setenv BOOT_B_LEFT 0;", true)
			noBoot(t, output, "A", "B")
			if !strings.Contains(output, "Resetting all slot tries to 3") {
				t.Fatal(output)
			}
			f.state("BOOT_A_LEFT=3", "BOOT_B_LEFT=3")
			lines, _ := f.boot(disk, "", true)
			booting(t, lines, "A")
			f.state("BOOT_A_LEFT=2", "BOOT_B_LEFT=3")
		})
		t.Run("missing state gets native defaults", func(t *testing.T) {
			f := newBootFixture(t)
			lines, _ := f.boot(f.disk(target, true, true, false), "env delete BOOT_ORDER BOOT_A_LEFT BOOT_B_LEFT;", true)
			booting(t, lines, "A")
			f.state("BOOT_ORDER=A B", "BOOT_A_LEFT=2", "BOOT_B_LEFT=3")
		})
		for _, missing := range []string{"init", "initrd", "kernel", "dtb/" + dtbs[target]} {
			t.Run("shared loader rejects missing "+missing, func(t *testing.T) {
				f := newBootFixture(t)
				f.missing = map[string][]string{"b": {missing}}
				_, output := f.boot(f.disk(target, true, true, false), "setenv BOOT_ORDER 'B A'; setenv BOOT_B_LEFT 3;", true)
				noBoot(t, output, "A", "B")
				f.state("BOOT_A_LEFT=3", "BOOT_B_LEFT=2")
			})
		}
		for _, file := range []string{"kernel", "dtb/" + dtbs[target]} {
			t.Run("shared loader rejects corrupt "+file, func(t *testing.T) {
				f := newBootFixture(t)
				f.corrupt = map[string][]string{"b": {file}}
				_, output := f.boot(f.disk(target, true, true, false), "setenv BOOT_ORDER 'B A'; setenv BOOT_B_LEFT 3;", true)
				noBoot(t, output, "A", "B")
			})
		}
		for _, metadata := range []string{"", "nabos_init=/nix/store/fixture-b/init\n", "nabos_kernel_params=ro\n"} {
			t.Run("metadata cannot reuse stored values/"+metadata, func(t *testing.T) {
				f := newBootFixture(t)
				f.initContents = map[string]string{"b": metadata}
				_, output := f.boot(f.disk(target, true, true, false), "setenv BOOT_ORDER 'B A'; setenv BOOT_B_LEFT 1; setenv nabos_init /nix/store/stale/init; setenv nabos_kernel_params stale=1;", true)
				noBoot(t, output, "A", "B")
			})
		}
		t.Run("broken overlay uses plain DTB", func(t *testing.T) {
			f := newBootFixture(t)
			lines, _ := f.boot(f.disk(target, true, true, true), "", true)
			containsAll(t, lines, "nabos: overlay failed, using the plain DTB")
			booting(t, lines, "A")
		})
		t.Run("missing watchdog blocks handoff", func(t *testing.T) {
			f := newBootFixture(t)
			lines, output := f.boot(f.disk(target, true, true, false), "", false)
			containsAll(t, lines, "nabos: cannot arm watchdog")
			if strings.Contains(output, "sandbox: continuing, as we cannot run Linux") {
				t.Fatal(output)
			}
		})
		for _, order := range []string{"C A", "A C", "A A", "B B", "A B C"} {
			t.Run("invalid order/"+order, func(t *testing.T) {
				f := newBootFixture(t)
				lines, output := f.boot(f.disk(target, true, true, false), "setenv BOOT_ORDER '"+order+"';", true)
				containsAll(t, lines, "nabos: invalid boot order, normal boot blocked")
				noBoot(t, output, "A", "B")
				f.state("BOOT_ORDER=A B", "BOOT_A_LEFT=3", "BOOT_B_LEFT=0")
			})
		}
		t.Run("native save failure never starts script or consumes durable attempt", func(t *testing.T) {
			f := newBootFixture(t)
			f.failEnv = true
			_, output := f.boot(f.disk(target, true, true, false), "", true)
			noBoot(t, output, "A", "B")
			f.state("BOOT_A_LEFT=3", "BOOT_B_LEFT=0")
		})
	})
}

func TestPhysicalRecoveryBootScript(t *testing.T) {
	bootTargets(t, func(t *testing.T, target string) {
		for _, preferred := range []string{"A", "B"} {
			previous := "B"
			if preferred == "B" {
				previous = "A"
			}
			for _, failure := range []string{"", "missing history", "stale history", "invalid history", "empty slot", "missing script", "corrupt script", "oversized script", "missing kernel", "missing initrd", "invalid metadata", "invalid kernel", "invalid DTB"} {
				t.Run(preferred+"/"+failure, func(t *testing.T) {
					f := newBootFixture(t)
					history, name := previous, strings.ToLower(previous)
					a, b := true, true
					switch failure {
					case "missing history":
						history = ""
					case "stale history":
						history = preferred
					case "invalid history":
						history = "C"
					case "empty slot":
						a, b = previous != "A", previous != "B"
					case "missing script":
						f.missing = map[string][]string{name: {"boot.scr"}}
					case "corrupt script":
						f.corrupt = map[string][]string{name: {"boot.scr"}}
					case "oversized script":
						f.scripts = map[string]string{name: "#" + strings.Repeat("x", 65536) + "\n"}
					case "missing kernel":
						f.missing = map[string][]string{name: {"kernel"}}
					case "missing initrd":
						f.missing = map[string][]string{name: {"initrd"}}
					case "invalid metadata":
						f.initContents = map[string]string{name: "corrupt"}
					case "invalid kernel":
						f.corrupt = map[string][]string{name: {"kernel"}}
					case "invalid DTB":
						f.corrupt = map[string][]string{name: {"dtb/" + dtbs[target]}}
					}
					env := fmt.Sprintf("setenv BOOT_ORDER '%s %s'; setenv BOOT_%s_LEFT 3; setenv NABOS_ROLLBACK_SLOT %s; setenv nabos_recovery_action rollback;", preferred, previous, preferred, history)
					lines, _ := f.boot(f.disk(target, a, b, false), env, true)
					if failure == "" {
						containsAll(t, lines, "nabos: rollback selected slot "+previous)
						booting(t, lines, previous)
						f.state("BOOT_ORDER="+previous+" "+preferred, "BOOT_"+previous+"_LEFT=2")
					} else {
						containsAll(t, lines, "nabos: rollback cancelled, no different loadable recorded slot")
						booting(t, lines, preferred)
						f.state("BOOT_ORDER="+preferred+" "+previous, "BOOT_"+preferred+"_LEFT=2")
					}
				})
			}
		}
		for _, action := range []string{"reset", "rollback"} {
			t.Run("environment write failure/"+action, func(t *testing.T) {
				f := newBootFixture(t)
				f.failEnv = true
				lines, output := f.boot(f.disk(target, true, true, false), "setenv NABOS_ROLLBACK_SLOT B; setenv nabos_recovery_action "+action+";", true)
				if action == "reset" {
					containsAll(t, lines, "nabos: reset authorization failed, normal boot blocked")
				} else {
					containsAll(t, lines, "nabos: rollback persistence failed")
				}
				noBoot(t, output, "A", "B")
				f.state("BOOT_ORDER=A B", "BOOT_A_LEFT=3", "BOOT_B_LEFT=0")
			})
		}
		for _, tc := range []struct {
			name, env, pending string
			missing            []string
		}{
			{"new reset", "setenv nabos_recovery_action reset;", "A", nil},
			{"pending precedence", "setenv NABOS_RESET_SLOT B; setenv BOOT_ORDER 'C A'; setenv nabos_recovery_action rollback;", "B", nil},
			{"missing rescue kernel", "setenv NABOS_RESET_SLOT A;", "A", []string{"kernel"}},
			{"missing rescue initrd", "setenv NABOS_RESET_SLOT A;", "A", []string{"initrd"}},
			{"invalid pending", "setenv NABOS_RESET_SLOT C;", "C", nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f := newBootFixture(t)
				f.recovery, f.recoveryMissing = true, tc.missing
				f.missing = map[string][]string{"a": {"kernel", "initrd"}, "b": {"kernel", "initrd"}}
				lines, output := f.boot(f.disk(target, true, true, false), tc.env, true)
				noBoot(t, output, "A", "B")
				if strings.Contains(output, "INFO: Booting slot") || strings.Contains(output, "invalid boot order") {
					t.Fatal("reset changed normal slot state", output)
				}
				if tc.name == "invalid pending" {
					containsAll(t, lines, "nabos: invalid pending reset, normal boot blocked")
				} else {
					containsAll(t, lines, "nabos: recovery failed, normal boot blocked")
					if len(tc.missing) == 0 {
						containsAll(t, lines, "nabos: starting independent recovery for slot "+tc.pending)
					}
				}
				f.state("BOOT_A_LEFT=3", "BOOT_B_LEFT=0")
				if tc.name == "new reset" {
					f.state("NABOS_RESET_SLOT=A")
				}
			})
		}
	})
}

func TestSelectedSlotBootContract(t *testing.T) {
	script := read(t, filepath.Join(bootDir, "boot.cmd"))
	slot := read(t, filepath.Join(bootDir, "slot.cmd"))
	for _, file := range []string{"boot.scr", "init", "initrd", "kernel", "dtb/${nabos_dtb}"} {
		if !strings.Contains(script, "${devnum}:${nabos_part} ") || !strings.Contains(script, "/boot/"+file) {
			t.Errorf("missing shared selected-slot load for %s", file)
		}
	}
	for _, required := range []string{"${distro_rootpart}", "${raucargs}", "run nabos_load", "nabos_led off", "wdt start 15999", "wdt stop", "booti ${kernel_addr_r} ${nabos_ramdisk} ${fdt_addr_r}", "bootz ${kernel_addr_r} ${nabos_ramdisk} ${fdt_addr_r}"} {
		if !strings.Contains(slot, required) {
			t.Errorf("missing native handoff contract %s", required)
		}
	}
	if strings.Contains(script+slot, "Debian") || strings.Contains(script+slot, "/usr/lib/nabos/boot-init") {
		t.Error("obsolete boot contract retained")
	}
	if !strings.Contains(script, "bootmeth order rauc") || !strings.Contains(script, "setenv boot_targets mmc0") || !strings.Contains(script, "bootflow scan -b;") {
		t.Error("automatic boot must be restricted to SD RAUC")
	}
	payload := read(t, filepath.Join(repo, "nix/image.nix"))
	if !strings.Contains(payload, `slotScript = pkgs.writeText "nabos-slot.cmd" (builtins.readFile ../image/boot/slot.cmd);`) || !strings.Contains(payload, "files/boot/boot.scr") {
		t.Error("payload must compile and ship the authoritative slot.cmd")
	}
}

func TestNativeBootSafety(t *testing.T) {
	f := newBootFixture(t)
	t.Run("root scripts cannot shadow the validated boot directory", func(t *testing.T) {
		f.t, f.shadow = t, true
		lines, output := f.boot(f.disk("zero-armv6", true, true, false), "", true)
		booting(t, lines, "A")
		if strings.Contains(output, "SHADOW SCRIPT") {
			t.Fatal(output)
		}
		f.shadow = false
	})
	t.Run("stale saved gesture is cleared before dispatch", func(t *testing.T) {
		f.t = t
		disk := f.disk("zero-armv6", true, true, false)
		f.sandbox("setenv NABOS_ROLLBACK_SLOT B; setenv nabos_recovery_action rollback; saveenv", true)
		lines, output := f.boot(disk, "", true)
		booting(t, lines, "A")
		if strings.Contains(output, "rollback selected") {
			t.Fatal(output)
		}
		f.state("BOOT_ORDER=A B", "BOOT_A_LEFT=2", "BOOT_B_LEFT=0")
	})
	t.Run("shared gesture selfcheck", func(t *testing.T) {
		f.t = t
		_, output := f.sandbox("nabos_recovery_test", true)
		if !strings.Contains(output, "PASS: stale actions overwritten; pending reset preserved") {
			t.Fatal(output)
		}
	})
}

func TestBootMBRSwitch(t *testing.T) {
	bootTargets(t, func(t *testing.T, target string) {
		f := newBootFixture(t)
		disk := f.disk(target, true, true, false)
		image, err := os.OpenFile(disk, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer image.Close()
		var before, after [512]byte
		if _, err := image.ReadAt(before[:], 0); err != nil {
			t.Fatal(err)
		}
		// Destroy the first FAT, then activate only the redundant FAT via p1.
		if _, err := image.WriteAt(make([]byte, 512), bootOffset); err != nil {
			t.Fatal(err)
		}
		var start [4]byte
		binary.LittleEndian.PutUint32(start[:], uint32((bootOffset+bootHalfSize)/512))
		if _, err := image.WriteAt(start[:], 454); err != nil {
			t.Fatal(err)
		}
		if _, err := image.ReadAt(after[:], 0); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(before[:454], after[:454]) || !slices.Equal(before[458:], after[458:]) {
			t.Fatal("FAT switch changed other MBR bytes")
		}
		lines, output := f.boot(disk, "setenv BOOT_ORDER 'B A'; setenv BOOT_B_LEFT 1;", true)
		containsAll(t, strings.Fields(booting(t, lines, "B")), "root=/dev/mmcblk0p3", "rauc.slot=B")
		noBoot(t, output, "A")
		f.state("BOOT_ORDER=B A", "BOOT_A_LEFT=3", "BOOT_B_LEFT=0")
	})
}
