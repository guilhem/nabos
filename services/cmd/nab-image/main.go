// Command nab-image runs the host-side image build steps: locked input
// queries, verified downloads, safe archive extraction and driver checks.
package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const usage = `usage:
  nab-image get LOCK KEY...                        print string values at dotted keys
  nab-image fetch LOCK TARGET DIR [--sources-only] download and verify locked inputs
  nab-image unpack LOCK INPUTS SRC                 extract locked source archives to SRC/NAME
  nab-image source LOCK NAME INPUTS SRC             verify and extract one pinned source
  nab-image verify-device-core LOCK DIR TARGET REV verify external component identity
  nab-image extract ARCHIVE.tar[.xz] DIR           extract component or replay inputs
  nab-image drivers LOCK --archives DIR [--kernel KERNEL]
                                                   patch and build driver overlays (and modules)`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "nab-image:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	cmd, args := args[0], args[1:]
	switch {
	case cmd == "get" && len(args) >= 2:
		return get(args[0], args[1:])
	case cmd == "fetch" && (len(args) == 3 || len(args) == 4 && args[3] == "--sources-only"):
		return fetch(http.DefaultClient, args[0], args[1], args[2], len(args) == 4)
	case cmd == "unpack" && len(args) == 3:
		return unpackSources(args[0], args[1], args[2])
	case cmd == "source" && len(args) == 4:
		return source(http.DefaultClient, args[0], args[1], args[2], args[3])
	case cmd == "verify-device-core" && len(args) == 4:
		return verifyDeviceCore(args[0], args[1], args[2], args[3])
	case cmd == "extract" && len(args) == 2:
		return extractArchive(args[0], args[1])
	case cmd == "drivers" && len(args) >= 3:
		return drivers(args[0], args[1:])
	}
	return errors.New(usage)
}

type lockFile struct {
	Targets map[string]struct {
		ImageURL    string `json:"image_url"`
		ImageSHA256 string `json:"image_sha256"`
	}
	Sources map[string]sourceLock
}

type sourceLock struct {
	Repository string `json:"repository"`
	Commit     string `json:"commit"`
	URL        string `json:"url"`
	SHA256     string `json:"sha256"`
}

func deviceCoreSource(lock lockFile) (sourceLock, error) {
	s, ok := lock.Sources["device_core"]
	if !ok || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(s.Commit) ||
		!sha256Hex.MatchString(s.SHA256) || s.URL != "https://codeload.github.com/"+s.Repository+"/tar.gz/"+s.Commit ||
		!regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(s.Repository) {
		return s, errors.New("device_core source pin is pending or invalid: commit archive and SHA-256 required")
	}
	return s, nil
}

func source(client *http.Client, lockPath, name, inputs, src string) error {
	lock, err := readLock(lockPath)
	if err != nil {
		return err
	}
	s, ok := lock.Sources[name]
	if !ok {
		return fmt.Errorf("unknown source %q", name)
	}
	if name == "device_core" {
		if s, err = deviceCoreSource(lock); err != nil {
			return err
		}
	}
	archive := filepath.Join(inputs, name+".tar.gz")
	if err := download(client, s.URL, s.SHA256, archive); err != nil {
		return err
	}
	root, err := unpack(archive, src)
	if err != nil {
		return err
	}
	fmt.Println(root)
	return nil
}

// Artifact provenance is independent of the NabOS revision. Verify it before
// assembling an image, including when CI supplied prebuilt components.
func verifyDeviceCore(lockPath, dir, target, revision string) error {
	lock, err := readLock(lockPath)
	if err != nil {
		return err
	}
	s, err := deviceCoreSource(lock)
	if err != nil {
		return err
	}
	if _, ok := lock.Targets[target]; !ok {
		return fmt.Errorf("unknown target %q", target)
	}
	info, err := os.ReadFile(filepath.Join(dir, "build-info"))
	if err != nil {
		return err
	}
	if string(info) != target+"\n"+revision+"\n"+s.Commit+"\n" {
		return errors.New("device-core target or source revision mismatch")
	}
	data, err := os.ReadFile(filepath.Join(dir, "inputs/device-core/source-identity.json"))
	if err != nil {
		return err
	}
	var identity sourceLock
	if err := json.Unmarshal(data, &identity); err != nil {
		return err
	}
	if identity != s {
		return errors.New("device-core source identity mismatch")
	}
	hash, err := digest(filepath.Join(dir, "inputs/device-core/device_core.tar.gz"))
	if err != nil {
		return err
	}
	if hash != s.SHA256 {
		return errors.New("device-core source archive checksum mismatch")
	}
	stage, err := os.MkdirTemp("", "device-core-identity-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	root, err := unpack(filepath.Join(dir, "inputs/device-core/device_core.tar.gz"), stage)
	if err != nil {
		return err
	}
	archivedLock, err := os.ReadFile(filepath.Join(dir, "inputs/device-core/Cargo.lock"))
	if err != nil {
		return err
	}
	sourceLock, err := os.ReadFile(filepath.Join(root, "Cargo.lock"))
	if err != nil {
		return err
	}
	if string(archivedLock) != string(sourceLock) {
		return errors.New("device-core Cargo.lock does not match pinned source")
	}
	for _, file := range []string{"Cargo.lock", "cargo-vendor"} {
		if _, err := os.Stat(filepath.Join(dir, "inputs/device-core", file)); err != nil {
			return err
		}
	}
	hash, err = digest(filepath.Join(dir, "device-core"))
	if err != nil {
		return err
	}
	expected, err := os.ReadFile(filepath.Join(dir, "binary.sha256"))
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(expected)) != hash {
		return errors.New("device-core binary checksum mismatch")
	}
	binary, err := elf.Open(filepath.Join(dir, "device-core"))
	if err != nil {
		return err
	}
	defer binary.Close()
	if target == "zero-armv6" && (binary.Machine != elf.EM_ARM || binary.Class != elf.ELFCLASS32) ||
		target == "zero2-arm64" && (binary.Machine != elf.EM_AARCH64 || binary.Class != elf.ELFCLASS64) {
		return errors.New("device-core ELF architecture mismatch")
	}
	return nil
}

