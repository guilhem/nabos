package image

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNetworkManagerSourceReplay(t *testing.T) {
	work := t.TempDir()
	image := filepath.Join(work, "image")
	inputs := filepath.Join(work, "inputs")
	if err := os.MkdirAll(image, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(inputs, 0o755); err != nil {
		t.Fatal(err)
	}
	copyFile(t, filepath.Join(imageDir, "network-manager.sh"), filepath.Join(image, "network-manager.sh"))
	contents := "locked vendor source\n"
	identity := map[string]any{"files": []map[string]string{{"name": "vendor.dsc", "url": "https://invalid.example/vendor.dsc", "sha256": fmt.Sprintf("%x", sha256.Sum256([]byte(contents)))}}}
	encoded, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(image, "network-manager.lock.json")
	write(t, lock, string(encoded))
	write(t, filepath.Join(inputs, "source-identity.json"), string(encoded))
	archive := filepath.Join(inputs, "vendor.dsc")
	write(t, archive, contents)
	fakes := newFakes(t, work, map[string]string{"curl": "echo unexpected-network >&2; exit 99"})
	t.Setenv("PATH", fakes.bin+":"+os.Getenv("PATH"))
	script := filepath.Join(image, "network-manager.sh")
	run(t, "", "bash", script, "fetch", inputs, "--offline")
	for _, scenario := range []string{"missing", "corrupt", "wrong-identity"} {
		t.Run(scenario, func(t *testing.T) {
			write(t, archive, contents)
			write(t, filepath.Join(inputs, "source-identity.json"), string(encoded))
			switch scenario {
			case "missing":
				if err := os.Remove(archive); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				write(t, archive, "corrupt source")
			case "wrong-identity":
				write(t, filepath.Join(inputs, "source-identity.json"), `{}`)
			}
			r := execute(t, "", nil, "bash", script, "fetch", inputs, "--offline")
			if r.code == 0 || r.code == 99 || strings.Contains(r.stderr, "unexpected-network") {
				t.Fatalf("offline replay did not reject input locally: %+v", r)
			}
			if got := read(t, fakes.log); got != "" {
				t.Fatalf("offline replay used a network client: %s", got)
			}
		})
	}
}
