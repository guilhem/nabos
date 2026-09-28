package image

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func sizes(t *testing.T, text string) int {
	units := map[byte]int{'K': 1024, 'M': MiB, 'G': 1024 * MiB}
	if u, ok := units[text[len(text)-1]]; ok {
		n, err := strconv.Atoi(text[:len(text)-1])
		if err != nil {
			t.Fatal(err)
		}
		return n * u
	}
	n, err := strconv.ParseInt(text, 0, 64)
	if err != nil {
		t.Fatal(err)
	}
	return int(n)
}

func genimagePartitions(t *testing.T) ([]string, map[string]map[string]string) {
	cfg := read(t, filepath.Join(imageDir, "genimage.cfg"))
	var names []string
	parts := map[string]map[string]string{}
	field := regexp.MustCompile(`([\w-]+) = "?([^"\n]+)"?`)
	for _, m := range regexp.MustCompile(`(?s)partition ([\w-]+) \{(.*?)\}`).FindAllStringSubmatch(cfg, -1) {
		names = append(names, m[1])
		parts[m[1]] = map[string]string{}
		for _, f := range field.FindAllStringSubmatch(m[2], -1) {
			parts[m[1]][f[1]] = f[2]
		}
	}
	return names, parts
}

// The same partition and environment constants appear in several files.
func TestCardLayout(t *testing.T) {
	names, parts := genimagePartitions(t)
	if !slices.Equal(names, []string{"uboot-env", "uboot-env-redund", "boot", "rootfs-a", "rootfs-b", "data"}) {
		t.Fatalf("partitions %q", names)
	}
	if parts["uboot-env"]["in-partition-table"] != "false" || parts["uboot-env-redund"]["in-partition-table"] != "false" {
		t.Error("environment copies must stay out of the partition table")
	}
	if sizes(t, parts["boot"]["offset"]) != 4*MiB {
		t.Error("boot offset")
	}
	for name, size := range map[string]int{"boot": 256 * MiB, "rootfs-a": 6144 * MiB, "rootfs-b": 6144 * MiB, "data": 1024 * MiB} {
		if got := sizes(t, parts[name]["size"]); got != size {
			t.Errorf("%s size %d", name, got)
		}
	}
	if parts["rootfs-a"]["image"] != "rootfs.ext4" {
		t.Error("slot A image")
	}
	if _, ok := parts["rootfs-b"]["image"]; ok {
		t.Error("slot B must ship empty")
	}
	images := map[string]bool{}
	for _, name := range []string{"boot", "data", "uboot-env", "uboot-env-redund"} {
		images[parts[name]["image"]] = true
	}
	if !reflect.DeepEqual(images, map[string]bool{"boot.vfat": true, "data.ext4": true, "uboot.env": true}) {
		t.Errorf("images %v", images)
	}
	if end := 4*MiB + (256+2*6144+1024)*MiB; end >= 15_000_000_000 {
		t.Error("does not fit the smallest \"16 GB\" cards")
	}
}

func TestEnvironmentLocationAgreesEverywhere(t *testing.T) {
	ubootConfig := read(t, filepath.Join(bootDir, "uboot.config"))
	config := map[string]string{}
	for _, line := range strings.Split(ubootConfig, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && strings.HasPrefix(line, "CONFIG_") {
			config[k] = v
		}
	}
	var fwEnv [][]string
	for _, line := range strings.Split(read(t, filepath.Join(rootfsDir, "etc/fw_env.config")), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			fwEnv = append(fwEnv, strings.Fields(line))
		}
	}
	hex := func(s string) int {
		n, err := strconv.ParseInt(s, 0, 64)
		if err != nil {
			t.Fatal(err)
		}
		return int(n)
	}
	offsets := []int{hex(config["CONFIG_ENV_OFFSET"]), hex(config["CONFIG_ENV_OFFSET_REDUND"])}
	size := hex(config["CONFIG_ENV_SIZE"])
	if !slices.Equal(offsets, []int{0x100000, 0x200000}) || size != 0x10000 {
		t.Fatalf("offsets %#x size %#x", offsets, size)
	}
	_, parts := genimagePartitions(t)
	for i, name := range []string{"uboot-env", "uboot-env-redund"} {
		if sizes(t, parts[name]["offset"]) != offsets[i] {
			t.Errorf("%s offset", name)
		}
	}
	want := [][]string{{"/dev/mmcblk0", fmt.Sprintf("%#x", offsets[0]), fmt.Sprintf("%#x", size)},
		{"/dev/mmcblk0", fmt.Sprintf("%#x", offsets[1]), fmt.Sprintf("%#x", size)}}
	if !reflect.DeepEqual(fwEnv, want) {
		t.Errorf("fw_env.config %q", fwEnv)
	}
	if offsets[1]+size > sizes(t, parts["boot"]["offset"]) {
		t.Error("environment overlaps the boot partition")
	}
	if config["CONFIG_ENV_REDUNDANT"] != "y" || !strings.Contains(ubootConfig, "# CONFIG_ENV_IS_IN_FAT is not set") {
		t.Error("environment must be redundant and outside FAT")
	}
}

