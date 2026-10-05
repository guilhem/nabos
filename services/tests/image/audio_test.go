package image

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Check resolved phandles in the shipped overlay. Cached builds retain pristine
// payload sources; compile provided patched sources only for standalone tests.
func TestVolumeOverlay(t *testing.T) {
	overlay := filepath.Join(os.Getenv("NABOS_IMAGE_OVERLAYS"), "tagtagtag-sound.dtbo")
	tools := []string{"fdtoverlay", "fdtget"}
	if os.Getenv("NABOS_IMAGE_OVERLAYS") == "" {
		tools = append(tools, "cpp", "dtc")
	}
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("NABOS_IMAGE_BOOT") != "" {
				t.Fatal(err)
			}
			t.Skipf("%s unavailable", tool)
		}
	}
	if os.Getenv("NABOS_IMAGE_OVERLAYS") == "" {
		dts := find(filepath.Join(sources, "sound"), "tagtagtag-sound-overlay.dts")
		if len(dts) != 1 {
			if os.Getenv("NABOS_IMAGE_BOOT") != "" {
				t.Fatalf("expected one patched sound overlay source, got %v", dts)
			}
			t.Skip("patched sound source unavailable (set NABOS_SOURCES)")
		}
		tmp := t.TempDir()
		overlay = filepath.Join(tmp, "sound.dtbo")
		preprocessed := run(t, "", "cpp", "-nostdinc", "-undef", "-D__DTS__", "-x", "assembler-with-cpp", "-P", dts[0])
		run(t, preprocessed, "dtc", "-@", "-I", "dts", "-O", "dtb", "-o", overlay, "-")
		// Keep the resolved-tree assertions runnable without vendor DTB downloads.
		base := filepath.Join(tmp, "fixture.dtb")
		resolved := filepath.Join(tmp, "fixture-sound.dtb")
		run(t, baseDTS, "dtc", "-@", "-I", "dts", "-O", "dtb", "-o", base, "-")
		run(t, "", "fdtoverlay", "-i", base, "-o", resolved, overlay)
		checkVolumeTree(t, resolved)
	}
	for _, target := range []string{"zero-armv6", "zero2-arm64"} {
		t.Run(target, func(t *testing.T) {
			base := filepath.Join(vendorDTBs, dtbs[target])
			if _, err := os.Stat(base); err != nil {
				if os.Getenv("NABOS_IMAGE_TARGET") == target {
					t.Fatal(err)
				}
				t.Skipf("vendor DTB unavailable: %s", base)
			}
			tree := filepath.Join(t.TempDir(), "sound.dtb")
			run(t, "", "fdtoverlay", "-i", base, "-o", tree, overlay)
			checkVolumeTree(t, tree)
		})
	}
}