func readLock(name string) (lockFile, error) {
	var lock lockFile
	data, err := os.ReadFile(name)
	if err == nil {
		err = json.Unmarshal(data, &lock)
	}
	return lock, err
}

// get prints one line per dotted key; every value must be a string.
func get(lockPath string, keys []string) error {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return err
	}
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		return err
	}
	for _, key := range keys {
		v := doc
		for _, part := range strings.Split(key, ".") {
			m, _ := v.(map[string]any)
			v = m[part]
		}
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("%s: no string value for %s", lockPath, key)
		}
		fmt.Println(s)
	}
	return nil
}

func fetch(client *http.Client, lockPath, target, dir string, sourcesOnly bool) error {
	lock, err := readLock(lockPath)
	if err != nil {
		return err
	}
	t, ok := lock.Targets[target]
	if !ok {
		return fmt.Errorf("unknown target %q", target)
	}
	if _, ok := lock.Sources["device_core"]; ok {
		if _, err := deviceCoreSource(lock); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	if !sourcesOnly {
		if err := download(client, t.ImageURL, t.ImageSHA256, filepath.Join(dir, "raspios.img.xz")); err != nil {
			return err
		}
	}
	for name, s := range lock.Sources {
		// Linux Voice Assistant is only installed on the ARM64 image.
		if name == "lva" && target != "zero2-arm64" {
			continue
		}
		dest := filepath.Join(dir, name+".tar.gz")
		if name == "device_core" {
			dest = filepath.Join(dir, "device-core", name+".tar.gz")
		}
		if err := download(client, s.URL, s.SHA256, dest); err != nil {
			return err
		}
	}
	fmt.Printf("Verified inputs for %s\n", target)
	return nil
}

func digest(name string) (string, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

var (
	sha256Hex   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	retryDelay  = time.Second
	idleTimeout = 120 * time.Second
)

// download replaces dest only with content matching expected; an already
// verified dest is kept without network access.
func download(client *http.Client, url, expected, dest string) error {
	if !strings.HasPrefix(url, "https://") || !sha256Hex.MatchString(expected) {
		return errors.New("input requires HTTPS and a SHA-256 digest")
	}
	if d, err := digest(dest); err == nil && d == expected {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o777); err != nil {
		return err
	}
	part := dest + ".part"
	var err error
	for attempt := range 3 {
		time.Sleep(time.Duration(attempt) * retryDelay)
		if err = downloadOnce(client, url, expected, part); err == nil {
			return os.Rename(part, dest)
		}
		os.Remove(part)
	}
	return err
}

func downloadOnce(client *http.Client, url, expected, part string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Abort a stalled transfer, not a slow but progressing one.
	idle := time.AfterFunc(idleTimeout, cancel)
	defer idle.Stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	f, err := os.Create(part)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), readerFunc(func(p []byte) (int, error) {
		idle.Reset(idleTimeout)
		return resp.Body.Read(p)
	}))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && hex.EncodeToString(h.Sum(nil)) != expected {
		err = fmt.Errorf("checksum mismatch: %s", url)
	}
	return err
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// unpack extracts a source archive with exactly one top-level directory and
// returns that directory.
func unpack(archive, dir string) (string, error) {
	f, err := os.Open(archive)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	top, err := extract(tar.NewReader(gz), dir, true)
	if err != nil {
		return "", fmt.Errorf("%s: %w", archive, err)
	}
	root := filepath.Join(dir, top)
	if fi, err := os.Lstat(root); top == "" || err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%s: source archive root must be a directory", archive)
	}
	return root, nil
}

