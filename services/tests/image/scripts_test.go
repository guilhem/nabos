package image

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestKernelSupportsZstdBundles(t *testing.T) {
	prepare := read(t, filepath.Join(imageDir, "prepare.sh"))
	_, check, ok := strings.Cut(prepare, "    for option in ")
	if !ok {
		t.Fatal("kernel option checks missing")
	}
	check, _, ok = strings.Cut(check, "\n    done")
	if !ok {
		t.Fatal("kernel option loop missing")
	}
	check = strings.ReplaceAll("for option in "+check+"\ndone", "/lib/modules/$kernel/build/.config", "$NABOS_TEST_CONFIG")
	for _, c := range []struct {
		name, config string
		valid        bool
	}{
		{"built-in", "CONFIG_SQUASHFS=y\nCONFIG_SQUASHFS_ZSTD=y\n", true},
		{"module", "CONFIG_SQUASHFS=m\nCONFIG_SQUASHFS_ZSTD=y\n", true},
		{"no-squashfs", "# CONFIG_SQUASHFS is not set\nCONFIG_SQUASHFS_ZSTD=y\n", false},
		{"no-zstd", "CONFIG_SQUASHFS=m\n# CONFIG_SQUASHFS_ZSTD is not set\n", false},
		{"missing-zstd", "CONFIG_SQUASHFS=m\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			config := filepath.Join(t.TempDir(), "kernel.config")
			write(t, config, "CONFIG_BCM2835_WDT=y\nCONFIG_WATCHDOG_HANDLE_BOOT_ENABLED=y\nCONFIG_LEDS_CLASS_MULTICOLOR=m\nCONFIG_DMA_BCM2835=y\n"+c.config)
			r := execute(t, "", []string{"NABOS_TEST_CONFIG=" + config}, "bash", "-eu", "-c", check)
			if (r.code == 0) != c.valid {
				t.Fatalf("kernel support: exit %d: %s", r.code, r.stderr)
			}
		})
	}
}

func TestReleaseUpload(t *testing.T) {
	workflow := read(t, filepath.Join(repo, ".github/workflows/images.yml"))
	// Exercise the final upload step with real checksums and a fake GitHub CLI.
	const marker = "        run: |\n"
	start := strings.LastIndex(workflow, marker)
	if start < 0 {
		t.Fatal("release upload step missing")
	}
	script := workflow[start+len(marker):]
	for _, scenario := range []struct {
		name, fail, want string
		existing         bool
	}{
		{"first", "", "view upload upload", false},
		{"rerun", "", "view delete-asset upload upload", true},
		{"corrupt", "", "", true},
		{"lookup-failure", "view", "view", true},
		{"delete-failure", "delete-asset", "view delete-asset", true},
		{"upload-failure", "upload", "view delete-asset upload", true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Chdir(tmp)
			if err := os.Mkdir("dist", 0o755); err != nil {
				t.Fatal(err)
			}
			var sums string
			for _, target := range []string{"zero-armv6", "zero2-arm64"} {
				asset := "nabos-" + target + ".raucb"
				write(t, filepath.Join("dist", asset), target)
				sum := fmt.Sprintf("%x  ./%s\n", sha256.Sum256([]byte(target)), asset)
				write(t, filepath.Join("dist", "SHA256SUMS-"+target), sum)
				sums += sum
			}
			fake := newFakes(t, tmp, map[string]string{"gh": `
[ "$1 $3" = "release v0.1.0" ] || exit 1
[ "$2" != "$FAIL_COMMAND" ] || exit 1
case "$2" in
  view) if [ "$EXISTING_MANIFEST" = true ]; then echo SHA256SUMS; fi ;;
  delete-asset) [ "$4 $5 $6 $7" = "SHA256SUMS --repo guilhem/nabos --yes" ] ;;
  upload) [ "$4 $5 $6" = "--repo guilhem/nabos --clobber" ] ;;
  *) exit 1 ;;
esac`})
			env := fake.env("GITHUB_REF_NAME=v0.1.0", "GITHUB_REPOSITORY=guilhem/nabos",
				fmt.Sprintf("EXISTING_MANIFEST=%t", scenario.existing), "FAIL_COMMAND="+scenario.fail)
			if scenario.name == "corrupt" {
				write(t, "dist/nabos-zero2-arm64.raucb", "corrupted")
			}
			r := execute(t, "", env, "bash", "-e", "-o", "pipefail", "-c", script)
			calls := fake.calls(t)
			var commands []string
			for _, call := range calls {
				commands = append(commands, strings.Fields(call)[2])
			}
			if strings.Join(commands, " ") != scenario.want {
				t.Fatalf("unexpected release operations: %v\n%s", calls, r.stderr)
			}
			if scenario.name == "first" || scenario.name == "rerun" {
				if r.code != 0 ||
					strings.Contains(calls[len(calls)-2], " ./SHA256SUMS ") ||
					!strings.HasSuffix(calls[len(calls)-1], "--clobber SHA256SUMS") ||
					read(t, "dist/SHA256SUMS") != sums {
					t.Fatalf("upload failed or manifest published too early: %v\n%s", calls, r.stderr)
				}
			} else if r.code == 0 {
				t.Fatalf("failed artifacts were published: %v\n%s", calls, r.stderr)
			}
		})
	}
}

