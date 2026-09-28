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
		"/var/lib/comitup", "/var/lib/systemd/timesync", "/var/lib/tagtagtag-sound", "/var/lib/nabos"}
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
	for _, p := range []struct {
		size int
		kind string
	}{{256 * MiB, "c"}, {6144 * MiB, "83"}, {6144 * MiB, "83"}, {1024 * MiB, "83"}} {
		table += fmt.Sprintf("start=%d,size=%d,type=%s\n", start/sector, p.size/sector, p.kind)
		start += p.size
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
	slot                string
	healthyAfter        int
	own, other, otherFS string
	sinks, sources      string
	mounts              map[string]string
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

func checkHealth(t *testing.T, c healthCase) (string, []string) {
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
	counter := filepath.Join(tmp, "curl-count")
	write(t, counter, "0")
	write(t, filepath.Join(tmp, "sinks"), c.sinks)
	write(t, filepath.Join(tmp, "sources"), c.sources)
	cases := ""
	for k, v := range c.mounts {
		cases += k + ") echo \"" + v + "\";; "
	}
	fake := newFakes(t, tmp, map[string]string{
		"systemctl": "exit 0",
		"curl":      fmt.Sprintf("n=$(($(cat %s) + 1)); echo $n > %s; [ $n -gt %d ]", counter, counter, c.healthyAfter),
		// runuser -u nabos -- env ... pactl list KIND
		"runuser": `[ "$1 $2 $3" = "-u nabos --" ] || exit 1; shift 3; exec "$@"`,
		"pactl":   `[ "$XDG_RUNTIME_DIR $LC_ALL $1" = "/run/user/1000 C list" ] && cat ` + tmp + "/$2",
		// findmnt -n -o COLUMNS --mountpoint PATH
		"findmnt":     `eval "p=\$$#"; case $p in ` + cases + "*) exit 1;; esac",
		"rauc":        "exit 0",
		"blkid":       "echo " + c.otherFS,
		"fw_printenv": fmt.Sprintf("case $2 in BOOT_%s_LEFT) echo %s;; *) echo %s;; esac", c.slot, c.own, c.other),
	})
	cmdline := filepath.Join(tmp, "cmdline")
	write(t, cmdline, "root=/dev/mmcblk0p2 ro rauc.slot="+c.slot+" quiet\n")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", health)
	cmd.Env = append(os.Environ(), fake.env("NABOS_CMDLINE="+cmdline, "NABOS_HEALTH_TIMEOUT=2",
		"NABOS_HEALTH_INTERVAL=1", "NABOS_BOOT_INIT="+bootInit)...)
	out, _ := cmd.Output()
	if ctx.Err() != nil {
		t.Fatal("health check timed out")
	}
	return string(out), fake.calls(t)
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
		_, calls := checkHealth(t, healthCase{})
		confirmed(t, calls, true)
		rebooted(t, calls, false)
	})
	check("persist list matches boot-init", func(t *testing.T) {
		m := regexp.MustCompile(`(?m)^PERSIST="([^"]+)"`).FindStringSubmatch(read(t, bootInit))
		if m == nil || !slices.Equal(strings.Fields(m[1]), persist) {
			t.Errorf("boot-init PERSIST %q", m)
		}
	})
	check("volatile data is never confirmed", func(t *testing.T) {
		_, calls := checkHealth(t, healthCase{mounts: mounts(map[string]string{"/data": "tmpfs tmpfs"})})
		confirmed(t, calls, false)
	})
	for _, path := range []string{"/etc/machine-id", "/var/lib/nabos", "/etc/NetworkManager/system-connections"} {
		check("missing bind "+path+" is never confirmed", func(t *testing.T) {
			_, calls := checkHealth(t, healthCase{mounts: mounts(map[string]string{path: ""})})
			confirmed(t, calls, false)
		})
	}
	check("bind from elsewhere is never confirmed", func(t *testing.T) {
		_, calls := checkHealth(t, healthCase{mounts: mounts(map[string]string{"/var/lib/comitup": "tmpfs"})})
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
		_, calls := checkHealth(t, healthCase{sinks: nullSink, sources: nullMonitor})
		confirmed(t, calls, false)
		rebooted(t, calls, true)
	})
	check("monitor is not a microphone", func(t *testing.T) {
		_, calls := checkHealth(t, healthCase{sources: nullMonitor + wm8960Monitor})
		confirmed(t, calls, false)
	})
	check("WM8960 next to null sink is enough", func(t *testing.T) {
		_, calls := checkHealth(t, healthCase{sinks: nullSink + wm8960Sink, sources: wm8960Capture + nullMonitor})
		confirmed(t, calls, true)
	})
	check("unhealthy slot with attempts reboots", func(t *testing.T) {
		_, calls := checkHealth(t, healthCase{healthyAfter: 100})
		rebooted(t, calls, true)
		confirmed(t, calls, false)
	})
	check("exhausted slot falls back to installed other slot", func(t *testing.T) {
		_, calls := checkHealth(t, healthCase{healthyAfter: 100, own: "0", other: "3"})
		rebooted(t, calls, true)
	})
	check("no reboot loop when other slot is empty", func(t *testing.T) {
		// Initial image: slot B has no filesystem, slot A is out of attempts.
		// Stay up, keep checking, confirm once healthy.
		out, calls := checkHealth(t, healthCase{healthyAfter: 5, own: "0", other: "3", otherFS: `""`})
		rebooted(t, calls, false)
		confirmed(t, calls, true)
		if !strings.Contains(out, "staying up unconfirmed") {
			t.Errorf("output %q", out)
		}
	})
	check("all attempts exhausted stays up", func(t *testing.T) {
		_, calls := checkHealth(t, healthCase{slot: "B", healthyAfter: 4, own: "0", other: "0"})
		rebooted(t, calls, false)
		confirmed(t, calls, true)
	})
	check("not booted from a slot does nothing", func(t *testing.T) {
		tmp := t.TempDir()
		fake := newFakes(t, tmp, map[string]string{"rauc": "exit 0", "systemctl": "exit 0"})
		cmdline := filepath.Join(tmp, "cmdline")
		write(t, cmdline, "root=/dev/mmcblk0p2\n")
		if r := execute(t, "", fake.env("NABOS_CMDLINE="+cmdline, "NABOS_BOOT_INIT="+bootInit), "sh", health); r.code != 0 {
			t.Fatalf("exit %d: %s", r.code, r.stderr)
		}
		if calls := fake.calls(t); len(calls) != 0 {
			t.Errorf("calls %q", calls)
		}
	})
}

