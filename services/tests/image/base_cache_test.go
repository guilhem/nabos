package image

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestPreparedBaseCache(t *testing.T) {
	checkout := t.TempDir()
	if err := os.Mkdir(filepath.Join(checkout, "image"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := []string{"base-cache.sh", "build.sh", "prepare.sh", "sources.lock.json", "lva-requirements.lock",
		"build-config/99nabos-build", "build-config/policy-rc.d"}
	patches, err := filepath.Glob(filepath.Join(imageDir, "patches", "*.patch"))
	if err != nil || len(patches) == 0 {
		t.Fatalf("patch fixtures: %v, %v", patches, err)
	}
	if err := os.Mkdir(filepath.Join(checkout, "image", "patches"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, patch := range patches {
		files = append(files, filepath.Join("patches", filepath.Base(patch)))
	}
	for _, name := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(checkout, "image", name)), 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(checkout, "image", name), read(t, filepath.Join(imageDir, name)))
	}
	script := filepath.Join(checkout, "image", "base-cache.sh")
	t.Setenv("NABOS_BASE_CACHE_EPOCH", "2026-09-30")
	key := func(t *testing.T, target string) string {
		t.Helper()
		return strings.TrimSpace(run(t, "", "bash", script, "key", target))
	}
	if key(t, "zero-armv6") == key(t, "zero2-arm64") {
		t.Fatal("targets share a cache identity")
	}
	for _, target := range []string{"zero-armv6", "zero2-arm64"} {
		t.Run(target, func(t *testing.T) {
			identity := key(t, target)
			prepared := t.TempDir()
			payload := map[string]string{
				"kernel-release":           "6.12-test-" + target + "\n",
				"builder-packages.tsv":     "test-package\t1.0\t" + target + "\n",
				"inputs/debs/manifest.tsv": "test-package\t1.0\t" + target + "\ttest.deb\n",
				"inputs/debs/test.deb":     "deb fixture for " + target,
			}
			if target == "zero2-arm64" {
				payload["inputs/wheels/test.whl"] = "wheel fixture for " + target
			}
			for _, name := range []string{"inputs/debs/test.deb", "inputs/wheels/test.whl"} {
				if data, ok := payload[name]; ok {
					payload[filepath.Join(filepath.Dir(name), "SHA256SUMS")] = fmt.Sprintf("%x  %s\n", sha256.Sum256([]byte(data)), filepath.Base(name))
				}
			}
			for name, content := range payload {
				path := filepath.Join(prepared, "payload", name)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				write(t, path, content)
			}
			const imageSize = int64(6 * 1024 * MiB)
			image := filepath.Join(prepared, "base.img")
			raFile(t, image, imageSize)
			marker := []byte("prepared base for " + target)
			f, err := os.OpenFile(image, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, offset := range []int64{0, imageSize / 2, imageSize - int64(len(marker))} {
				if _, err := f.WriteAt(marker, offset); err != nil {
					f.Close()
					t.Fatal(err)
				}
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			run(t, "", "bash", script, "save", target, prepared)
			cache := filepath.Join(checkout, "build", "cache", "base", target)
			archive := filepath.Join(cache, "base.tar")
			if read(t, filepath.Join(cache, "identity")) != identity+"\n" {
				t.Fatal("saved identity differs from the key")
			}
			if stat := must(os.Stat(archive)); stat.Size() >= MiB {
				t.Fatalf("6 GiB sparse image produced a %d-byte archive", stat.Size())
			}
			archiveHash := raFileHash(t, archive)
			restore := func() string {
				t.Helper()
				work := t.TempDir()
				if err := os.Mkdir(filepath.Join(work, "payload"), 0o755); err != nil {
					t.Fatal(err)
				}
				run(t, "", "bash", script, "restore", target, work)
				image := filepath.Join(work, "base.img")
				stat := must(os.Stat(image))
				if stat.Size() != imageSize || stat.Sys().(*syscall.Stat_t).Blocks*512 >= MiB {
					t.Fatalf("restored image lost its size or holes: %v", stat)
				}
				for _, offset := range []int64{0, imageSize / 2, imageSize - int64(len(marker))} {
					if raRangeHash(t, image, offset, int64(len(marker))) != sha256.Sum256(marker) {
						t.Fatalf("restored image data differs at %d", offset)
					}
				}
				if raRangeHash(t, image, MiB, 4096) != sha256.Sum256(make([]byte, 4096)) {
					t.Fatal("restored hole contains data")
				}
				for name, content := range payload {
					if read(t, filepath.Join(work, "payload", name)) != content {
						t.Fatalf("restored payload differs: %s", name)
					}
				}
				return work
			}
			copy := restore()
			write(t, filepath.Join(copy, "base.img"), "changed image")
			write(t, filepath.Join(copy, "payload", "kernel-release"), "changed kernel")
			if raFileHash(t, archive) != archiveHash {
				t.Fatal("changing the restored copy modified the cache archive")
			}
			restore()
			miss := func(t *testing.T) {
				t.Helper()
				work := t.TempDir()
				if err := os.Mkdir(filepath.Join(work, "payload"), 0o755); err != nil {
					t.Fatal(err)
				}
				write(t, filepath.Join(work, "base.img"), "keep image")
				write(t, filepath.Join(work, "payload", "kernel-release"), "keep kernel")
				r := execute(t, "", nil, "bash", script, "restore", target, work)
				if r.code != 1 || read(t, filepath.Join(work, "base.img")) != "keep image" ||
					read(t, filepath.Join(work, "payload", "kernel-release")) != "keep kernel" ||
					len(must(os.ReadDir(filepath.Join(work, "payload")))) != 1 {
					t.Fatalf("cache miss used the archive: exit %d\n%s%s", r.code, r.stdout, r.stderr)
				}
			}
			for _, change := range []string{"epoch", "prepare", "build-config"} {
				t.Run(change, func(t *testing.T) {
					prepare := filepath.Join(checkout, "image", "prepare.sh")
					if change == "build-config" {
						prepare = filepath.Join(checkout, "image", "build-config", "policy-rc.d")
					}
					if change == "epoch" {
						t.Setenv("NABOS_BASE_CACHE_EPOCH", "2026-10-01")
					} else {
						original := read(t, prepare)
						write(t, prepare, original+"\n# changed preparation\n")
						t.Cleanup(func() { write(t, prepare, original) })
					}
					if key(t, target) == identity {
						t.Fatalf("%s did not invalidate the key", change)
					}
					miss(t)
				})
			}
			if key(t, target) != identity {
				t.Fatal("restoring the inputs did not restore the key")
			}
			write(t, archive, "corrupt tar")
			miss(t)
		})
	}
}
