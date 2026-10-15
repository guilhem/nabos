package image

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func installCheckFixture(t *testing.T) (string, string, fakeCommands) {
	t.Helper()
	dir := t.TempDir()
	for _, path := range []string{"data", "etc/rauc", "usr/share/nabos", "run"} {
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
	write(t, filepath.Join(dir, "usr/share/nabos/ca.cert.pem"), "image public authority\n")
	if err := os.Chmod(filepath.Join(dir, "usr/share/nabos/ca.cert.pem"), 0444); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "cmdline"), "root=/dev/mmcblk0p2 rauc.slot=A quiet\n")
	hook := strings.NewReplacer("/sys/class/block", filepath.Join(dir, "sys"),
		"/proc/cmdline", filepath.Join(dir, "cmdline"),
		"/usr/share/nabos", filepath.Join(dir, "usr/share/nabos"),
		"/run/nabos-rollback", filepath.Join(dir, "run/nabos-rollback"),
		"/etc/rauc", filepath.Join(dir, "etc/rauc"), "/data", filepath.Join(dir, "data")).
		Replace(read(t, filepath.Join(imageDir, "rauc-hook.sh")))
	write(t, filepath.Join(dir, "layout.sh"), read(t, filepath.Join(imageDir, "layout.sh")))
	path := filepath.Join(dir, "hook.sh")
	write(t, path, hook)
	fake := newFakes(t, dir, map[string]string{
		"findmnt":     `printf '%s\n' "${MOUNT_SOURCE:-/dev/mmcblk0p4 ext4}"`,
		"fw_setenv":   `test "${ENV_WRITE_FAIL:-0}" != 1 || exit 42; test "${ENV_WRITE_WARNING:-0}" != 1 || echo "fsync failed on SD environment" >&2; awk '$1 == "NABOS_ROLLBACK_SLOT" {print $2}' "$2" > ` + filepath.Join(dir, "environment"),
		"fw_printenv": `test "${ENV_READ_FAIL:-0}" != 1 || exit 42; test "${ENV_READ_VALUE:-}" = "" || { echo "$ENV_READ_VALUE"; exit; }; cat ` + filepath.Join(dir, "environment"),
	})
	return dir, path, fake
}

func TestRaucNativeInstallCheck(t *testing.T) {
	for _, target := range []string{"zero-armv6", "zero2-arm64"} {
		t.Run(target, func(t *testing.T) {
			dir, hook, fake := installCheckFixture(t)
			certificate := filepath.Join(dir, "usr/share/nabos/ca.cert.pem")
			before := read(t, certificate)
			env := fake.env("RAUC_SYSTEM_COMPATIBLE=nabos-native-rauc-"+target, "RAUC_MF_COMPATIBLE=nabos-native-rauc-"+target)
			for _, bootStart := range []string{"8192", "532480"} {
				write(t, filepath.Join(dir, "sys/mmcblk0p1/start"), bootStart)
				write(t, filepath.Join(dir, "sys/mmcblk0p4/size"), "4194304")
				if r := execute(t, "", env, "sh", hook, "install-check"); r.code != 0 {
					t.Fatal(r.stderr)
				}
				if read(t, certificate) != before {
					t.Fatal("update changed immutable trust")
				}
				if _, err := os.Stat(filepath.Join(dir, "data/rauc")); !os.IsNotExist(err) {
					t.Fatal("created persistent trust", err)
				}
			}
		})
	}
}

