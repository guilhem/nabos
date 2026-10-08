// Package image checks NabOS NixOS image inputs and shared runtime policies.
// Run go test ./tests/image from services/. U-Boot fixtures use the native
// .#uboot-sandbox package (NABOS_UBOOT_SANDBOX), installed under bin/.
// NABOS_SOURCES optionally supplies standalone sound/U-Boot sources.
// Artifact tests supply NABOS_IMAGE_BOOT, NABOS_IMAGE_ENV, NABOS_IMAGE_TARGET,
// NABOS_IMAGE_OVERLAYS and NABOS_VENDOR_DTBS; missing explicit inputs fail.
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

func actualImage() bool {
	for _, key := range []string{"NABOS_IMAGE_BOOT", "NABOS_IMAGE_ENV", "NABOS_IMAGE_TARGET", "NABOS_IMAGE_OVERLAYS", "NABOS_IMAGE_DISK"} {
		if _, set := os.LookupEnv(key); set {
			return true
		}
	}
	return false
}

func checkImageInputs(t *testing.T) {
	t.Helper()
	for _, key := range []string{"NABOS_IMAGE_BOOT", "NABOS_IMAGE_ENV", "NABOS_IMAGE_OVERLAYS", "NABOS_IMAGE_DISK", "NABOS_VENDOR_DTBS"} {
		if path, set := os.LookupEnv(key); set {
			if path == "" {
				t.Fatalf("%s is explicitly empty", key)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("%s: %v", key, err)
			}
		}
	}
	if path, set := os.LookupEnv("NABOS_UBOOT_SANDBOX"); set {
		for _, name := range []string{"u-boot", "dtc", "mkimage", "mkenvimage"} {
			binary := filepath.Join(path, "bin", name)
			if info, err := os.Stat(binary); path == "" || err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
				t.Fatalf("NABOS_UBOOT_SANDBOX missing executable %s (%v)", binary, err)
			}
		}
		if _, err := os.Stat(filepath.Join(path, "SHA256SUMS")); err != nil {
			t.Fatalf("NABOS_UBOOT_SANDBOX checksum manifest: %v", err)
		}
	}
	if !actualImage() {
		return
	}
	target := os.Getenv("NABOS_IMAGE_TARGET")
	dtb, ok := dtbs[target]
	if !ok {
		t.Fatalf("unknown NABOS_IMAGE_TARGET %q", target)
	}
	for _, key := range []string{"NABOS_IMAGE_BOOT", "NABOS_IMAGE_ENV", "NABOS_IMAGE_OVERLAYS", "NABOS_VENDOR_DTBS", "NABOS_UBOOT_SANDBOX"} {
		if os.Getenv(key) == "" {
			t.Fatalf("actual image requires %s", key)
		}
	}
	for _, path := range []string{
		filepath.Join(os.Getenv("NABOS_IMAGE_BOOT"), "boot.scr"),
		filepath.Join(os.Getenv("NABOS_IMAGE_BOOT"), "boot.env"),
		filepath.Join(vendorDTBs, dtb),
		filepath.Join(os.Getenv("NABOS_IMAGE_OVERLAYS"), "tagtagtag-sound.dtbo"),
	} {
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("actual image file missing: %s (%v)", path, err)
		}
	}
}

func TestActualImageInputs(t *testing.T) { checkImageInputs(t) }

func TestRequiredImageInputsRejectMissingPaths(t *testing.T) {
	for _, key := range []string{"NABOS_IMAGE_BOOT", "NABOS_IMAGE_ENV", "NABOS_IMAGE_OVERLAYS", "NABOS_IMAGE_DISK", "NABOS_VENDOR_DTBS", "NABOS_UBOOT_SANDBOX"} {
		t.Run(key, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestActualImageInputs$")
			for _, value := range os.Environ() {
				name, _, _ := strings.Cut(value, "=")
				if !strings.HasPrefix(name, "NABOS_IMAGE_") && name != "NABOS_VENDOR_DTBS" && name != "NABOS_UBOOT_SANDBOX" {
					cmd.Env = append(cmd.Env, value)
				}
			}
			cmd.Env = append(cmd.Env, key+"="+filepath.Join(t.TempDir(), "missing"))
			output, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(output), key) {
				t.Fatalf("missing %s must fail: %v\n%s", key, err, output)
			}
		})
	}
}
