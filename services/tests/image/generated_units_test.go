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
		voice, fail  bool
	}{
		{"armv6", "zero-armv6", false, false},
		{"arm64-with-voice", "zero2-arm64", true, false},
		{"arm64-without-voice", "zero2-arm64", false, false},
		{"voice-on-armv6", "zero-armv6", true, false},
		{"invalid-unit", "zero-armv6", false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			tmp := t.TempDir()
			root := filepath.Join(tmp, "root")
			for _, path := range []string{"etc/systemd/system", "etc/systemd/user"} {
				if err := os.MkdirAll(filepath.Join(root, path), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if c.voice {
				write(t, filepath.Join(root, "etc/systemd/system/linux-voice-assistant.service"), "[Service]\n")
			}
			body := `test "${SYSTEMD_UNIT_PATH-}" != /must-not-use-host-units`
			if c.fail {
				body = "exit 1"
			}
			fake := newFakes(t, tmp, map[string]string{"systemd-analyze": body})
			r := execute(t, "", fake.env("SYSTEMD_UNIT_PATH=/must-not-use-host-units"), "bash",
				filepath.Join(imageDir, "test-generated-units.sh"), c.target, root)
			if c.fail || c.voice && c.target == "zero-armv6" {
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
				for _, flag := range []string{"verify", "--root=" + root, "--man=no", "--generators=no", "--recursive-errors=yes"} {
					if !strings.Contains(call, flag) {
						t.Errorf("missing %s: %s", flag, call)
					}
				}
			}
			if !strings.Contains(calls[0], "user@1004.service") || !strings.Contains(calls[1], "pipewire.socket") ||
				strings.Contains(calls[0], "linux-voice-assistant.service") != c.voice {
				t.Fatalf("incomplete generated-unit scope: %v", calls)
			}
		})
	}
}
