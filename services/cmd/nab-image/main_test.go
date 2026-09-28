package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestVerifiedDownloadIsAtomic(t *testing.T) {
	retryDelay = 0
	body := "tampered"
	hits := 0
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		hits++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	url := "https://example.org/source"
	dest := filepath.Join(t.TempDir(), "source.tar.gz")
	os.WriteFile(dest, []byte("previous"), 0o644)
	sum := sha256.Sum256([]byte("reviewed"))
	expected := hex.EncodeToString(sum[:])

	if err := download(client, url, expected, dest); err == nil {
		t.Fatal("tampered download accepted")
	}
	if data, _ := os.ReadFile(dest); string(data) != "previous" || hits != 3 {
		t.Fatalf("dest %q after %d attempts", data, hits)
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Fatal("partial download left behind")
	}
	body = "reviewed"
	if err := download(client, url, expected, dest); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(dest); string(data) != "reviewed" {
		t.Fatalf("dest %q", data)
	}
	hits = 0
	if err := download(client, url, expected, dest); err != nil || hits != 0 {
		t.Fatalf("verified input fetched again: %v, %d requests", err, hits)
	}
	if download(client, "http://example.org/x", expected, dest) == nil ||
		download(client, url, "latest", dest) == nil || hits != 0 {
		t.Fatal("input without HTTPS and SHA-256 accepted")
	}
}

func TestLVAOnlyForARM64(t *testing.T) {
	source := archive(t, entry{name: "source/file", kind: tar.TypeReg})
	data, _ := os.ReadFile(source)
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	dir := t.TempDir()
	lock := filepath.Join(dir, "sources.lock.json")
	write := func(name string) string {
		return `"` + name + `": {"url": "https://example.org/` + name + `", "sha256": "` + hash + `"}`
	}
	os.WriteFile(lock, []byte(`{"targets": {"zero-armv6": {}, "zero2-arm64": {}}, "sources": {`+write("ears")+", "+write("lva")+"}}"), 0o644)
	var fetched []string
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		fetched = append(fetched, r.URL.Path)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(data))}, nil
	})}
	inputs, src := filepath.Join(dir, "inputs"), filepath.Join(dir, "src")
	if err := fetch(client, lock, "zero-armv6", inputs, true); err != nil || strings.Join(fetched, " ") != "/ears" {
		t.Fatalf("ARMv6 fetched %q: %v", fetched, err)
	}
	if err := unpackSources(lock, inputs, src); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(src); len(entries) != 1 || entries[0].Name() != "ears" {
		t.Fatalf("ARMv6 sources %v", entries)
	}
	if err := fetch(client, lock, "zero2-arm64", inputs, true); err != nil || strings.Join(fetched, " ") != "/ears /lva" {
		t.Fatalf("ARM64 fetched %q: %v", fetched, err)
	}
}

type entry struct {
	name, link string
	kind       byte
	mode       int64
}

func archive(t *testing.T, entries ...entry) string {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Linkname: e.link, Typeflag: e.kind, Mode: e.mode}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if e.kind == tar.TypeReg {
			h.Size = 1
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.kind == tar.TypeReg {
			tw.Write([]byte("x"))
		}
	}
	tw.Close()
	gz.Close()
	path := filepath.Join(t.TempDir(), "source.tar.gz")
	os.WriteFile(path, buf.Bytes(), 0o644)
	return path
}

func TestArchiveCannotEscape(t *testing.T) {
	cases := map[string][]entry{
		"parent":     {{name: "../outside", kind: tar.TypeReg}},
		"absolute":   {{name: "/absolute/file", kind: tar.TypeReg}},
		"dotdot":     {{name: "source/../../outside", kind: tar.TypeReg}},
		"symlink":    {{name: "source/link", link: "../../outside", kind: tar.TypeSymlink}},
		"abssymlink": {{name: "source/link", link: "/etc", kind: tar.TypeSymlink}},
		"through": {{name: "source/up", link: "..", kind: tar.TypeSymlink},
			{name: "source/up/up2", link: "..", kind: tar.TypeSymlink}},
		"hardlink": {{name: "source/link", link: "../outside", kind: tar.TypeLink}},
		"hardlink-to-symlink": {{name: "source/a/b/l", link: "../..", kind: tar.TypeSymlink},
			{name: "source/h", link: "source/a/b/l", kind: tar.TypeLink}},
		"device": {{name: "source/tty", kind: tar.TypeChar}},
		"roots":  {{name: "source/file", kind: tar.TypeReg}, {name: "other/file", kind: tar.TypeReg}},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			path := archive(t, entries...)
			base := t.TempDir()
			_, err := unpack(path, filepath.Join(base, "out"))
			if err == nil {
				t.Fatal("accepted")
			}
			t.Log(err)
			if _, err := os.Lstat(filepath.Join(base, "outside")); !os.IsNotExist(err) {
				t.Fatal("wrote outside the destination")
			}
		})
	}
}

