package image

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUbootSandboxCacheNamespace(t *testing.T) {
	script := read(t, filepath.Join(imageDir, "uboot-sandbox.sh"))
	_, block, ok := strings.Cut(script, "if [[ $cc == *ccache* ]]; then\n")
	if !ok {
		t.Fatal("ccache namespace block missing")
	}
	block, _, ok = strings.Cut(block, "\nfi\n")
	if !ok {
		t.Fatal("ccache namespace block unterminated")
	}
	inputs := []string{"image/sources.lock.json", "image/uboot-sandbox.sh", "output/.config"}
	key := func(dir string) string {
		t.Helper()
		r := execute(t, "", []string{"repo=" + dir, "output=" + filepath.Join(dir, "output")},
			"bash", "-e", "-o", "pipefail", "-c", block+"\nprintf '%s\\n' \"$CCACHE_NAMESPACE\"")
		if r.code != 0 {
			t.Fatalf("namespace: %s", r.stderr)
		}
		return strings.TrimSpace(r.stdout)
	}
	var dirs []string
	for range 2 {
		dir := t.TempDir()
		dirs = append(dirs, dir)
		for _, input := range inputs {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, input)), 0o755); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(dir, input), input)
		}
	}
	want := key(dirs[0])
	if len(want) != 64 || key(dirs[1]) != want {
		t.Fatal("namespace is not a stable digest across disposable paths")
	}
	for _, input := range inputs {
		path := filepath.Join(dirs[1], input)
		write(t, path, input+" changed")
		if key(dirs[1]) == want {
			t.Errorf("%s content did not invalidate the namespace", input)
		}
		write(t, path, input)
	}
}