var (
	bootInit = filepath.Join(rootfsDir, "usr/lib/nabos/boot-init")
	health   = filepath.Join(rootfsDir, "usr/lib/nabos/health")
	persist  = []string{"/etc/NetworkManager/system-connections", "/var/lib/NetworkManager",
		"/var/lib/systemd/timesync", "/var/lib/tagtagtag-sound", "/var/lib/nabos"}
)

func TestBootInitRefusesToRunOutsidePID1(t *testing.T) {
	r := execute(t, "", nil, "sh", bootInit)
	if r.code != 1 || !strings.Contains(r.stderr, "must run as PID 1") {
		t.Errorf("exit %d: %s", r.code, r.stderr)
	}
}

func TestBootInitGrowsOnlyTheDataPartitionOnA16GBCard(t *testing.T) {
	tmp := t.TempDir()
	card := filepath.Join(tmp, "mmcblk0")
	write(t, card, "")
	if err := os.Truncate(card, 15_931_539_456); err != nil { // a real 16 GB card, sparse
		t.Fatal(err)
	}
	const sector = 512
	table := "label: dos\n"
	start := 4 * MiB
	for i, p := range []struct {
		size int
		kind string
	}{{256 * MiB, "c"}, {6144 * MiB, "83"}, {6144 * MiB, "83"}, {1024 * MiB, "83"}} {
		table += fmt.Sprintf("start=%d,size=%d,type=%s\n", start/sector, p.size/sector, p.kind)
		start += p.size
		if i == 0 {
			start += 256 * MiB // second boot copy is reserved outside the MBR table
		}
	}
	run(t, table, "sfdisk", "-q", card)
	type part = map[string]any
	partitions := func() []part {
		var out struct {
			Partitiontable struct{ Partitions []part }
		}
		dec := json.NewDecoder(strings.NewReader(run(t, "", "sfdisk", "--json", card)))
		dec.UseNumber()
		if err := dec.Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out.Partitiontable.Partitions
	}
	sysfs := func() []part {
		parts := partitions()
		block := filepath.Join(tmp, "sys/class/block")
		os.MkdirAll(filepath.Join(block, "mmcblk0p4"), 0o755)
		os.MkdirAll(filepath.Join(block, "mmcblk0"), 0o755)
		fi, _ := os.Stat(card)
		write(t, filepath.Join(block, "mmcblk0/size"), fmt.Sprintf("%d\n", fi.Size()/sector))
		write(t, filepath.Join(block, "mmcblk0p4/start"), fmt.Sprintf("%v\n", parts[3]["start"]))
		write(t, filepath.Join(block, "mmcblk0p4/size"), fmt.Sprintf("%v\n", parts[3]["size"]))
		return parts
	}
	before := partitions()
	kmsg := filepath.Join(tmp, "kmsg")
	env := []string{"NABOS_BOOT_INIT_LIB=1", "NABOS_DISK=" + card, "NABOS_SYSFS=" + filepath.Join(tmp, "sys"), "NABOS_KMSG=" + kmsg}
	grow := func() {
		if r := execute(t, "", env, "sh", "-c", ". "+bootInit+"; grow_partition"); r.code != 0 {
			t.Fatalf("grow_partition: exit %d: %s", r.code, r.stderr)
		}
	}
	sysfs()
	grow()
	if !strings.Contains(read(t, kmsg), "extending the data partition") {
		t.Error("growth not logged")
	}
	after := sysfs()
	if !reflect.DeepEqual(before[:3], after[:3]) { // boot, A and B untouched
		t.Errorf("other partitions changed: %v", after[:3])
	}
	num := func(p part, k string) int64 { return must(p[k].(json.Number).Int64()) }
	if num(after[3], "start") != num(before[3], "start") || num(after[3], "size") <= num(before[3], "size") {
		t.Errorf("data partition %v -> %v", before[3], after[3])
	}
	fi, _ := os.Stat(card)
	if fi.Size()/sector-(num(after[3], "start")+num(after[3], "size")) > 2048 {
		t.Error("data partition does not reach the end of the card")
	}
	os.Remove(kmsg)
	grow()
	if _, err := os.Stat(kmsg); !os.IsNotExist(err) {
		t.Error("second run changed the card again")
	}
	if !reflect.DeepEqual(sysfs(), after) {
		t.Error("second run changed the partition table")
	}
}