func TestAttemptsAndSlotsAgree(t *testing.T) {
	conf := read(t, filepath.Join(rootfsDir, "etc/rauc/system.conf"))
	for _, want := range []string{"boot-attempts=3", "boot-attempts-primary=3",
		"device=/dev/mmcblk0p2\ntype=ext4\nbootname=A", "device=/dev/mmcblk0p3\ntype=ext4\nbootname=B"} {
		if !strings.Contains(conf, want) {
			t.Errorf("system.conf lacks %q", want)
		}
	}
	if !strings.Contains(read(t, filepath.Join(bootDir, "boot.cmd")), "setenv BOOT_A_LEFT 3\nsetenv BOOT_B_LEFT 3\nsaveenv\nreset") {
		t.Error("boot.cmd does not restore 3 attempts")
	}
	env := ubootEnv(t)
	if env["BOOT_ORDER"] != "A B" || env["BOOT_A_LEFT"] != "3" || env["BOOT_B_LEFT"] != "0" {
		t.Errorf("uboot.env %v", env)
	}
}

func TestStoredEnvironmentMatchesPinnedBoardDefaults(t *testing.T) {
	rpiEnv := find(filepath.Join(sources, "uboot"), "board/raspberrypi/rpi/rpi.env")
	if len(rpiEnv) == 0 {
		t.Skip("pinned U-Boot sources not available")
	}
	env := ubootEnv(t)
	for _, m := range regexp.MustCompile(`(?m)^(\w+)=(0x[0-9a-fA-F]+)$`).FindAllStringSubmatch(read(t, rpiEnv[0]), -1) {
		if env[m[1]] != m[2] {
			t.Errorf("%s=%q, pinned %q", m[1], env[m[1]], m[2])
		}
	}
	if !strings.Contains(env["bootcmd"], "boot.scr") {
		t.Error("bootcmd does not load boot.scr")
	}
}

func TestOverlayScriptsAreExecutable(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join(rootfsDir, "usr/lib/nabos"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if fi, err := e.Info(); err != nil || fi.Mode()&0o111 == 0 {
			t.Errorf("%s is not executable", e.Name())
		}
	}
}

func TestNoOverlayFileIsGitIgnored(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		t.Skip("not a git checkout")
	}
	var files []string
	filepath.WalkDir(rootfsDir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			files = append(files, must(filepath.Rel(repo, p)))
		}
		return nil
	})
	if r := execute(t, "", nil, "git", append([]string{"-C", repo, "check-ignore"}, files...)...); r.stdout != "" {
		t.Errorf("ignored overlay files:\n%s", r.stdout)
	}
}

func TestBuiltinResourcesAreSelfContained(t *testing.T) {
	assets := filepath.Join(repo, "assets")
	sounds := filepath.Join(assets, "sounds")
	patterns := []string{"system/abort.wav", "clock/signature.mp3", "weather/today.mp3", "weather/sky/*.mp3",
		"sleep/*.mp3", "wakeup/*.mp3"}
	for hour := range 24 {
		patterns = append(patterns, fmt.Sprintf("clock/%d/*.mp3", hour))
	}
	exists := func(pattern string) bool {
		matches, _ := filepath.Glob(pattern)
		for _, m := range matches {
			if fi, err := os.Stat(m); err == nil && fi.Mode().IsRegular() {
				return true
			}
		}
		return false
	}
	for _, p := range patterns {
		if !exists(filepath.Join(sounds, "fr_FR", p)) && !exists(filepath.Join(sounds, p)) {
			t.Errorf("missing sound %s", p)
		}
	}
	for _, p := range []string{"system/rfid.chor", "system/streaming/*.chor", "taichi/*.chor"} {
		if !exists(filepath.Join(assets, "choreographies", p)) {
			t.Errorf("missing choreography %s", p)
		}
	}
	real := must(filepath.EvalSymlinks(assets))
	filepath.WalkDir(assets, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type()&fs.ModeSymlink != 0 {
			target, err := filepath.EvalSymlinks(p)
			if rel, rerr := filepath.Rel(real, target); err != nil || rerr != nil || strings.HasPrefix(rel, "..") {
				t.Errorf("%s points outside assets", p)
			}
		}
		return nil
	})
}

