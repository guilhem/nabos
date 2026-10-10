package image

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func imageTools(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			if actualImage() {
				t.Fatal(err)
			}
			t.Skipf("image tool %s not installed", name)
		}
	}
}

func diskRangeHash(t *testing.T, disk string, offset, size int64) []byte {
	t.Helper()
	f, err := os.Open(disk)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if n, err := io.Copy(h, io.NewSectionReader(f, offset, size)); err != nil || n != size {
		t.Fatalf("read %s at %d: %d bytes: %v", disk, offset, n, err)
	}
	return h.Sum(nil)
}

func checkInitialCard(t *testing.T, disk string) {
	t.Helper()
	f, err := os.Open(disk)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	mbr := make([]byte, 512)
	if _, err := io.ReadFull(f, mbr); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mbr[510:], []byte{0x55, 0xaa}) {
		t.Fatal("missing MBR signature")
	}
	for i, p := range []struct{ start, size uint32 }{{4, 256}, {516, 6144}, {6660, 6144}, {12804, 1024}} {
		entry := mbr[446+i*16 : 462+i*16]
		if binary.LittleEndian.Uint32(entry[8:12]) != p.start*(MiB/512) ||
			binary.LittleEndian.Uint32(entry[12:16]) != p.size*(MiB/512) {
			t.Fatalf("MBR p%d: %x, want start=%dM size=%dM", i+1, entry, p.start, p.size)
		}
		kind := byte(0x83)
		if i == 0 {
			kind = 0x0c
			if entry[0] != 0x80 {
				t.Error("p1 is not bootable")
			}
		}
		if entry[4] != kind {
			t.Errorf("p%d type %x", i+1, entry[4])
		}
	}
	for _, region := range []struct{ first, second, size int64 }{{MiB, 2 * MiB, 65536}, {4 * MiB, 260 * MiB, 256 * MiB}} {
		if !bytes.Equal(diskRangeHash(t, disk, region.first, region.size), diskRangeHash(t, disk, region.second, region.size)) {
			t.Errorf("initial copies differ at %d and %d", region.first, region.second)
		}
	}
	if fi, err := f.Stat(); err != nil || fi.Size() != 13828*MiB {
		t.Fatalf("unexpected card size: %v, %v", fi, err)
	}
}