// pactl list output shaped like pipewire-pulse (LC_ALL=C), trimmed to the fields used.
const (
	wm8960Sink = "Sink #52\n\tState: SUSPENDED\n\tName: alsa_output.platform-soc_sound.stereo-fallback\n\tDriver: PipeWire\n" +
		"\tProperties:\n\t\talsa.card_name = \"tagtagtag-sound\"\n\t\tdevice.api = \"alsa\"\n\t\tmedia.class = \"Audio/Sink\"\n"
	nullSink = "Sink #33\n\tState: SUSPENDED\n\tName: auto_null\n\tDriver: PipeWire\n" +
		"\tProperties:\n\t\tnode.name = \"auto_null\"\n\t\tmedia.class = \"Audio/Sink\"\n"
	wm8960Monitor = "Source #53\n\tState: SUSPENDED\n\tName: alsa_output.platform-soc_sound.stereo-fallback.monitor\n" +
		"\tMonitor of Sink: alsa_output.platform-soc_sound.stereo-fallback\n" +
		"\tProperties:\n\t\talsa.card_name = \"tagtagtag-sound\"\n\t\tdevice.class = \"monitor\"\n"
	wm8960Capture = "Source #54\n\tState: SUSPENDED\n\tName: alsa_input.platform-soc_sound.stereo-fallback\n" +
		"\tMonitor of Sink: n/a\n" +
		"\tProperties:\n\t\talsa.card_name = \"tagtagtag-sound\"\n\t\tmedia.class = \"Audio/Source\"\n"
	nullMonitor = "Source #34\n\tState: SUSPENDED\n\tName: auto_null.monitor\n\tMonitor of Sink: auto_null\n" +
		"\tProperties:\n\t\tdevice.class = \"monitor\"\n"
)

// healthCase leaves fields empty for a healthy slot A with attempts left.
type healthCase struct {
	deviceUnready       bool
	deviceInactive      bool
	hardwareUnready     bool
	hardwareInactive    bool
	appInactive         bool
	slot                string
	healthyAfter        int
	own, other, otherFS string
	sinks, sources      string
	mounts              map[string]string
	raucFails           bool // mark-good fails
	stuck               bool // never healthy: stop the check after a few seconds
}

// mounts returns findmnt answers for a correct boot-init run; "" removes an entry.
func mounts(changes map[string]string) map[string]string {
	table := map[string]string{"/data": "/dev/mmcblk0p4 ext4", "/etc/machine-id": "/dev/mmcblk0p4[/system/machine-id]"}
	for _, p := range persist {
		table[p] = "/dev/mmcblk0p4[/system" + p + "]"
	}
	for k, v := range changes {
		if v == "" {
			delete(table, k)
		} else {
			table[k] = v
		}
	}
	return table
}

