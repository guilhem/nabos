// Package image checks the NabOS OS image files (image/boot, image/rootfs,
// genimage.cfg, assets).
//
// Run: go test ./tests/image (from services/). Nothing here touches host
// block devices or needs root. The U-Boot tests run the real boot.scr in a
// sandbox build of the pinned U-Boot and are skipped when NABOS_UBOOT_SANDBOX
// (default /tmp/nabos-uboot-sandbox) has no u-boot binary:
//
//	make O=$NABOS_UBOOT_SANDBOX sandbox_defconfig
//	scripts/config --file $NABOS_UBOOT_SANDBOX/.config -d SANDBOX_SDL -d TOOLS_MKEFICAPSULE \
//	    -d UNIT_TEST -d EFI_CAPSULE_AUTHENTICATE -d EFI_CAPSULE_ON_DISK -d CMD_UPL -d UPL
//	make O=$NABOS_UBOOT_SANDBOX olddefconfig
//	make O=$NABOS_UBOOT_SANDBOX CONFIG_PYLIBFDT= u-boot tools
//
// NABOS_SOURCES (default /tmp/nabos-sources) holds the unpacked locked
// sources (uboot, sound, ears); NABOS_VENDOR_DTBS the vendor DTBs.
// image/test.sh supplies NABOS_IMAGE_BOOT, NABOS_IMAGE_ENV, NABOS_IMAGE_TARGET
// and NABOS_IMAGE_OVERLAYS to exercise the shipped files from its disposable copy.
package image

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const MiB = 1 << 20

var (
	repo       = must(filepath.Abs("../../.."))
	imageDir   = filepath.Join(repo, "image")
	bootDir    = filepath.Join(imageDir, "boot")
	rootfsDir  = filepath.Join(imageDir, "rootfs")
	sandbox    = envOr("NABOS_UBOOT_SANDBOX", "/tmp/nabos-uboot-sandbox")
	sources    = envOr("NABOS_SOURCES", "/tmp/nabos-sources")
	vendorDTBs = envOr("NABOS_VENDOR_DTBS", "/tmp/nabos-vendor-dtb")
	dtbs       = map[string]string{"zero-armv6": "bcm2708-rpi-zero-w.dtb", "zero2-arm64": "bcm2710-rpi-zero-2-w.dtb"}
)

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

type result struct {
	stdout, stderr string
	code           int
}

// execute runs a command; env entries are added to the current environment.
func execute(t *testing.T, stdin string, env []string, name string, args ...string) result {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = append(os.Environ(), env...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("%s: %v", name, err)
	}
	return result{out.String(), errOut.String(), cmd.ProcessState.ExitCode()}
}

// run executes a command that must succeed and returns its stdout.
func run(t *testing.T, stdin string, name string, args ...string) string {
	t.Helper()
	r := execute(t, stdin, nil, name, args...)
	if r.code != 0 {
		t.Fatalf("%s %v: exit %d\n%s%s", name, args, r.code, r.stdout, r.stderr)
	}
	return r.stdout
}

func read(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// find returns the sorted paths below dir ending with suffix.
func find(dir, suffix string) []string {
	var found []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, "/"+suffix) {
			found = append(found, p)
		}
		return nil
	})
	sort.Strings(found)
	return found
}

func ubootEnv(t *testing.T) map[string]string {
	env := map[string]string{}
	for _, line := range strings.Split(read(t, filepath.Join(bootDir, "uboot.env")), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "#") {
			env[k] = v
		}
	}
	return env
}

// fakeCommands puts logging stand-ins for system commands first in PATH.
type fakeCommands struct{ bin, log string }

func newFakes(t *testing.T, dir string, scripts map[string]string) fakeCommands {
	f := fakeCommands{filepath.Join(dir, "bin"), filepath.Join(dir, "calls")}
	os.Mkdir(f.bin, 0o755)
	write(t, f.log, "")
	for name, body := range scripts {
		script := "#!/bin/sh\necho \"" + name + " $*\" >> " + f.log + "\n" + body + "\n"
		if err := os.WriteFile(filepath.Join(f.bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f fakeCommands) env(extra ...string) []string {
	return append([]string{"PATH=" + f.bin + ":" + os.Getenv("PATH")}, extra...)
}

func (f fakeCommands) calls(t *testing.T) []string {
	if s := strings.TrimSpace(read(t, f.log)); s != "" {
		return strings.Split(s, "\n")
	}
	return nil
}
