package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Locked driver sources: DTBO overlay name and kernel modules built.
var driverSet = []struct {
	name, overlay string
	modules       []string
}{
	{"ears", "tagtagtag-ears", []string{"tagtagtag-ears"}},
	{"sound", "tagtagtag-sound", []string{"snd-soc-wm8960", "snd-soc-max9759", "snd-soc-volume-gpio"}},
	{"cr14", "cr14", []string{"cr14"}},
	{"nfc", "st25r391x", []string{"st25r391x"}},
}

func kernelBuild(value string) (string, string, error) {
	dir := value
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		dir = filepath.Join("/lib/modules", value, "build")
	}
	header := filepath.Join(dir, "include/generated/utsrelease.h")
	data, err := os.ReadFile(header)
	if err != nil {
		return "", "", err
	}
	m := regexp.MustCompile(`#define UTS_RELEASE "([^"]+)"`).FindSubmatch(data)
	if m == nil {
		return "", "", fmt.Errorf("no UTS_RELEASE in %s", header)
	}
	release := string(m[1])
	if dir == filepath.Join("/lib/modules", value, "build") && release != value {
		return "", "", fmt.Errorf("kernel release mismatch: %s != %s", release, value)
	}
	return dir, release, nil
}

func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// drivers applies the locked patches to the locked driver archives, builds
// each DTBO (which must keep external fixups) and optionally the modules.
func drivers(lockPath string, args []string) error {
	fs := flag.NewFlagSet("drivers", flag.ContinueOnError)
	archives := fs.String("archives", "", "directory containing the locked ears/sound/cr14/nfc tar.gz files")
	kernelArg := fs.String("kernel", "", "KERNELRELEASE or its headers build directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *archives == "" || fs.NArg() != 0 {
		return fmt.Errorf("drivers: --archives DIR is required")
	}
	lock, err := readLock(lockPath)
	if err != nil {
		return err
	}
	image := filepath.Dir(lockPath)
	kernel, release := "", ""
	if *kernelArg != "" {
		if kernel, release, err = kernelBuild(*kernelArg); err != nil {
			return err
		}
	}
	work, err := os.MkdirTemp("", "nabos-drivers-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	for _, d := range driverSet {
		archive := filepath.Join(*archives, d.name+".tar.gz")
		if sum, err := digest(archive); err != nil || sum != lock.Sources[d.name].SHA256 {
			return fmt.Errorf("locked archive hash mismatch: %s", archive)
		}
		source, err := unpack(archive, filepath.Join(work, d.name))
		if err != nil {
			return err
		}
		patch, err := filepath.Abs(filepath.Join(image, "patches", d.name+".patch"))
		if err != nil {
			return err
		}
		if err := runCmd("patch", "--batch", "--fuzz=0", "-d", source, "-p1", "-i", patch); err != nil {
			return err
		}
		if err := runCmd("make", "-C", source, d.overlay+".dtbo"); err != nil {
			return err
		}
		dtbo := filepath.Join(source, d.overlay+".dtbo")
		decoded, err := exec.Command("dtc", "-I", "dtb", "-O", "dts", dtbo).Output()
		if err != nil {
			return fmt.Errorf("dtc %s: %w", dtbo, err)
		}
		if !strings.Contains(string(decoded), "__fixups__ {") {
			return fmt.Errorf("no external fixups in %s", dtbo)
		}
		done := "patch + DTBO"
		if kernel != "" {
			if err := runCmd("make", "-C", kernel, "M="+source, "modules"); err != nil {
				return err
			}
			for _, module := range d.modules {
				ko := filepath.Join(source, module+".ko")
				out, err := exec.Command("modinfo", "-F", "vermagic", ko).Output()
				if f := strings.Fields(string(out)); err != nil || len(f) == 0 || f[0] != release {
					return fmt.Errorf("wrong vermagic in %s: %s", ko, strings.TrimSpace(string(out)))
				}
			}
			done += " + modules"
		}
		fmt.Printf("%s: %s\n", d.name, done)
	}
	return nil
}