// checkHealth runs the health check and returns its output, the commands it
// ran and the boot health marker left for nabos.
func checkHealth(t *testing.T, c healthCase) (string, []string, string) {
	t.Helper()
	def := func(v *string, d string) {
		if *v == "" {
			*v = d
		}
	}
	def(&c.slot, "A")
	def(&c.own, "2")
	def(&c.other, "3")
	def(&c.otherFS, "ext4")
	def(&c.sinks, wm8960Sink)
	def(&c.sources, wm8960Monitor+wm8960Capture)
	if c.mounts == nil {
		c.mounts = mounts(nil)
	}
	tmp := t.TempDir()
	marker := filepath.Join(tmp, "nabos-boot-health")
	write(t, marker, "good "+c.slot+"\n") // stale verdict of an earlier run
	counter := filepath.Join(tmp, "curl-count")
	write(t, counter, "0")
	write(t, filepath.Join(tmp, "sinks"), c.sinks)
	write(t, filepath.Join(tmp, "sources"), c.sources)
	cases := ""
	for k, v := range c.mounts {
		cases += k + ") echo \"" + v + "\";; "
	}
	fake := newFakes(t, tmp, map[string]string{
		"systemctl": fmt.Sprintf(`case "$*" in
"is-active --quiet device-core.service") [ %t = false ] ;;
"is-active --quiet nab-hardware.service") [ %t = false ] ;;
"is-active --quiet nabos.service") [ %t = false ] ;;
"reboot") exit 0 ;;
*) exit 1 ;;
esac`, c.deviceInactive, c.hardwareInactive, c.appInactive),
		"busctl": fmt.Sprintf(`case "$*" in
"--system --timeout=5 get-property io.github.guilhem.DeviceCore1 /io/github/guilhem/DeviceCore1 io.github.guilhem.DeviceCore1.Manager Ready") echo 'b %t' ;;
"--system --timeout=5 get-property io.github.guilhem.NabHardware1 /io/github/guilhem/NabHardware1 io.github.guilhem.NabHardware1 Ready") echo 'b %t' ;;
*) exit 1 ;;
esac`, !c.deviceUnready, !c.hardwareUnready),
		"curl": fmt.Sprintf("n=$(($(cat %s) + 1)); echo $n > %s; [ $n -gt %d ]", counter, counter, c.healthyAfter),
		// runuser -u nab-audio -- env ... pactl list KIND
		"runuser": `[ "$1 $2 $3" = "-u nab-audio --" ] || exit 1; shift 3; exec "$@"`,
		"pactl":   `[ "$XDG_RUNTIME_DIR $LC_ALL $1" = "/run/user/1004 C list" ] && cat ` + tmp + "/$2",
		// findmnt -n -o COLUMNS --mountpoint PATH
		"findmnt":     `eval "p=\$$#"; case $p in ` + cases + "*) exit 1;; esac",
		"rauc":        fmt.Sprintf("exit %d", map[bool]int{false: 0, true: 1}[c.raucFails]),
		"blkid":       "echo " + c.otherFS,
		"fw_printenv": fmt.Sprintf("case $2 in BOOT_%s_LEFT) echo %s;; *) echo %s;; esac", c.slot, c.own, c.other),
	})
	cmdline := filepath.Join(tmp, "cmdline")
	write(t, cmdline, "root=/dev/mmcblk0p2 ro rauc.slot="+c.slot+" quiet\n")
	limit := 30 * time.Second
	if c.stuck {
		limit = 6 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", health)
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = append(os.Environ(), fake.env("NABOS_CMDLINE="+cmdline, "NABOS_HEALTH_TIMEOUT=2",
		"NABOS_HEALTH_INTERVAL=1", "NABOS_BOOT_INIT="+bootInit, "NABOS_BOOT_HEALTH="+marker)...)
	out, _ := cmd.Output()
	if ctx.Err() != nil && !c.stuck {
		t.Fatal("health check timed out")
	}
	verdict := ""
	if fi, err := os.Stat(marker); err == nil {
		if fi.Mode().Perm() != 0o644 {
			t.Errorf("marker mode %v", fi.Mode())
		}
		verdict = read(t, marker)
	}
	return string(out), fake.calls(t), verdict
}

func TestBoardLEDOffAfterHealthyBoot(t *testing.T) {
	units := filepath.Join(rootfsDir, "usr/lib/systemd/system")
	healthUnit := strings.Split(read(t, filepath.Join(units, "nabos-health.service")), "\n")
	for _, line := range []string{"Type=exec", "OnSuccess=nabos-board-led-off.service"} {
		if !slices.Contains(healthUnit, line) {
			t.Errorf("health unit missing %q", line)
		}
	}
	unit := read(t, filepath.Join(units, "nabos-board-led-off.service"))
	const brightness = "/sys/class/leds/ACT/brightness"
	const marker = "/run/nabos-boot-health"
	for _, line := range []string{"Type=oneshot", "ConditionPathExists=" + brightness,
		"ConditionPathExists=" + marker, "NoNewPrivileges=yes", "ProtectSystem=strict", "CapabilityBoundingSet="} {
		if !slices.Contains(strings.Split(unit, "\n"), line) {
			t.Errorf("LED unit missing %q", line)
		}
	}
	command := func(key string) string {
		m := regexp.MustCompile(`(?m)^` + key + `=(.+)$`).FindStringSubmatch(unit)
		if m == nil {
			t.Fatalf("LED unit missing %s", key)
		}
		return m[1]
	}
	condition, action := command("ExecCondition"), command("ExecStart")
	// Execute the unit's commands with only paths replaced; systemd guards and
	// sandboxing are inspected above, not exercised by this host test.
	for _, c := range []struct {
		name, verdict string
		healthy       bool
	}{
		{"healthy-A", "good A\n", true}, {"healthy-B", "good B\n", true},
		{"pending", "pending A\n", false}, {"stranded", "stranded B\n", false},
		{"invalid-slot", "good C\n", false}, {"missing-marker", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			tmp := t.TempDir()
			led, verdict := filepath.Join(tmp, "brightness"), filepath.Join(tmp, "health")
			write(t, led, "1\n")
			if c.verdict != "" {
				write(t, verdict, c.verdict)
			}
			replace := strings.NewReplacer(brightness, led, marker, verdict)
			r := execute(t, "", nil, "/bin/sh", "-c", replace.Replace(condition))
			if (r.code == 0) != c.healthy {
				t.Fatalf("condition exit %d: %s", r.code, r.stderr)
			}
			want := "1\n"
			if r.code == 0 {
				run(t, "", "/bin/sh", "-c", replace.Replace(action))
				want = "0\n"
			}
			if got := read(t, led); got != want {
				t.Errorf("brightness %q, want %q", got, want)
			}
		})
	}
}

