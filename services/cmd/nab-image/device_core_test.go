package main

import (
	"archive/tar"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeviceCoreArtifactIdentity(t *testing.T) {
	for _, target := range []string{"zero-armv6", "zero2-arm64"} {
		for _, corrupt := range []string{"", "target", "nabos-revision", "external-revision", "identity", "archive", "lock", "binary", "architecture"} {
			t.Run(target+"/"+corrupt, func(t *testing.T) {
				dir := t.TempDir()
				inputs := filepath.Join(dir, "inputs/device-core")
				if err := os.MkdirAll(filepath.Join(inputs, "cargo-vendor"), 0o755); err != nil {
					t.Fatal(err)
				}
				put := func(name string, data []byte) {
					t.Helper()
					if err := os.WriteFile(name, data, 0o644); err != nil {
						t.Fatal(err)
					}
				}
				data, err := os.ReadFile(archive(t, entry{name: "source/Cargo.lock", kind: tar.TypeReg}))
				if err != nil {
					t.Fatal(err)
				}
				put(filepath.Join(inputs, "device_core.tar.gz"), data)
				hash, err := digest(filepath.Join(inputs, "device_core.tar.gz"))
				if err != nil {
					t.Fatal(err)
				}
				commit := strings.Repeat("a", 40)
				identity := sourceLock{"example/device-core", commit, "https://codeload.github.com/example/device-core/tar.gz/" + commit, hash}
				locked, err := json.Marshal(map[string]any{"targets": map[string]any{"zero-armv6": map[string]any{}, "zero2-arm64": map[string]any{}}, "sources": map[string]sourceLock{"device_core": identity}})
				if err != nil {
					t.Fatal(err)
				}
				lock := filepath.Join(dir, "sources.lock.json")
				put(lock, locked)
				encoded, err := json.Marshal(identity)
				if err != nil {
					t.Fatal(err)
				}
				put(filepath.Join(inputs, "source-identity.json"), encoded)
				put(filepath.Join(inputs, "Cargo.lock"), []byte("x"))
				info := target + "\nnabos-revision\n" + commit + "\n"
				put(filepath.Join(dir, "build-info"), []byte(info))
				// Minimal ELF headers exercise the real parser for both targets.
				elf := make([]byte, 64)
				copy(elf, []byte{0x7f, 'E', 'L', 'F', 1, 1, 1})
				binary.LittleEndian.PutUint16(elf[16:], 2)
				binary.LittleEndian.PutUint16(elf[18:], 40)
				binary.LittleEndian.PutUint32(elf[20:], 1)
				if target == "zero2-arm64" {
					elf[4] = 2
					binary.LittleEndian.PutUint16(elf[18:], 183)
				}
				if corrupt == "architecture" {
					binary.LittleEndian.PutUint16(elf[18:], 62)
				}
				put(filepath.Join(dir, "device-core"), elf)
				hash, err = digest(filepath.Join(dir, "device-core"))
				if err != nil {
					t.Fatal(err)
				}
				put(filepath.Join(dir, "binary.sha256"), []byte(hash+"\n"))
				switch corrupt {
				case "target":
					put(filepath.Join(dir, "build-info"), []byte(strings.Replace(info, target, "other-target", 1)))
				case "nabos-revision":
					put(filepath.Join(dir, "build-info"), []byte(strings.Replace(info, "nabos-revision", "another-revision", 1)))
				case "external-revision":
					put(filepath.Join(dir, "build-info"), []byte(strings.Replace(info, commit, strings.Repeat("b", 40), 1)))
				case "identity":
					put(filepath.Join(inputs, "source-identity.json"), []byte(`{}`))
				case "archive":
					put(filepath.Join(inputs, "device_core.tar.gz"), []byte("wrong source"))
				case "lock":
					put(filepath.Join(inputs, "Cargo.lock"), []byte("other lock"))
				case "binary":
					put(filepath.Join(dir, "device-core"), []byte("wrong binary"))
				}
				if err := verifyDeviceCore(lock, dir, target, "nabos-revision"); (err == nil) != (corrupt == "") {
					t.Fatalf("corrupt=%q: %v", corrupt, err)
				}
			})
		}
	}
}

func TestDeviceCorePinMustBeImmutable(t *testing.T) {
	commit := strings.Repeat("a", 40)
	s := sourceLock{"example/device-core", commit, "https://codeload.github.com/example/device-core/tar.gz/" + commit, strings.Repeat("b", 64)}
	for _, field := range []string{"", "commit", "url", "sha256", "repository"} {
		c := s
		switch field {
		case "commit":
			c.Commit = "PENDING_PARENT_REMOTE_COMMIT"
		case "url":
			c.URL = "https://codeload.github.com/example/device-core/tar.gz/main"
		case "sha256":
			c.SHA256 = "PENDING_PARENT_ARCHIVE_SHA256"
		case "repository":
			c.Repository = "../device-core"
		}
		lock := lockFile{Sources: map[string]sourceLock{"device_core": c}}
		if _, err := deviceCoreSource(lock); (err == nil) != (field == "") {
			t.Fatal(fmt.Sprintf("field=%s: %v", field, err))
		}
	}
}
