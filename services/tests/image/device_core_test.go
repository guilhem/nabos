package image

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestDeviceCoreImageContract(t *testing.T) {
	unit := read(t, filepath.Join(rootfsDir, "usr/lib/systemd/system/device-core.service"))
	for _, required := range []string{
		"User=device-core", "Environment=HOME=/data/device-core", "Environment=XDG_RUNTIME_DIR=/run/device-core", "Environment=PIPEWIRE_REMOTE=/run/nabos-audio/pipewire-0", "SupplementaryGroups=nab-media nab-audio",
		"Environment=DEVICE_CORE_DATA_DIR=/data/device-core", "Environment=DEVICE_CORE_NETWORK_GUARD=/run/lock/device-core/network",
		"Environment=DEVICE_CORE_PRESENCE_USER=nab-hardware",
		"Environment=DEVICE_CORE_MAINTENANCE_USERS=nab-app:nab-hardware", "RuntimeDirectory=device-core", "WorkingDirectory=/run/device-core",
		"CapabilityBoundingSet=", "ProtectSystem=strict", "ReadWritePaths=/data/device-core /run/device-core /run/lock/device-core",
	} {
		if !strings.Contains(unit, required+"\n") {
			t.Errorf("missing %q", required)
		}
	}
	for _, forbidden := range []string{"CAP_SYS_RAWIO", "AmbientCapabilities", "DEVICE_CORE_HTTP_ADDR", "Environment=DEVICE_CORE_LVA_UNIT=", "RuntimeDirectory=device-core/lock"} {
		if strings.Contains(unit, forbidden) {
			t.Errorf("unexpected %q", forbidden)
		}
	}
	for _, file := range []string{"usr/lib/systemd/system/nab-hardware.service", "usr/lib/systemd/system/nabos.service", "usr/lib/systemd/system/nabos-health.service"} {
		if !strings.Contains(read(t, filepath.Join(rootfsDir, file)), "device-core.service") {
			t.Errorf("missing dependency in %s", file)
		}
	}
	if _, err := os.Stat(filepath.Join(rootfsDir, "etc/dbus-1/system.d/org.nabaztag.Core.conf")); !os.IsNotExist(err) {
		t.Error("obsolete D-Bus policy remains")
	}
	policy := read(t, filepath.Join(rootfsDir, "etc/dbus-1/system.d/io.github.guilhem.DeviceCore1.conf"))
	if !strings.Contains(policy, `<deny own="io.github.guilhem.DeviceCore1"/>`) || !strings.Contains(policy, `<policy user="device-core">`) {
		t.Error("missing default deny/service-account policy")
	}
	config := read(t, filepath.Join(repo, "nix/system.nix"))
	for _, required := range []string{
		`authorizedKeysCommand = "${pkgs.coreutils}/bin/cat /data/device-core/ssh/authorized_keys";`,
		`authorizedKeysCommandUser = "device-core";`, `ConditionFileNotEmpty = "/data/device-core/ssh/authorized_keys";`,
		`ConditionPathExists = "!/data/.volatile";`, `name = mkForce "ssh.service";`,
		`DEVICE_CORE_IMAGE_VERSION=${nabosVersion}`, `DEVICE_CORE_UPDATE_REPO=guilhem/nabos`,
		`DEVICE_CORE_UPDATE_ASSET=nabos-${nabosTarget}.raucb`,
	} {
		if !strings.Contains(config, required) {
			t.Errorf("NixOS configuration lacks %q", required)
		}
	}
	voice := read(t, filepath.Join(rootfsDir, "usr/lib/systemd/system/linux-voice-assistant.service"))
	if !strings.Contains(voice, "ConditionPathExists=/data/device-core/voice-enabled") || !strings.Contains(voice, "--preferences-file /var/lib/nabos/lva/preferences.json") {
		t.Error("voice flag/preferences contract")
	}
}