func TestRfidProbe(t *testing.T) {
	for name, c := range map[string]struct {
		reg7f, reg00 string
		want         []string
	}{
		"ST25R391x 2022 NFC card": {"0x2a", "0x00", []string{"dtoverlay -d /boot/overlays st25r391x"}},
		"CR14 TagTagTag":          {"0x13", "0x00", []string{"dtoverlay -d /boot/overlays cr14"}},
		"no reader":               {"", "", nil},
	} {
		fake := newFakes(t, t.TempDir(), map[string]string{
			"modprobe":  "exit 0",
			"dtoverlay": "exit 0",
			// i2cget -y 1 0x50 REG b; empty value = no answer.
			"i2cget": fmt.Sprintf(`case $4 in 0x7f) v="%s";; *) v="%s";; esac; [ -n "$v" ] && echo $v`, c.reg7f, c.reg00),
		})
		if r := execute(t, "", fake.env(), "sh", filepath.Join(rootfsDir, "usr/lib/nabos/rfid-probe")); r.code != 0 {
			t.Fatalf("%s: exit %d: %s", name, r.code, r.stderr)
		}
		var overlays []string
		for _, call := range fake.calls(t) {
			if strings.HasPrefix(call, "dtoverlay") {
				overlays = append(overlays, call)
			}
		}
		if !slices.Equal(overlays, c.want) {
			t.Errorf("%s: %q", name, overlays)
		}
	}
}