func TestHealth(t *testing.T) {
	t.Parallel()
	markGood, reboot := "rauc status mark-good", "systemctl reboot"
	confirmed := func(t *testing.T, calls []string, want bool) {
		t.Helper()
		if slices.Contains(calls, markGood) != want {
			t.Errorf("mark-good %v, want %v: %q", !want, want, calls)
		}
	}
	rebooted := func(t *testing.T, calls []string, want bool) {
		t.Helper()
		if slices.Contains(calls, reboot) != want {
			t.Errorf("reboot %v, want %v: %q", !want, want, calls)
		}
	}
	// Every scenario waits for the health timeout; run them side by side.
	check := func(name string, fn func(t *testing.T)) {
		t.Run(name, func(t *testing.T) { t.Parallel(); fn(t) })
	}

	check("healthy slot is marked good", func(t *testing.T) {
		_, calls, marker := checkHealth(t, healthCase{})
		confirmed(t, calls, true)
		rebooted(t, calls, false)
		if marker != "good A\n" {
			t.Errorf("marker %q", marker)
		}
	})
	for _, c := range []healthCase{{deviceUnready: true}, {deviceInactive: true}} {
		check(fmt.Sprintf("device-core inactive=%t unready=%t", c.deviceInactive, c.deviceUnready), func(t *testing.T) {
			_, calls, _ := checkHealth(t, c)
			confirmed(t, calls, false)
			rebooted(t, calls, true)
		})
	}
	for _, c := range []healthCase{{hardwareUnready: true}, {hardwareInactive: true}, {appInactive: true}} {
		check(fmt.Sprintf("hardware inactive=%t unready=%t app inactive=%t", c.hardwareInactive, c.hardwareUnready, c.appInactive), func(t *testing.T) {
			_, calls, _ := checkHealth(t, c)
			confirmed(t, calls, false)
			rebooted(t, calls, true)
		})
	}
	check("failed mark-good leaves no verdict", func(t *testing.T) {
		_, calls, marker := checkHealth(t, healthCase{raucFails: true})
		confirmed(t, calls, true)
		if marker != "" {
			t.Errorf("marker %q", marker)
		}
	})
	check("persist list matches boot-init", func(t *testing.T) {
		m := regexp.MustCompile(`(?m)^PERSIST="([^"]+)"`).FindStringSubmatch(read(t, bootInit))
		if m == nil || !slices.Equal(strings.Fields(m[1]), persist) {
			t.Errorf("boot-init PERSIST %q", m)
		}
	})
	check("volatile data is never confirmed", func(t *testing.T) {
		_, calls, marker := checkHealth(t, healthCase{mounts: mounts(map[string]string{"/data": "tmpfs tmpfs"})})
		confirmed(t, calls, false)
		if marker != "" {
			t.Errorf("stale marker %q", marker)
		}
	})
	for _, path := range []string{"/etc/machine-id", "/var/lib/nabos", "/etc/NetworkManager/system-connections"} {
		check("missing bind "+path+" is never confirmed", func(t *testing.T) {
			_, calls, _ := checkHealth(t, healthCase{mounts: mounts(map[string]string{path: ""})})
			confirmed(t, calls, false)
		})
	}
	check("bind from elsewhere is never confirmed", func(t *testing.T) {
		_, calls, _ := checkHealth(t, healthCase{mounts: mounts(map[string]string{"/var/lib/NetworkManager": "tmpfs"})})
		confirmed(t, calls, false)
	})
	check("card name matches the pinned sound overlay", func(t *testing.T) {
		dts := find(filepath.Join(sources, "sound"), "tagtagtag-sound-overlay.dts")
		if len(dts) == 0 {
			t.Skip("pinned sound driver sources not available")
		}
		m := regexp.MustCompile(`simple-audio-card,name = "([^"]+)"`).FindStringSubmatch(read(t, dts[0]))
		if m == nil || !strings.Contains(read(t, health), `alsa\.card_name = "`+m[1]+`"`) {
			t.Errorf("health does not look for card %q", m)
		}
	})
	check("null sink is not audio hardware", func(t *testing.T) {
		_, calls, _ := checkHealth(t, healthCase{sinks: nullSink, sources: nullMonitor})
		confirmed(t, calls, false)
		rebooted(t, calls, true)
	})
	check("monitor is not a microphone", func(t *testing.T) {
		_, calls, _ := checkHealth(t, healthCase{sources: nullMonitor + wm8960Monitor})
		confirmed(t, calls, false)
	})
	check("WM8960 next to null sink is enough", func(t *testing.T) {
		_, calls, _ := checkHealth(t, healthCase{sinks: nullSink + wm8960Sink, sources: wm8960Capture + nullMonitor})
		confirmed(t, calls, true)
	})
	check("unhealthy slot with attempts reboots", func(t *testing.T) {
		_, calls, _ := checkHealth(t, healthCase{healthyAfter: 100})
		rebooted(t, calls, true)
		confirmed(t, calls, false)
	})
	check("exhausted slot falls back to installed other slot", func(t *testing.T) {
		_, calls, _ := checkHealth(t, healthCase{healthyAfter: 100, own: "0", other: "3"})
		rebooted(t, calls, true)
	})
	check("no reboot loop when other slot is empty", func(t *testing.T) {
		// Initial image: slot B has no filesystem, slot A is out of attempts.
		// Stay up, keep checking, confirm once healthy.
		out, calls, _ := checkHealth(t, healthCase{healthyAfter: 5, own: "0", other: "3", otherFS: `""`})
		rebooted(t, calls, false)
		confirmed(t, calls, true)
		if !strings.Contains(out, "staying up unconfirmed") {
			t.Errorf("output %q", out)
		}
	})
	check("all attempts exhausted stays up", func(t *testing.T) {
		_, calls, marker := checkHealth(t, healthCase{slot: "B", healthyAfter: 4, own: "0", other: "0"})
		rebooted(t, calls, false)
		confirmed(t, calls, true)
		if marker != "good B\n" {
			t.Errorf("marker %q", marker)
		}
	})
	check("stranded slot is reported for manual repair", func(t *testing.T) {
		_, calls, marker := checkHealth(t, healthCase{healthyAfter: 100, own: "0", other: "0", stuck: true})
		rebooted(t, calls, false)
		confirmed(t, calls, false)
		if marker != "stranded A\n" {
			t.Errorf("marker %q", marker)
		}
	})
	check("not booted from a slot does nothing", func(t *testing.T) {
		tmp := t.TempDir()
		fake := newFakes(t, tmp, map[string]string{"rauc": "exit 0", "systemctl": "exit 0"})
		cmdline := filepath.Join(tmp, "cmdline")
		write(t, cmdline, "root=/dev/mmcblk0p2\n")
		marker := filepath.Join(tmp, "nabos-boot-health")
		write(t, marker, "good A\n")
		if r := execute(t, "", fake.env("NABOS_CMDLINE="+cmdline, "NABOS_BOOT_INIT="+bootInit, "NABOS_BOOT_HEALTH="+marker), "sh", health); r.code != 0 {
			t.Fatalf("exit %d: %s", r.code, r.stderr)
		}
		if calls := fake.calls(t); len(calls) != 0 {
			t.Errorf("calls %q", calls)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Error("stale marker kept")
		}
	})
}