func checkVolumeTree(t *testing.T, tree string) {
	t.Helper()
	get := func(kind, node, property string) string {
		return strings.TrimSpace(run(t, "", "fdtget", "-t", kind, tree, node, property))
	}
	want := func(kind, node, property, expected string) {
		t.Helper()
		if got := get(kind, node, property); got != expected {
			t.Errorf("%s %s = %q, want %q", node, property, got, expected)
		}
	}
	var keys []string
	var walk func(string)
	walk = func(node string) {
		compatible := strings.Fields(strings.TrimSpace(run(t, "", "fdtget", "-d", "", "-t", "s", tree, node, "compatible")))
		for _, value := range compatible {
			if value == "linux,volume-gpio" {
				t.Errorf("removed volume driver remains at %s", node)
			}
			if value == "gpio-keys" && strings.TrimSpace(run(t, "", "fdtget", "-d", "", tree, node, "label")) == "tagtagtag-volume" {
				keys = append(keys, node)
			}
		}
		for _, child := range strings.Fields(run(t, "", "fdtget", "-l", tree, node)) {
			walk(strings.TrimRight(node, "/") + "/" + child)
		}
	}
	walk("/")
	if len(keys) != 1 {
		t.Fatalf("expected one tagtagtag-volume gpio-keys device, got %v", keys)
	}
	gpio := get("s", "/__symbols__", "gpio")
	gpioRef := get("x", gpio, "phandle")
	children := strings.Fields(run(t, "", "fdtget", "-l", tree, keys[0]))
	if len(children) != 2 {
		t.Fatalf("expected two volume keys, got %v", children)
	}
	seen := map[string]bool{}
	for _, child := range children {
		node := keys[0] + "/" + child
		code := get("x", node, "linux,code")
		pin, ok := map[string]int{"100": 27, "101": 22}[code]
		if !ok || seen[code] {
			t.Fatalf("unexpected or duplicate volume key code %s", code)
		}
		seen[code] = true
		want("x", node, "gpios", fmt.Sprintf("%s %x 0", gpioRef, pin))
		want("u", node, "debounce-interval", "10")
		if got := strings.TrimSpace(run(t, "", "fdtget", "-d", "1", "-t", "u", tree, node, "linux,input-type")); got != "1" {
			t.Errorf("%s input type %s, want EV_KEY", node, got)
		}
	}
	var volumePins []string
	for i, pin := range []string{"27", "22"} {
		node := get("s", "/__symbols__", fmt.Sprintf("gpio_physical_volume_%d_pin", i+1))
		want("u", node, "brcm,pins", pin)
		want("u", node, "brcm,function", "0")
		volumePins = append(volumePins, get("x", node, "phandle"))
	}
	want("x", keys[0], "pinctrl-0", strings.Join(volumePins, " "))
	want("s", keys[0], "pinctrl-names", "default")
	sound := get("s", "/__symbols__", "sound")
	i2s := get("s", "/__symbols__", "i2s")
	i2c := get("s", "/__symbols__", "i2c1")
	codec := get("s", "/__symbols__", "wm8960")
	amp := get("s", "/__symbols__", "max9759")
	want("s", sound, "status", "okay")
	want("s", sound, "simple-audio-card,name", "tagtagtag-sound")
	want("s", sound, "simple-audio-card,format", "i2s")
	want("x", sound, "simple-audio-card,aux-devs", get("x", amp, "phandle"))
	want("x", sound, "simple-audio-card,hp-det-gpio", gpioRef+" 19 0")
	want("s", i2s, "status", "okay")
	want("s", i2c, "status", "okay")
	want("s", codec, "compatible", "wlf,wm8960")
	want("x", codec, "reg", "1a")
	for property, symbol := range map[string]string{"AVDD-supply": "vdd_5v0_reg", "DVDD-supply": "vdd_3v3_reg"} {
		want("x", codec, property, get("x", get("s", "/__symbols__", symbol), "phandle"))
	}
	want("s", amp, "compatible", "maxim,max9759")
	want("x", amp, "shutdown-gpios", gpioRef+" 7 1")
	want("x", amp, "mute-gpios", gpioRef+" 8 1")
	want("x", sound+"/simple-audio-card,cpu", "sound-dai", get("x", i2s, "phandle"))
	want("x", sound+"/simple-audio-card,codec", "sound-dai", get("x", codec, "phandle"))
	clock := get("s", "/__symbols__", "wm8960_mclk")
	want("u", clock, "clock-frequency", "12000000")
	want("x", sound+"/simple-audio-card,codec", "clocks", get("x", clock, "phandle"))
	want("s", sound+"/simple-audio-card,codec", "clock-names", "mclk")
	jack := get("s", "/__symbols__", "gpio_line_out_detect_pin")
	want("x", sound, "pinctrl-0", get("x", jack, "phandle"))
	want("u", jack, "brcm,pins", "25")
	want("u", jack, "brcm,function", "0")
	want("u", jack, "brcm,pull", "2")
	want("s", sound, "simple-audio-card,routing", "Headphones OUT3 Speaker (tag) SPK_RP Speaker (tag) SPK_RN INL HP_L INR HP_R Speaker (tagtag) OUTL Speaker (tagtag) OUTR LINPUT1 Onboard Left Microphone RINPUT1 Onboard Right Microphone LINPUT1 External Microphone LINPUT2 External Microphone")
}