func TestPolkitRule(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	rules := read(t, filepath.Join(rootfsDir, "etc/polkit-1/rules.d/50-nabos.rules"))
	decide := func(user, action string, details map[string]string) string {
		js := `const polkit = { Result: { YES: "yes", NOT_HANDLED: "not_handled" }, rules: [],
                  addRule(f) { this.rules.push(f); } };
` + rules + fmt.Sprintf(`
const details = %s;
const action = { id: %s, lookup: (k) => details[k] };
console.log(polkit.rules[0](action, { user: %s }));
`, must(json.Marshal(details)), must(json.Marshal(action)), must(json.Marshal(user)))
		return strings.TrimSpace(run(t, "", "node", "-e", js))
	}
	units := func(unit, verb string) map[string]string { return map[string]string{"unit": unit, "verb": verb} }
	manage := "org.freedesktop.systemd1.manage-units"
	for _, c := range []struct {
		user, action string
		details      map[string]string
		want         string
	}{
		{"nabos", "org.freedesktop.login1.reboot", nil, "yes"},
		{"nabos", "org.freedesktop.login1.power-off-multiple-sessions", nil, "yes"},
		{"nabos", manage, units("linux-voice-assistant.service", "start"), "yes"},
		{"nabos", manage, units("ssh.service", "start"), "yes"},
		{"nabos", manage, units("ssh.service", "stop"), "yes"},
		{"nabos", manage, units("ssh.service", "restart"), "not_handled"},
		{"nabos", manage, units("ssh.service", "enable"), "not_handled"},
		{"nobody", manage, units("ssh.service", "start"), "not_handled"},
		{"nabos", manage, units("linux-voice-assistant.service", "enable"), "not_handled"},
		{"nabos", "org.freedesktop.systemd1.manage-unit-files", nil, "not_handled"},
		{"nabos", "org.freedesktop.timedate1.set-time", nil, "yes"},
		{"nabos", "org.freedesktop.timedate1.set-ntp", nil, "not_handled"},
		{"nabos", manage, units("systemd-timesyncd.service", "start"), "yes"},
		{"nabos", manage, units("systemd-timesyncd.service", "stop"), "yes"},
		{"nabos", manage, units("systemd-timesyncd.service", "restart"), "not_handled"},
		{"nobody", "org.freedesktop.login1.reboot", nil, "not_handled"},
	} {
		if got := decide(c.user, c.action, c.details); got != c.want {
			t.Errorf("%s %s %v: %s, want %s", c.user, c.action, c.details, got, c.want)
		}
	}
}

func TestRaucInstallerOnlyForNabos(t *testing.T) {
	type rule struct {
		XMLName xml.Name
		Attrs   []xml.Attr `xml:",any,attr"`
	}
	var conf struct {
		Policies []struct {
			Context string `xml:"context,attr"`
			User    string `xml:"user,attr"`
			Rules   []rule `xml:",any"`
		} `xml:"policy"`
	}
	if err := xml.Unmarshal([]byte(read(t, filepath.Join(rootfsDir, "etc/dbus-1/system.d/nabos-rauc.conf"))), &conf); err != nil {
		t.Fatal(err)
	}
	policies := map[string][]rule{}
	for _, p := range conf.Policies {
		policies[p.Context+p.User] = p.Rules
	}
	attrs := func(r rule) map[string]string {
		m := map[string]string{}
		for _, a := range r.Attrs {
			m[a.Name.Local] = a.Value
		}
		return m
	}
	if policies["default"][0].XMLName.Local != "deny" {
		t.Error("default policy must deny")
	}
	if a := attrs(policies["root"][0]); !reflect.DeepEqual(a, map[string]string{"send_destination": "de.pengutronix.rauc"}) {
		t.Errorf("root policy %v", a)
	}
	install := false
	for _, r := range policies["nabos"] {
		a := attrs(r)
		if r.XMLName.Local != "allow" || a["send_destination"] != "de.pengutronix.rauc" {
			t.Errorf("nabos rule %s %v", r.XMLName.Local, a)
		}
		if a["send_interface"] == "de.pengutronix.rauc.Installer" {
			if a["send_member"] != "InstallBundle" {
				t.Errorf("nabos may call Installer.%s", a["send_member"])
			}
			install = true
		}
	}
	if !install {
		t.Error("nabos cannot install bundles")
	}
}

func TestUnitsParse(t *testing.T) {
	if _, err := exec.LookPath("systemd-analyze"); err != nil {
		t.Skip("systemd-analyze not available")
	}
	units, _ := filepath.Glob(filepath.Join(rootfsDir, "usr/lib/systemd/system/*.service"))
	r := execute(t, "", nil, "systemd-analyze", append([]string{"verify", "--man=no", "--generators=no"}, units...)...)
	noise := []string{"is not executable", "No such file or directory", "Failed to prepare filename",
		"Unit is bound to inactive", "not found", "Failed to resolve", "Operation not permitted"}
	for _, line := range strings.Split(r.stdout+r.stderr, "\n") {
		if strings.TrimSpace(line) != "" && !slices.ContainsFunc(noise, func(n string) bool { return strings.Contains(line, n) }) {
			t.Error(line)
		}
	}
}