func TestArchiveNormalizesSymlinks(t *testing.T) {
	for name, entries := range map[string][]entry{
		"backward": {{name: "source/a/b/d", link: "../../..", kind: tar.TypeSymlink},
			{name: "source/a/b/e", link: "d/..", kind: tar.TypeSymlink}},
		"forward": {{name: "source/p", link: "q/../..", kind: tar.TypeSymlink},
			{name: "source/q", link: ".", kind: tar.TypeSymlink}},
	} {
		t.Run(name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "out")
			if _, err := unpack(archive(t, entries...), out); err != nil {
				t.Fatal(err)
			}
			out, err := filepath.EvalSymlinks(out)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				resolved, err := filepath.EvalSymlinks(filepath.Join(out, e.name))
				if err != nil {
					t.Fatal(err)
				}
				rel, err := filepath.Rel(out, resolved)
				if err != nil || !filepath.IsLocal(rel) {
					t.Fatalf("%s resolves outside the destination: %s", e.name, resolved)
				}
			}
		})
	}
}

func TestArchiveExtractsOneSourceTree(t *testing.T) {
	path := archive(t,
		entry{name: "source/", kind: tar.TypeDir, mode: 0o755},
		entry{name: "source/bin/tool", kind: tar.TypeReg, mode: 0o4777},
		entry{name: "source/file", kind: tar.TypeReg, mode: 0o444},
		entry{name: "source/dts/include", link: "../bin", kind: tar.TypeSymlink},
		entry{name: "source/hard", link: "source/file", kind: tar.TypeLink})
	root, err := unpack(path, filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "dts/include/tool")); string(data) != "x" {
		t.Fatalf("symlinked content %q", data)
	}
	for file, want := range map[string]os.FileMode{"bin/tool": 0o755, "file": 0o644, "hard": 0o644} {
		if fi, err := os.Stat(filepath.Join(root, file)); err != nil || fi.Mode() != want {
			t.Errorf("%s: %v %v, want %v", file, fi.Mode(), err, want)
		}
	}
}

func TestReplayArchiveWithReadOnlyTree(t *testing.T) {
	if _, err := exec.LookPath("xz"); err != nil {
		t.Skip("xz not available")
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range []*tar.Header{
		{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "./go-modcache/mod@v1/", Typeflag: tar.TypeDir, Mode: 0o555},
		{Name: "./go-modcache/mod@v1/go.mod", Typeflag: tar.TypeReg, Mode: 0o444, Size: 1},
		{Name: "./sources.lock.json", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1},
	} {
		tw.WriteHeader(h)
		if h.Size > 0 {
			tw.Write([]byte("x"))
		}
	}
	tw.Close()
	// Trailing zero blocks past the end marker, larger than a pipe buffer
	// (as tar --blocking-factor=2048 writes): xz must still exit.
	buf.Write(make([]byte, 2<<20))
	dir := t.TempDir()
	cmd := exec.Command("xz", "--stdout")
	cmd.Stdin = &buf
	data, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "inputs.tar.xz"), data, 0o644)
	out := filepath.Join(dir, "inputs")
	done := make(chan error, 1)
	go func() { done <- extractXZ(filepath.Join(dir, "inputs.tar.xz"), out) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("extraction did not finish")
	}
	for _, file := range []string{"go-modcache/mod@v1/go.mod", "sources.lock.json"} {
		if data, err := os.ReadFile(filepath.Join(out, file)); err != nil || string(data) != "x" {
			t.Fatalf("%s: %q %v", file, data, err)
		}
	}
	// Directory modes are not restored, so the build can clean up afterwards.
	if err := os.RemoveAll(out); err != nil {
		t.Fatal(err)
	}
}

func TestComponentArchive(t *testing.T) {
	for _, name := range []string{"nab-core", "../nab-core"} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o755, Size: 6})
			tw.Write([]byte("binary"))
			tw.Close()
			dir := t.TempDir()
			archive := filepath.Join(dir, "component.tar")
			os.WriteFile(archive, buf.Bytes(), 0o644)
			out := filepath.Join(dir, "out")
			err := run([]string{"extract", archive, out})
			if name != "nab-core" {
				if err == nil {
					t.Fatal("component archive escaped its destination")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(out, name)
			data, err := os.ReadFile(binary)
			if err != nil || string(data) != "binary" {
				t.Fatalf("component contents %q: %v", data, err)
			}
			info, err := os.Stat(binary)
			if err != nil || info.Mode().Perm() != 0o755 {
				t.Fatalf("executable mode lost: %v, %v", info, err)
			}
		})
	}
}
