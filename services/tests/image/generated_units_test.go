package image

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGeneratedUnitVerifier(t *testing.T) {
	for _, c := range []struct {
		name, target string
		fail         bool
	}{
		{"armv6", "zero-armv6", false},
		{"arm64", "zero2-arm64", false},
		{"invalid-unit", "zero-armv6", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			tmp := t.TempDir()
			root := filepath.Join(tmp, "root")
			if err := os.MkdirAll(filepath.Join(root, "etc/systemd"), 0o755); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"system", "user"} {
				if err := os.Symlink("/nix/store/unavailable-on-host-"+name, filepath.Join(root, "etc/systemd", name)); err != nil {
					t.Fatal(err)
				}
			}
			body := `test "${SYSTEMD_UNIT_PATH-}" = /etc/systemd/system || test "${SYSTEMD_UNIT_PATH-}" = /etc/systemd/user:/etc/systemd/system`
			if c.fail {
				body = "exit 1"
			}
			fake := newFakes(t, tmp, map[string]string{"chroot": body})
			r := execute(t, "", fake.env("SYSTEMD_UNIT_PATH=/must-not-use-host-units"), "bash",
				filepath.Join(imageDir, "test-generated-units.sh"), c.target, root)
			if c.fail {
				if r.code == 0 {
					t.Fatal("invalid generated units accepted")
				}
				return
			}
			if r.code != 0 {
				t.Fatal(r.stderr)
			}
			calls := fake.calls(t)
			if len(calls) != 2 {
				t.Fatalf("expected system and user verification: %v", calls)
			}
			for _, call := range calls {
				for _, flag := range []string{root, "/run/current-system/systemd/bin/systemd-analyze", "verify", "--man=no", "--generators=no", "--recursive-errors=yes"} {
					if !strings.Contains(call, flag) {
						t.Errorf("missing %s: %s", flag, call)
					}
				}
			}
			if !strings.Contains(calls[0], "user@1004.service") || !strings.Contains(calls[1], "pipewire.socket") ||
				strings.Contains(calls[0], "linux-voice-assistant.service") != (c.target == "zero2-arm64") {
				t.Fatalf("incomplete generated-unit scope: %v", calls)
			}
		})
	}
}