func TestHardwareApplicationImageContract(t *testing.T) {
	for name, required := range map[string][]string{
		"nab-hardware": {"User=nab-hardware", "Group=nab-hardware", "Type=notify", "NotifyAccess=main", "WatchdogSec=1s", "WatchdogSignal=SIGKILL", "KillMode=control-group", "ExecStopPost=/usr/bin/nab-hardware --stop-hardware", "TimeoutStopSec=6s", "SupplementaryGroups=gpio", "CapabilityBoundingSet=", "RuntimeDirectory=nab-hardware", "WorkingDirectory=/run/nab-hardware", "Environment=HOME=/run/nab-hardware", "Restart=always", "RestartSec=2"},
		"nabos":        {"User=nab-app", "AmbientCapabilities=CAP_NET_BIND_SERVICE", "CapabilityBoundingSet=CAP_NET_BIND_SERVICE", "Environment=NABOS_HTTP_ADDR=:80", "Environment=NABOS_DATA_DIR=/data/nabos", "ReadWritePaths=/data/nabos", "RuntimeDirectory=nabos", "WorkingDirectory=/run/nabos", "PrivateDevices=yes"},
	} {
		unit := read(t, filepath.Join(rootfsDir, "usr/lib/systemd/system", name+".service"))
		for _, line := range append(required, "ExecStart=/usr/bin/"+name, "ProtectSystem=strict", "NoNewPrivileges=yes") {
			if !strings.Contains(unit, line+"\n") {
				t.Errorf("%s missing %q", name, line)
			}
		}
		for _, line := range strings.Split(unit, "\n") {
			if (strings.HasPrefix(line, "AmbientCapabilities=") || strings.HasPrefix(line, "BusName=")) && name == "nab-hardware" || strings.HasPrefix(line, "SupplementaryGroups=") && name == "nabos" || strings.Contains(line, "mosquitto") || line == "DefaultDependencies=no" {
				t.Errorf("%s unexpected %q", name, line)
			}
		}
	}
	hardwareUnit := read(t, filepath.Join(rootfsDir, "usr/lib/systemd/system/nab-hardware.service"))
	for _, forbidden := range []string{"CAP_SYS_RAWIO", " video", " kmem", "/dev/mem", "/dev/vcio"} {
		if strings.Contains(hardwareUnit, forbidden) {
			t.Errorf("raw memory authority remains: %s", forbidden)
		}
	}
	allowedPaths := map[string]bool{"-/sys/class/leds/multi:indicator-0/sync": true}
	for i := 0; i < 5; i++ {
		for _, attr := range []string{"brightness", "multi_intensity"} {
			allowedPaths[fmt.Sprintf("-/sys/class/leds/multi:indicator-%d/%s", i, attr)] = true
		}
		device := fmt.Sprintf(`sys-class-leds-multi:indicator\x2d%d.device`, i)
		for _, directive := range []string{"Requires=", "After="} {
			found := false
			for _, line := range strings.Split(hardwareUnit, "\n") {
				if strings.HasPrefix(line, directive) && strings.Contains(line, device) {
					found = true
				}
			}
			if !found {
				t.Errorf("missing LED readiness %s%s", directive, device)
			}
		}
	}
	for _, line := range strings.Split(hardwareUnit, "\n") {
		if paths, ok := strings.CutPrefix(line, "ReadWritePaths="); ok {
			for _, path := range strings.Fields(paths) {
				if !allowedPaths[path] {
					t.Errorf("unexpected writable hardware path: %s", path)
				}
				delete(allowedPaths, path)
			}
		}
	}
	if len(allowedPaths) != 0 {
		t.Errorf("missing writable LED attributes: %v", allowedPaths)
	}
	for _, file := range []string{"etc/mosquitto/mosquitto.conf", "usr/lib/systemd/system/nab-core.service", "usr/lib/systemd/system/nab-service.service"} {
		if _, err := os.Stat(filepath.Join(rootfsDir, file)); !os.IsNotExist(err) {
			t.Errorf("obsolete file remains: %s", file)
		}
	}
	config := read(t, filepath.Join(repo, "nix/system.nix"))
	operator := regexp.MustCompile(`(?s)nabos = \{(.*?)\};`).FindStringSubmatch(config)
	if operator == nil || strings.Contains(operator[1], "extraGroups") {
		t.Error("operator must not inherit hardware groups")
	}
	for _, required := range []string{`nab-hardware = fixedUser 1002 "/run/nab-hardware" [ "gpio" ];`, `nab-hardware.gid = 1002;`, `createHome = false;`, `hashedPassword = "!";`} {
		if !strings.Contains(config, required) {
			t.Errorf("NixOS account configuration lacks %s", required)
		}
	}
	udev := read(t, filepath.Join(rootfsDir, "etc/udev/rules.d/60-nabos.rules"))
	i2cRule := `SUBSYSTEM=="i2c-dev", KERNEL=="i2c-1", GROUP:="nab-hardware", MODE:="0660", TAG+="systemd"`
	for _, rule := range []string{
		`SUBSYSTEM=="gpio", KERNEL=="gpiochip*", GROUP="gpio", MODE="0660"`,
		i2cRule,
	} {
		if !strings.Contains(udev, rule+"\n") {
			t.Errorf("missing hardware device access rule %s", rule)
		}
	}
	for _, line := range strings.Split(udev, "\n") {
		if !strings.HasPrefix(line, "#") && strings.Contains(line, "i2c") && line != i2cRule {
			t.Errorf("unexpected I2C access rule: %s", line)
		}
	}
	units, err := filepath.Glob(filepath.Join(rootfsDir, "usr/lib/systemd/system/*.service"))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range units {
		for _, line := range strings.Split(read(t, file), "\n") {
			if (strings.HasPrefix(line, "Group=") || strings.HasPrefix(line, "SupplementaryGroups=")) &&
				strings.Contains(line, "nab-hardware") && filepath.Base(file) != "nab-hardware.service" {
				t.Errorf("private hardware group granted to %s: %s", file, line)
			}
		}
	}
	policy := read(t, filepath.Join(rootfsDir, "etc/dbus-1/system.d/io.github.guilhem.NabHardware1.conf"))
	for _, rule := range []string{`<deny own="io.github.guilhem.NabHardware1"/>`, `<deny send_destination="io.github.guilhem.NabHardware1"/>`, `<policy user="nab-app">`, `<allow own="io.github.guilhem.NabHardware1"/>`, `<allow send_destination="io.github.guilhem.NabHardware1"/>`} {
		if !strings.Contains(policy, rule) {
			t.Errorf("missing hardware bus policy %s", rule)
		}
	}
}