func TestUserspaceHardwareImageContract(t *testing.T) {
	if got := read(t, filepath.Join(rootfsDir, "etc/modules-load.d/nabos.conf")); got != "i2c-dev\nbcm2835-ws2812\n" {
		t.Errorf("unexpected hardware module configuration: %q", got)
	}
	if !strings.Contains(read(t, filepath.Join(imageDir, "nabos-overlay.dts")), `&i2c1 { status = "okay"; };`) {
		t.Error("the Linux slot DTB must enable I2C bus 1 independently of reader overlays")
	}
	prepare := read(t, filepath.Join(imageDir, "prepare.sh"))
	if !strings.Contains(prepare, "for driver in sound led; do") || strings.Contains(prepare, "libws2811") {
		t.Error("sound and native LED modules must replace the raw-memory LED library")
	}
	if !strings.Contains(prepare, "/boot/overlays/bcm2835-ws2812.dtbo") {
		t.Error("the LED overlay must be merged into the Linux slot DTB")
	}
	var lock struct{ Sources map[string]json.RawMessage }
	if err := json.Unmarshal([]byte(read(t, filepath.Join(imageDir, "sources.lock.json"))), &lock); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ears", "cr14", "nfc", "st25r391x"} {
		if _, exists := lock.Sources[name]; exists {
			t.Errorf("obsolete kernel source pin remains: %s", name)
		}
		for _, file := range []string{"prepare.sh", "boot/boot.cmd", "../services/cmd/nab-image/drivers.go"} {
			if strings.Contains(read(t, filepath.Join(imageDir, file)), name) {
				t.Errorf("obsolete kernel driver remains in %s: %s", file, name)
			}
		}
	}
	udev := read(t, filepath.Join(rootfsDir, "etc/udev/rules.d/60-nabos.rules"))
	for _, obsolete := range []string{`KERNEL=="ear`, `KERNEL=="mem"`, `KERNEL=="vcio"`} {
		if strings.Contains(udev, obsolete) {
			t.Errorf("obsolete raw hardware access rule remains: %s", obsolete)
		}
	}
	for _, file := range []string{
		"patches/ears.patch", "patches/cr14.patch", "patches/nfc.patch",
		"rootfs/usr/lib/nabos/rfid-probe", "rootfs/usr/lib/systemd/system/nabos-rfid.service",
	} {
		if _, err := os.Lstat(filepath.Join(imageDir, file)); !os.IsNotExist(err) {
			t.Errorf("obsolete kernel hardware integration remains: %s", file)
		}
	}
	for _, file := range []string{"usr/lib/nabos/image-setup", "usr/lib/systemd/system/nab-hardware.service"} {
		if strings.Contains(read(t, filepath.Join(rootfsDir, file)), "nabos-rfid") {
			t.Errorf("obsolete reader startup remains: %s", file)
		}
	}
}
