package image

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func migrationFixture(t *testing.T, syncFailure string) (string, string, fakeCommands) {
	t.Helper()
	dir := t.TempDir()
	for _, path := range []string{"data", "etc/rauc"} {
		if err := os.MkdirAll(filepath.Join(dir, path), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for n, geometry := range [][2]int{{8192, 524288}, {1056768, 12582912}, {13639680, 12582912}, {26222592, 2097152}} {
		path := filepath.Join(dir, "sys", fmt.Sprintf("mmcblk0p%d", n+1))
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(path, "start"), fmt.Sprint(geometry[0]))
		write(t, filepath.Join(path, "size"), fmt.Sprint(geometry[1]))
	}
	write(t, filepath.Join(dir, "etc/rauc/ca.cert.pem"), "existing device authority\n")
	if err := os.Chmod(filepath.Join(dir, "etc/rauc/ca.cert.pem"), 0444); err != nil {
		t.Fatal(err)
	}
	hook := strings.NewReplacer("/sys/class/block", filepath.Join(dir, "sys"),
		"/etc/rauc", filepath.Join(dir, "etc/rauc"), "/data", filepath.Join(dir, "data")).
		Replace(read(t, filepath.Join(imageDir, "rauc-hook.sh")))
	path := filepath.Join(dir, "hook.sh")
	write(t, path, hook)
	fake := newFakes(t, dir, map[string]string{
		"findmnt": `printf '%s\n' "${MOUNT_SOURCE:-/dev/mmcblk0p4 ext4}"`,
		"sync":    "case $2 in " + syncFailure + ") exit 42 ;; esac",
	})
	return dir, path, fake
}

func TestRaucMigrationInstallCheck(t *testing.T) {
	for _, target := range []string{"zero-armv6", "zero2-arm64"} {
		for _, system := range []string{"nabos-" + target, "nabos-nixos-" + target} {
			t.Run(system, func(t *testing.T) {
				dir, hook, fake := migrationFixture(t, "never-match")
				env := fake.env("RAUC_SYSTEM_COMPATIBLE="+system, "RAUC_MF_COMPATIBLE=nabos-"+target)
				r := execute(t, "", env, "sh", hook, "install-check")
				if r.code != 0 {
					t.Fatal(r.stderr)
				}
				certificate := filepath.Join(dir, "data/rauc/ca.cert.pem")
				if read(t, certificate) != read(t, filepath.Join(dir, "etc/rauc/ca.cert.pem")) {
					t.Fatal("device authority changed during migration")
				}
				info, err := os.Stat(certificate)
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatal("persistent certificate must be private", info, err)
				}
				// A switched FAT and already-grown /data are normal on the next update.
				write(t, filepath.Join(dir, "sys/mmcblk0p1/start"), "532480")
				write(t, filepath.Join(dir, "sys/mmcblk0p4/size"), "4194304")
				if err := os.Chmod(filepath.Join(dir, "etc/rauc/ca.cert.pem"), 0644); err != nil {
					t.Fatal(err)
				}
				write(t, filepath.Join(dir, "etc/rauc/ca.cert.pem"), "different authority\n")
				if r := execute(t, "", env, "sh", hook, "install-check"); r.code != 0 {
					t.Fatal(r.stderr)
				}
				if read(t, certificate) != "existing device authority\n" {
					t.Fatal("an existing persistent authority was replaced")
				}
			})
		}
	}
}