func TestDeviceCorePrivateDevices(t *testing.T) {
	unit := read(t, filepath.Join(rootfsDir, "usr/lib/systemd/system/device-core.service"))
	for _, required := range []string{"PrivateDevices=yes", "CapabilityBoundingSet=", "NoNewPrivileges=yes", "Environment=XDG_RUNTIME_DIR=/run/device-core"} {
		if !strings.Contains(unit, required+"\n") {
			t.Errorf("missing device isolation setting %q", required)
		}
	}
	for _, forbidden := range []string{"DeviceAllow=", "BindPaths=", "BindReadOnlyPaths=", "AmbientCapabilities=", "/dev/"} {
		if strings.Contains(unit, forbidden) {
			t.Errorf("device access restored by %q", forbidden)
		}
	}
	config := read(t, filepath.Join(repo, "nix/system.nix"))
	for _, required := range []string{`alsa.enable = true; pulse.enable = true;`, `systemd.user.services.pipewire = { wantedBy = [ "default.target" ];`, `systemd.user.services.pipewire-pulse = { wantedBy = [ "default.target" ];`, `ConditionUser = [ "" "nab-audio" ];`, `touch $out/var/lib/systemd/linger/nab-audio`} {
		if !strings.Contains(config, required) {
			t.Errorf("PipeWire user session configuration lacks %s", required)
		}
	}
}

func TestLVAUnitAdvertisedOnlyWhenInstalled(t *testing.T) {
	config := read(t, filepath.Join(repo, "nix/system.nix"))
	for _, required := range []string{
		`hasVoice = nabosTarget == "zero2-arm64" && packages.lva != null;`,
		`linux-voice-assistant = mkIf hasVoice {`,
		`DEVICE_CORE_LVA_UNIT=${lib.optionalString hasVoice "linux-voice-assistant.service"}`,
		`rm $out/lib/systemd/system/linux-voice-assistant.service`,
	} {
		if !strings.Contains(config, required) {
			t.Errorf("LVA availability lacks %s", required)
		}
	}
}