func TestGeneratedCardLayout(t *testing.T) {
	// The build exercises the actual assembled image too, before the initrd grows p4.
	if disk := os.Getenv("NABOS_IMAGE_DISK"); disk != "" {
		checkInitialCard(t, disk)
		return
	}
	imageTools(t, "genimage")
	tmp := t.TempDir()
	for _, dir := range []string{"images", "empty"} {
		if err := os.Mkdir(filepath.Join(tmp, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Sparse inputs keep the real geometry cheap; genimage copies distinct markers.
	for name, size := range map[string]int64{"uboot.env": 65536, "boot.vfat": 256 * MiB, "rootfs.ext4": 6144 * MiB, "data.ext4": 1024 * MiB} {
		path := filepath.Join(tmp, "images", name)
		write(t, path, name)
		if err := os.Truncate(path, size); err != nil {
			t.Fatal(err)
		}
	}
	run(t, "", "genimage", "--config", filepath.Join(imageDir, "genimage.cfg"),
		"--rootpath", filepath.Join(tmp, "empty"), "--inputpath", filepath.Join(tmp, "images"),
		"--outputpath", filepath.Join(tmp, "images"), "--tmppath", filepath.Join(tmp, "genimage"))
	disk := filepath.Join(tmp, "images/sdcard.img")
	checkInitialCard(t, disk)
	for offset, name := range map[int64]string{MiB: "uboot.env", 2 * MiB: "uboot.env", 4 * MiB: "boot.vfat", 260 * MiB: "boot.vfat", 516 * MiB: "rootfs.ext4", 12804 * MiB: "data.ext4"} {
		want := sha256.Sum256([]byte(name))
		if got := diskRangeHash(t, disk, offset, int64(len(name))); !bytes.Equal(got, want[:]) {
			t.Errorf("missing %s at %d", name, offset)
		}
	}
}

func TestSignedCompleteBundle(t *testing.T) {
	imageTools(t, "rauc", "openssl", "mksquashfs", "unsquashfs")
	var lock struct {
		Targets map[string]struct{ Compatible string }
	}
	if err := json.Unmarshal([]byte(read(t, filepath.Join(imageDir, "sources.lock.json"))), &lock); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"zero-armv6", "zero2-arm64"} {
		t.Run(target, func(t *testing.T) {
			compatible := lock.Targets[target].Compatible
			if compatible != "nabos-nixos-"+target {
				t.Fatal("installed NixOS must reject old bundles without an install-check hook")
			}
			compatible = "nabos-" + target
			tmp := t.TempDir()
			for _, dir := range []string{"images"} {
				if err := os.Mkdir(filepath.Join(tmp, dir), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			// Verity needs a squashfs larger than one block. Random content also
			// detects a missing/truncated payload instead of hashing only zeros.
			for _, name := range []string{"rootfs.ext4", "boot.vfat"} {
				data := make([]byte, 16*1024)
				if _, err := rand.Read(data); err != nil {
					t.Fatal(err)
				}
				write(t, filepath.Join(tmp, "images", name), string(data))
			}
			cert, key := filepath.Join(tmp, "cert.pem"), filepath.Join(tmp, "key.pem")
			run(t, "", "openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-subj", "/CN=NabOS bundle test/", "-keyout", key, "-out", cert)
			bundle := raBundle(t, tmp, "complete", compatible, filepath.Join(tmp, "images/rootfs.ext4"), filepath.Join(tmp, "images/boot.vfat"), key, cert, read(t, filepath.Join(imageDir, "rauc-hook.sh")))
			if r := execute(t, "", nil, "rauc", "info", "--keyring="+cert, bundle); r.code != 0 {
				t.Fatal(r.stderr)
			}
			if stat := run(t, "", "unsquashfs", "-stat", bundle); !strings.Contains(stat, "Compression zstd") {
				t.Fatalf("bundle compression: %s", stat)
			}
			manifest := run(t, "", "unsquashfs", "-cat", bundle, "manifest.raucm")
			if !strings.Contains(manifest, "compatible="+compatible+"\n") || !strings.Contains(manifest, "format=verity") || !strings.Contains(manifest, "filename=rauc-hook.sh\n") || !strings.Contains(manifest, "hooks=install-check") || strings.Index(manifest, "[image.rootfs]") >= strings.Index(manifest, "[image.bootloader]") {
				t.Fatalf("format or image order: %s", manifest)
			}
			if hook := run(t, "", "unsquashfs", "-cat", bundle, "rauc-hook.sh"); hook != read(t, filepath.Join(imageDir, "rauc-hook.sh")) {
				t.Fatal("signed bundle changed the install-check hook")
			}
			for _, name := range []string{"rootfs.ext4", "boot.vfat"} {
				if content := run(t, "", "unsquashfs", "-cat", bundle, name); content != read(t, filepath.Join(tmp, "images", name)) {
					t.Errorf("bundle changed %s", name)
				}
			}
			untrusted := filepath.Join(tmp, "untrusted.pem")
			run(t, "", "openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-subj", "/CN=Untrusted/", "-keyout", filepath.Join(tmp, "untrusted.key"), "-out", untrusted)
			if r := execute(t, "", nil, "rauc", "info", "--keyring="+untrusted, bundle); r.code == 0 {
				t.Fatal("untrusted signing certificate accepted")
			}
			corrupt := []byte(read(t, bundle))
			corrupt[len(corrupt)-9] ^= 1 // last CMS byte, before the 8-byte length trailer
			write(t, bundle, string(corrupt))
			if r := execute(t, "", nil, "rauc", "info", "--keyring="+cert, bundle); r.code == 0 {
				t.Fatal("corrupted signature accepted")
			}
		})
	}
}

// Exercise the shared hook consumed by Nix; inject sync failure only.
func raucPostInstall(t *testing.T) string {
	t.Helper()
	config := read(t, filepath.Join(repo, "nix/system.nix"))
	if !strings.Contains(config, `${builtins.readFile (rootfs + "/usr/lib/nabos/rauc-post-install")}`) {
		t.Fatal("Nix must consume the shared RAUC post-install hook")
	}
	return read(t, filepath.Join(rootfsDir, "usr/lib/nabos/rauc-post-install"))
}

func TestRaucPostInstallSync(t *testing.T) {
	for _, status := range []int{0, 74} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			fake := newFakes(t, t.TempDir(), map[string]string{"sync": fmt.Sprintf("exit %d", status)})
			r := execute(t, "", fake.env(), "sh", "-c", raucPostInstall(t))
			if r.code != status || !slices.Equal(fake.calls(t), []string{"sync /dev/mmcblk0"}) {
				t.Fatalf("sync result: %d, calls %q: %s", r.code, fake.calls(t), r.stderr)
			}
		})
	}
}