func TestRaucMigrationRejectionsDoNotPublishTrust(t *testing.T) {
	for _, failure := range []string{"wrong-target", "missing-target", "wrong-system", "old-nixos-bundle", "boot-start", "boot-size", "root-start", "root-size", "old-single-fat", "data-start", "data-size", "missing-partition", "unmounted-data", "volatile-data", "missing-authority"} {
		t.Run(failure, func(t *testing.T) {
			dir, hook, fake := migrationFixture(t, "never-match")
			system, bundle := "nabos-zero-armv6", "nabos-zero-armv6"
			extra := []string{}
			switch failure {
			case "wrong-target":
				bundle = "nabos-zero2-arm64"
			case "missing-target":
				bundle = ""
			case "wrong-system":
				system = "other-board"
			case "old-nixos-bundle":
				bundle = "nabos-nixos-zero-armv6"
			case "boot-start":
				write(t, filepath.Join(dir, "sys/mmcblk0p1/start"), "1")
			case "boot-size":
				write(t, filepath.Join(dir, "sys/mmcblk0p1/size"), "1")
			case "root-start", "old-single-fat":
				write(t, filepath.Join(dir, "sys/mmcblk0p2/start"), "532480")
			case "root-size":
				write(t, filepath.Join(dir, "sys/mmcblk0p3/size"), "1")
			case "data-start":
				write(t, filepath.Join(dir, "sys/mmcblk0p4/start"), "1")
			case "data-size":
				write(t, filepath.Join(dir, "sys/mmcblk0p4/size"), "1")
			case "missing-partition":
				if err := os.Remove(filepath.Join(dir, "sys/mmcblk0p4/start")); err != nil {
					t.Fatal(err)
				}
			case "unmounted-data":
				extra = append(extra, "MOUNT_SOURCE=tmpfs tmpfs")
			case "volatile-data":
				write(t, filepath.Join(dir, "data/.volatile"), "")
			case "missing-authority":
				if err := os.Remove(filepath.Join(dir, "etc/rauc/ca.cert.pem")); err != nil {
					t.Fatal(err)
				}
			}
			env := fake.env(append(extra, "RAUC_SYSTEM_COMPATIBLE="+system, "RAUC_MF_COMPATIBLE="+bundle)...)
			if r := execute(t, "", env, "sh", hook, "install-check"); r.code < 10 {
				t.Fatalf("invalid installation not rejected: %d %s", r.code, r.stderr)
			}
			if _, err := os.Stat(filepath.Join(dir, "data/rauc")); !os.IsNotExist(err) {
				t.Fatal("rejected installation published trust", err)
			}
		})
	}
}

func TestRaucMigrationInterruptedTrustCopy(t *testing.T) {
	for _, stage := range []string{"*/.ca.cert.*", "*/rauc"} {
		t.Run(stage, func(t *testing.T) {
			dir, hook, fake := migrationFixture(t, stage)
			env := fake.env("RAUC_SYSTEM_COMPATIBLE=nabos-zero-armv6", "RAUC_MF_COMPATIBLE=nabos-zero-armv6")
			if r := execute(t, "", env, "sh", hook, "install-check"); r.code == 0 {
				t.Fatal("sync failure accepted")
			}
			certificate := filepath.Join(dir, "data/rauc/ca.cert.pem")
			if stage == "*/.ca.cert.*" {
				if _, err := os.Stat(certificate); !os.IsNotExist(err) {
					t.Fatal("authority published before file sync", err)
				}
			} else if read(t, certificate) != "existing device authority\n" {
				t.Fatal("incomplete authority published")
			}
			if temporary, err := filepath.Glob(filepath.Join(dir, "data/rauc/.ca.cert.*")); err != nil || len(temporary) != 0 {
				t.Fatal("temporary certificate retained", temporary, err)
			}
			write(t, filepath.Join(fake.bin, "sync"), "#!/bin/sh\nexit 0\n")
			if r := execute(t, "", env, "sh", hook, "install-check"); r.code != 0 || read(t, certificate) != "existing device authority\n" {
				t.Fatal("interrupted migration cannot be retried", r.stderr)
			}
		})
	}
}

func TestRaucMigrationInvalidExistingTrust(t *testing.T) {
	for _, kind := range []string{"empty", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir, hook, fake := migrationFixture(t, "never-match")
			path := filepath.Join(dir, "data/rauc")
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			certificate := filepath.Join(path, "ca.cert.pem")
			if kind == "symlink" {
				if err := os.Symlink(filepath.Join(dir, "etc/rauc/ca.cert.pem"), certificate); err != nil {
					t.Fatal(err)
				}
			} else {
				write(t, certificate, "")
			}
			env := fake.env("RAUC_SYSTEM_COMPATIBLE=nabos-zero-armv6", "RAUC_MF_COMPATIBLE=nabos-zero-armv6")
			if r := execute(t, "", env, "sh", hook, "install-check"); r.code != 10 {
				t.Fatalf("invalid existing trust accepted: %d %s", r.code, r.stderr)
			}
			info, err := os.Lstat(certificate)
			if err != nil || (kind == "symlink" && info.Mode()&os.ModeSymlink == 0) || (kind == "empty" && info.Size() != 0) {
				t.Fatal("invalid authority was silently replaced", info, err)
			}
		})
	}
}