func unpackSources(lockPath, inputs, src string) error {
	lock, err := readLock(lockPath)
	if err != nil {
		return err
	}
	for name := range lock.Sources {
		archive := filepath.Join(inputs, name+".tar.gz")
		if name == "device_core" {
			archive = filepath.Join(inputs, "device-core", name+".tar.gz")
		}
		if _, err := os.Stat(archive); errors.Is(err, os.ErrNotExist) {
			continue
		}
		stage := filepath.Join(src, "."+name)
		root, err := unpack(archive, stage)
		if err != nil {
			return err
		}
		if err := os.Rename(root, filepath.Join(src, name)); err != nil {
			return err
		}
		if err := os.Remove(stage); err != nil {
			return err
		}
	}
	return nil
}

func extractArchive(archive, dir string) error {
	if strings.HasSuffix(archive, ".xz") {
		return extractXZ(archive, dir)
	}
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = extract(tar.NewReader(f), dir, false)
	return err
}

// extractXZ extracts a replay archive; xz itself only decompresses.
func extractXZ(archive, dir string) error {
	cmd := exec.Command("xz", "--decompress", "--stdout", archive)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	_, err = extract(tar.NewReader(out), dir, false)
	if err == nil {
		// tar stops at the end marker; xz blocks until its padding is read.
		_, err = io.Copy(io.Discard, out)
	}
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return fmt.Errorf("%s: %w", archive, err)
	}
	return cmd.Wait()
}

var errEscape = errors.New("path escapes the destination")

// extract applies the rules of Python's tarfile "data" filter: every path
// and link target stays inside dir (following links already extracted),
// no device files, ownership ignored, no setuid or group/other write bits.
func extract(tr *tar.Reader, dir string, singleRoot bool) (string, error) {
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	type dirTime struct {
		name string
		t    time.Time
	}
	var dirs []dirTime
	top := ""
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		name := path.Clean(h.Name)
		if path.IsAbs(h.Name) || name == ".." || strings.HasPrefix(name, "../") {
			return "", fmt.Errorf("%s: %w", h.Name, errEscape)
		}
		if name == "." {
			if h.Typeflag != tar.TypeDir {
				return "", fmt.Errorf("%s: not a directory", h.Name)
			}
			continue
		}
		first, _, _ := strings.Cut(name, "/")
		if singleRoot && top != "" && first != top {
			return "", errors.New("source archive must have exactly one root directory")
		}
		top = first
		if h.Typeflag != tar.TypeDir {
			if err := root.MkdirAll(path.Dir(name), 0o777); err != nil {
				return "", err
			}
			if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
				return "", err
			}
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o777); err != nil {
				return "", err
			}
			dirs = append(dirs, dirTime{name, h.ModTime})
		case tar.TypeReg, tar.TypeGNUSparse:
			mode := h.FileInfo().Mode().Perm() & 0o755
			if mode&0o100 == 0 {
				mode &^= 0o011
			}
			if err := writeFile(root, name, tr, mode|0o600, h.ModTime); err != nil {
				return "", err
			}
		case tar.TypeSymlink:
			h.Linkname = path.Clean(h.Linkname)
			if path.IsAbs(h.Linkname) || within(root, "", path.Dir(name)+"/"+h.Linkname, 0) != nil {
				return "", fmt.Errorf("%s -> %s: %w", h.Name, h.Linkname, errEscape)
			}
			if err := root.Symlink(h.Linkname, name); err != nil {
				return "", err
			}
		case tar.TypeLink:
			if path.IsAbs(h.Linkname) || within(root, "", h.Linkname, 0) != nil {
				return "", fmt.Errorf("%s => %s: %w", h.Name, h.Linkname, errEscape)
			}
			fi, err := root.Lstat(path.Clean(h.Linkname))
			if err != nil {
				return "", err
			}
			if !fi.Mode().IsRegular() {
				return "", fmt.Errorf("%s => %s: hard link target must be a regular file", h.Name, h.Linkname)
			}
			if err := root.Link(path.Clean(h.Linkname), name); err != nil {
				return "", err
			}
		default:
			return "", fmt.Errorf("%s: unsupported file type %q", h.Name, h.Typeflag)
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := root.Chtimes(dirs[i].name, dirs[i].t, dirs[i].t); err != nil {
			return "", err
		}
	}
	return top, nil
}

func writeFile(root *os.Root, name string, r io.Reader, mode os.FileMode, mtime time.Time) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = root.Chmod(name, mode)
	}
	if err == nil {
		err = root.Chtimes(name, mtime, mtime)
	}
	return err
}

// within resolves rel below base like realpath, following symlinks that
// already exist in root, and fails if the result leaves root.
func within(root *os.Root, base, rel string, depth int) error {
	if depth > 40 {
		return errEscape
	}
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		switch p {
		case "", ".":
		case "..":
			if base == "" {
				return errEscape
			}
			if base = path.Dir(base); base == "." {
				base = ""
			}
		default:
			next := path.Join(base, p)
			target, err := root.Readlink(next)
			if err != nil {
				base = next
				continue
			}
			if path.IsAbs(target) {
				return errEscape
			}
			return within(root, base, target+"/"+strings.Join(parts[i+1:], "/"), depth+1)
		}
	}
	return nil
}
