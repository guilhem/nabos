package image

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestManualBundleTrustIsTemporary(t *testing.T) {
	imageTools(t, "rauc", "openssl", "mksquashfs")
	dir := t.TempDir()
	input, runtime, trust, work := filepath.Join(dir, "input"), filepath.Join(dir, "runtime"), filepath.Join(dir, "trust"), filepath.Join(dir, "work")
	for _, path := range []string{input, runtime} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	cert, key := filepath.Join(dir, "signer.pem"), filepath.Join(dir, "signer.key")
	production := filepath.Join(dir, "production.pem")
	for _, paths := range [][2]string{{cert, key}, {production, filepath.Join(dir, "production.key")}} {
		run(t, "", "openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-subj", "/CN=Bundle test/", "-keyout", paths[1], "-out", paths[0])
	}
	write(t, filepath.Join(input, "manifest.raucm"), "[update]\ncompatible=nabos-test\nversion=dev-manual\n[bundle]\nformat=verity\n[image.rootfs]\nfilename=rootfs.ext4\n")
	payload := make([]byte, 16*1024)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(input, "rootfs.ext4"), string(payload))
	bundle := filepath.Join(dir, "original.raucb")
	run(t, "", "rauc", "bundle", "--cert="+cert, "--key="+key, "--mksquashfs-args=-processors 1", input, bundle)
	conf := filepath.Join(dir, "system.conf")
	write(t, conf, "[system]\ncompatible=nabos-test\nbootloader=noop\n[keyring]\npath="+production+"\ndirectory="+trust+"\n")
	info := func(path string) result {
		return execute(t, "", nil, "rauc", "--conf="+conf, "info", path)
	}
	productionBundle := filepath.Join(dir, "production.raucb")
	run(t, "", "rauc", "resign", "--no-verify", "--cert="+production, "--key="+filepath.Join(dir, "production.key"), bundle, productionBundle)
	if r := info(productionBundle); r.code != 0 {
		t.Fatal("production trust failed before any manual install", r.stderr)
	}
	if info(bundle).code == 0 {
		t.Fatal("unknown signer accepted without opting in")
	}
	if err := os.Mkdir(trust, 0755); err != nil {
		t.Fatal(err)
	}
	// Exercise the shipped script with host uid/gid and disposable paths. This
	// checks real CMS/RAUC behavior; it does not qualify the device's systemd sandbox.
	helper := strings.NewReplacer(
		"/data/nabos-rauc-manual", work,
		"/run/nabos-rauc-manual", runtime,
		"/run/nabos-rauc-trust", trust,
		"/data/device-core/updates/manual.raucb", filepath.Join(dir, "upload.raucb"),
		"-o root -g nabos", fmt.Sprintf("-o %d -g %d", os.Getuid(), os.Getgid()),
		"root:nabos", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
	).Replace(read(t, filepath.Join(rootfsDir, "usr/lib/nabos/rauc-manual")))
	path := filepath.Join(dir, "helper.sh")
	write(t, path, helper)
	write(t, filepath.Join(dir, "upload.raucb"), read(t, bundle))
	run(t, "", "sh", path)
	prepared := filepath.Join(work, "bundle.raucb")
	if r := info(prepared); r.code != 0 || !strings.Contains(r.stdout, "dev-manual") || !strings.Contains(r.stdout, "nabos-test") {
		t.Fatalf("temporary trust failed or changed manifest: %s%s", r.stdout, r.stderr)
	}
	if info(bundle).code == 0 {
		t.Fatal("temporary trust also accepted the original unknown signer")
	}
	if _, err := os.Stat(filepath.Join(runtime, "key.pem")); !os.IsNotExist(err) {
		t.Fatal("temporary signing key retained", err)
	}
	run(t, "", "sh", path, "cleanup")
	if entries, _ := os.ReadDir(trust); len(entries) != 0 {
		t.Fatal("temporary trust retained", entries)
	}
	if _, err := os.Stat(prepared); !os.IsNotExist(err) {
		t.Fatal("prepared bundle retained", err)
	}
	if r := info(productionBundle); r.code != 0 {
		t.Fatal("production trust failed after cleanup", r.stderr)
	}
	if r := execute(t, "", nil, "rauc", "info", "--keyring="+cert, bundle); r.code != 0 {
		t.Fatal("original bundle altered", r.stderr)
	}
	for _, invalid := range []string{"signature", "trailer", "symlink", "fifo"} {
		t.Run(invalid, func(t *testing.T) {
			_ = os.Remove(filepath.Join(dir, "upload.raucb"))
			raw := []byte(read(t, bundle))
			switch invalid {
			case "signature":
				raw[len(raw)-9] ^= 1
			case "trailer":
				binary.BigEndian.PutUint64(raw[len(raw)-8:], 1048577)
			case "symlink":
				if err := os.Symlink(bundle, filepath.Join(dir, "upload.raucb")); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := syscall.Mkfifo(filepath.Join(dir, "upload.raucb"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if invalid != "symlink" && invalid != "fifo" {
				write(t, filepath.Join(dir, "upload.raucb"), string(raw))
			}
			if r := execute(t, "", nil, "sh", path); r.code == 0 {
				t.Fatal("invalid upload accepted")
			}
			run(t, "", "sh", path, "cleanup")
			if entries, _ := os.ReadDir(trust); len(entries) != 0 {
				t.Fatal("invalid upload published trust")
			}
		})
	}
}
