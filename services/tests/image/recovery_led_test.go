package image

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecoveryLED(t *testing.T) {
	for _, order := range []string{"red green blue", "green red blue", "blue green red", "red red blue", "red blue", "red green white"} {
		t.Run(order, func(t *testing.T) {
			root := t.TempDir()
			for index := 0; index < 5; index++ {
				dir := filepath.Join(root, fmt.Sprintf("multi:indicator-%d", index))
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				for name, value := range map[string]string{"multi_index": order + "\n", "max_brightness": "255\n", "multi_intensity": "0 0 0\n", "brightness": "0\n", "sync": "0\n"} {
					write(t, filepath.Join(dir, name), value)
				}
			}
			fake := newFakes(t, root, map[string]string{"sleep": "exit 0"})
			script := strings.NewReplacer("/sys/class/leds", root, "PATH=/bin:/sbin", "PATH=\""+fake.bin+":"+os.Getenv("PATH")+"\"").Replace(read(t, filepath.Join(repo, "nix/runtime/recovery-led.sh")))
			path := filepath.Join(root, "led.sh")
			write(t, path, script)
			r := execute(t, "", nil, "sh", path, "redblink")
			invalid := order == "red red blue" || order == "red blue" || order == "red green white"
			if invalid {
				if r.code == 0 {
					t.Fatal("invalid channel mapping accepted")
				}
				return
			}
			if r.code != 0 {
				t.Fatal(r.stderr)
			}
			values := strings.Fields(order)
			for n, color := range values {
				if color == "red" {
					values[n] = "255"
				} else {
					values[n] = "0"
				}
			}
			for index := 0; index < 5; index++ {
				dir := filepath.Join(root, fmt.Sprintf("multi:indicator-%d", index))
				if got := read(t, filepath.Join(dir, "multi_intensity")); got != strings.Join(values, " ")+"\n" {
					t.Fatal(got, order)
				}
				if got := read(t, filepath.Join(dir, "brightness")); got != "0\n" {
					t.Fatal("bounded blink did not end its final pulse", got)
				}
			}
			if calls := fake.calls(t); len(calls) != 6 {
				t.Fatal("blink is not bounded to three pulses", calls)
			}
			if r := execute(t, "", nil, "sh", path, "red"); r.code != 0 {
				t.Fatal("steady red failed", r.stderr)
			}
			for index := 0; index < 5; index++ {
				if got := read(t, filepath.Join(root, fmt.Sprintf("multi:indicator-%d/brightness", index))); got != "200\n" {
					t.Fatal("steady red did not restore brightness", got)
				}
			}
			if calls := fake.calls(t); len(calls) != 6 {
				t.Fatal("steady red slept or blinked", calls)
			}
			if r := execute(t, "", nil, "sh", path, "unsupported"); r.code == 0 {
				t.Fatal("invalid action accepted")
			}
		})
	}
}