func TestRaucInstallCheckRejectionsDoNotWrite(t *testing.T) {
	for _, failure := range []string{"wrong-target", "missing-target", "wrong-system", "old-nixos-bundle", "old-system", "old-bundle", "boot-start", "boot-size", "root-start", "root-size", "old-single-fat", "data-start", "data-size", "missing-partition", "unmounted-data", "volatile-data", "missing-authority"} {
		t.Run(failure, func(t *testing.T) {
			dir, hook, fake := installCheckFixture(t)
			system, bundle := "nabos-native-rauc-zero-armv6", "nabos-native-rauc-zero-armv6"
			extra := []string{}
			switch failure {
			case "wrong-target":
				bundle = "nabos-native-rauc-zero2-arm64"
			case "missing-target":
				bundle = ""
			case "wrong-system":
				system = "other-board"
			case "old-system":
				system = "nabos-nixos-zero-armv6"
			case "old-bundle":
				bundle = "nabos-zero-armv6"
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
				if err := os.Remove(filepath.Join(dir, "usr/share/nabos/ca.cert.pem")); err != nil {
					t.Fatal(err)
				}
			}
			env := fake.env(append(extra, "RAUC_SYSTEM_COMPATIBLE="+system, "RAUC_MF_COMPATIBLE="+bundle)...)
			if r := execute(t, "", env, "sh", hook, "install-check"); r.code < 10 {
				t.Fatalf("invalid installation not rejected: %d %s", r.code, r.stderr)
			}
			if _, err := os.Stat(filepath.Join(dir, "data/rauc")); !os.IsNotExist(err) {
				t.Fatal("rejected installation wrote persistent trust", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "environment")); !os.IsNotExist(err) {
				t.Fatal("rejected installation changed environment", err)
			}
		})
	}
}

func TestRaucInvalidImageTrust(t *testing.T) {
	for _, kind := range []string{"empty", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir, hook, fake := installCheckFixture(t)
			certificate := filepath.Join(dir, "usr/share/nabos/ca.cert.pem")
			if err := os.Remove(certificate); err != nil {
				t.Fatal(err)
			}
			if kind == "symlink" {
				write(t, filepath.Join(dir, "etc/rauc/ca.cert.pem"), "old authority\n")
				if err := os.Symlink(filepath.Join(dir, "etc/rauc/ca.cert.pem"), certificate); err != nil {
					t.Fatal(err)
				}
			} else {
				write(t, certificate, "")
			}
			env := fake.env("RAUC_SYSTEM_COMPATIBLE=nabos-native-rauc-zero-armv6", "RAUC_MF_COMPATIBLE=nabos-native-rauc-zero-armv6")
			if r := execute(t, "", env, "sh", hook, "install-check"); r.code != 10 {
				t.Fatalf("invalid image trust accepted: %d %s", r.code, r.stderr)
			}
			if _, err := os.Stat(filepath.Join(dir, "environment")); !os.IsNotExist(err) {
				t.Fatal("invalid trust changed environment", err)
			}
		})
	}
}

func TestRaucRecordsRunningSlotOnlyAfterValidation(t *testing.T) {
	for _, tc := range []struct {
		name, cmdline, flag string
		success             bool
	}{
		{"A", "BOOT_ORDER=B A rauc.slot=A", "", true},
		{"B", "rauc.slot=B", "", true},
		{"missing", "root=/dev/mmcblk0p2", "", false},
		{"invalid", "rauc.slot=C", "", false},
		{"ambiguous", "rauc.slot=A rauc.slot=B", "", false},
		{"empty then valid", "rauc.slot= rauc.slot=A", "", false},
		{"write failure", "rauc.slot=A", "ENV_WRITE_FAIL=1", false},
		{"write flush warning", "rauc.slot=A", "ENV_WRITE_WARNING=1", false},
		{"verification failure", "rauc.slot=A", "ENV_READ_VALUE=B", false},
		{"read failure", "rauc.slot=A", "ENV_READ_FAIL=1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, hook, fake := installCheckFixture(t)
			write(t, filepath.Join(dir, "cmdline"), tc.cmdline)
			env := fake.env("RAUC_SYSTEM_COMPATIBLE=nabos-native-rauc-zero-armv6", "RAUC_MF_COMPATIBLE=nabos-native-rauc-zero-armv6")
			if tc.flag != "" {
				env = append(env, tc.flag)
			}
			r := execute(t, "", env, "sh", hook, "install-check")
			if (r.code == 0) != tc.success {
				t.Fatalf("exit %d: %s", r.code, r.stderr)
			}
			if tc.success {
				want := tc.name + "\n"
				if got := read(t, filepath.Join(dir, "environment")); got != want {
					t.Fatalf("history %q, want %q", got, want)
				}
				if _, err := os.Stat(filepath.Join(dir, "data/rauc")); !os.IsNotExist(err) {
					t.Fatal("image trust must not require persistent trust")
				}
			}
		})
	}
}