func TestFixedServiceAccounts(t *testing.T) {
	config := read(t, filepath.Join(repo, "nix/system.nix"))
	for _, required := range []string{
		`uid = 1000; group = "nabos";`, `home = "/var/lib/nabos/admin"; createHome = false;`,
		`nab-app = fixedUser 1001 "/data/nabos" [ "nab-media" ];`,
		`device-core = fixedUser 1003 "/data/device-core" [ "nab-media" "nab-audio" ];`,
		`nab-audio = fixedUser 1004 "/var/lib/nabos/lva" [ "audio" ];`,
		`group = (builtins.elemAt [ "nab-app" "nab-hardware" "device-core" "nab-audio" ] (uid - 1001));`,
		`nabos.gid = 1000;`, `nab-app.gid = 1001;`, `device-core.gid = 1003;`, `nab-audio.gid = 1004;`,
	} {
		if !strings.Contains(config, required) {
			t.Errorf("fixed account configuration lacks %s", required)
		}
	}
}

func TestDeviceCoreSystemPolkitAccountBoundary(t *testing.T) {
	imageTools(t, "node")
	rules := read(t, filepath.Join(rootfsDir, "etc/polkit-1/rules.d/50-nabos.rules"))
	run(t, "", "node", "-e", `const assert = require('node:assert/strict');
const polkit = {Result: {YES:'yes', NO:'no', NOT_HANDLED:'not_handled'}, rules:[], addRule(f) { this.rules.push(f); }};
`+rules+`
const decide = (id, subject, unit='', verb='') => polkit.rules[0]({id,lookup:k=>({unit,verb}[k])},subject);
const core = {user:'device-core'};
for (const id of ['org.freedesktop.login1.reboot','org.freedesktop.login1.power-off', 'org.freedesktop.timedate1.set-time']) {
  assert.equal(decide(id,core),'yes');
  for (const user of ['nab-app','nab-hardware','nab-audio'])
    assert.equal(decide(id,{user}),'no');
}
for (const unit of ['linux-voice-assistant.service','ssh.service','systemd-timesyncd.service','nabos-rauc-manual.service']) {
  const id='org.freedesktop.systemd1.manage-units';
  assert.equal(decide(id,core,unit,'start'),'yes');
  assert.equal(decide(id,{user:'nab-app'},unit,'start'),'no');
  assert.equal(decide(id,core,unit,'enable'),'no');
}
`)
}

func TestNetworkLockTmpfilesPreservesInode(t *testing.T) {
	imageTools(t, "systemd-tmpfiles")
	config := read(t, filepath.Join(rootfsDir, "usr/lib/tmpfiles.d/nabos.conf"))
	var lockRules string
	for _, line := range strings.Split(config, "\n") {
		if strings.Contains(line, "/run/lock/device-core") {
			// Exercise tmpfiles on an owned root. Production owners are checked
			// separately; this check needs no root or host account named nabos.
			fields := strings.Fields(line)
			if fields[0] == "d" && fields[3] != "root" || fields[0] == "f" && fields[3] != "device-core" || fields[5] != "-" {
				t.Fatalf("unsafe lock rule: %s", line)
			}
			fields[3], fields[4] = fmt.Sprint(os.Getuid()), fmt.Sprint(os.Getgid())
			lockRules += strings.Join(fields, " ") + "\n"
		}
	}
	if !strings.Contains(lockRules, "f /run/lock/device-core/network 0600") {
		t.Fatal("missing non-truncating lock file")
	}
	tmp := t.TempDir()
	conf := filepath.Join(tmp, "lock.conf")
	write(t, conf, lockRules)
	run(t, "", "systemd-tmpfiles", "--root="+tmp, "--create", conf)
	lock := filepath.Join(tmp, "run/lock/device-core/network")
	before := must(os.Stat(lock))
	write(t, lock, "must survive")
	run(t, "", "systemd-tmpfiles", "--root="+tmp, "--create", conf)
	run(t, "", "systemd-tmpfiles", "--root="+tmp, "--clean", conf)
	if !os.SameFile(before, must(os.Stat(lock))) || read(t, lock) != "must survive" {
		t.Fatal("tmpfiles replaced, truncated or cleaned the network lock")
	}
}
