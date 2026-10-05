package image

import (
	"fmt"
	"os"
	"path/filepath"
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
	for _, file := range []string{"etc/systemd/system/ssh.service.d/nabos.conf", "etc/ssh/sshd_config.d/00-nabos.conf"} {
		if !strings.Contains(read(t, filepath.Join(rootfsDir, file)), "/data/device-core/ssh/authorized_keys") {
			t.Errorf("SSH path in %s", file)
		}
	}
	voice := read(t, filepath.Join(rootfsDir, "usr/lib/systemd/system/linux-voice-assistant.service"))
	if !strings.Contains(voice, "ConditionPathExists=/data/device-core/voice-enabled") || !strings.Contains(voice, "--preferences-file /var/lib/nabos/lva/preferences.json") {
		t.Error("voice flag/preferences contract")
	}
	build := read(t, filepath.Join(imageDir, "build.sh"))
	for _, required := range []string{"for component in go rust device-core uboot", "verify-device-core", `"$root/usr/bin/device-core"`, "DEVICE_CORE_IMAGE_VERSION", "DEVICE_CORE_UPDATE_REPO", "device_core_revision", "device_core_archive_sha256", "device_core_binary_sha256"} {
		if !strings.Contains(build, required) {
			t.Errorf("build lacks %q", required)
		}
	}
	makefile := read(t, filepath.Join(repo, "Makefile"))
	for _, required := range []string{
		"$$out/inputs/$$component", "$$inputs/$$component/Cargo.lock", "$$inputs/$$component/cargo-vendor", "$$repo/build/sysroot/$$component/$$target", "--locked --offline", "export RUST_COMPONENT = nab-hardware", `"$$out/nabos" ./cmd/nabos`, "if [[ $$component == nab-hardware ]]", "--exclude=./.source", "cc_key=CC_$${rust_target//-/_}", `"$$cc_key=$$linker"`,
		`cargo vendor --locked --manifest-path "$$source/Cargo.toml" "$$component_inputs/cargo-vendor" > "$$component_inputs/cargo-vendor.toml"`,
		`cp "$$inputs/$$component/cargo-vendor.toml" "$$component_inputs/"`,
		`cargo --config "$$component_inputs/cargo-vendor.toml"`,
		`--config "source.vendored-sources.directory=\"$$component_inputs/cargo-vendor\""`,
	} {
		if !strings.Contains(makefile, required) {
			t.Errorf("Make lacks %q", required)
		}
	}
}

func TestHardwareApplicationImageContract(t *testing.T) {
	for name, required := range map[string][]string{
		"nab-hardware": {"User=nab-hardware", "Group=nab-hardware", "Type=dbus", "BusName=io.github.guilhem.NabHardware1", "SupplementaryGroups=gpio video kmem", "AmbientCapabilities=CAP_SYS_RAWIO", "CapabilityBoundingSet=CAP_SYS_RAWIO", "RuntimeDirectory=nab-hardware", "WorkingDirectory=/run/nab-hardware", `Requires=dev-i2c\x2d1.device`, `After=systemd-tmpfiles-setup.service tagtagtag-mixerd.service device-core.service dev-i2c\x2d1.device`},
		"nabos":        {"User=nab-app", "AmbientCapabilities=CAP_NET_BIND_SERVICE", "CapabilityBoundingSet=CAP_NET_BIND_SERVICE", "Environment=NABOS_HTTP_ADDR=:80", "Environment=NABOS_DATA_DIR=/data/nabos", "ReadWritePaths=/data/nabos", "RuntimeDirectory=nabos", "WorkingDirectory=/run/nabos", "PrivateDevices=yes"},
	} {
		unit := read(t, filepath.Join(rootfsDir, "usr/lib/systemd/system", name+".service"))
		for _, line := range append(required, "ExecStart=/usr/bin/"+name, "ProtectSystem=strict", "NoNewPrivileges=yes") {
			if !strings.Contains(unit, line+"\n") {
				t.Errorf("%s missing %q", name, line)
			}
		}
		for _, line := range strings.Split(unit, "\n") {
			if strings.HasPrefix(line, "ReadWritePaths=") && name == "nab-hardware" || strings.HasPrefix(line, "SupplementaryGroups=") && name == "nabos" || strings.Contains(line, "mosquitto") || line == "DefaultDependencies=no" {
				t.Errorf("%s unexpected %q", name, line)
			}
		}
	}
	for _, file := range []string{"etc/mosquitto/mosquitto.conf", "usr/lib/systemd/system/nab-core.service", "usr/lib/systemd/system/nab-service.service"} {
		if _, err := os.Stat(filepath.Join(rootfsDir, file)); !os.IsNotExist(err) {
			t.Errorf("obsolete file remains: %s", file)
		}
	}
	prepare := read(t, filepath.Join(imageDir, "prepare.sh"))
	if !strings.Contains(prepare, "usermod -G '' nabos") {
		t.Error("operator account must not inherit hardware groups")
	}
	udev := read(t, filepath.Join(rootfsDir, "etc/udev/rules.d/60-nabos.rules"))
	i2cRule := `SUBSYSTEM=="i2c-dev", KERNEL=="i2c-1", GROUP:="nab-hardware", MODE:="0660", TAG+="systemd"`
	for _, rule := range []string{
		`KERNEL=="mem", GROUP="kmem", MODE="0660"`,
		`KERNEL=="vcio", GROUP="video", MODE="0660"`,
		`SUBSYSTEM=="gpio", KERNEL=="gpiochip*", GROUP="gpio", MODE="0660"`,
		`KERNEL=="ear[01]", GROUP="gpio", MODE="0660"`,
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
	if !strings.Contains(prepare, "nab-hardware:1002") || !strings.Contains(prepare, "--user-group --no-create-home") {
		t.Error("hardware needs its fixed private account and primary group")
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
	prepare := read(t, filepath.Join(imageDir, "prepare.sh"))
	setup := read(t, filepath.Join(rootfsDir, "usr/lib/nabos/image-setup"))
	if !strings.Contains(prepare, "pipewire-alsa") || !strings.Contains(setup, "systemctl --global enable pipewire.service pipewire.socket") {
		t.Fatal("device-core audio requires the PipeWire ALSA plugin and user session")
	}
}

func TestLVAUnitAdvertisedOnlyWhenInstalled(t *testing.T) {
	setup := read(t, filepath.Join(rootfsDir, "usr/lib/nabos/image-setup"))
	_, block, ok := strings.Cut(setup, "# Advertise Voice only when finalize installed its executable environment.\n")
	if !ok {
		t.Fatal("LVA availability check missing")
	}
	block, _, ok = strings.Cut(block, "\n# pi-gen soft-blocks")
	if !ok {
		t.Fatal("end of LVA availability check missing")
	}
	for _, scenario := range []struct {
		name      string
		installed bool
		mode      os.FileMode
		want      string
	}{
		{"absent", false, 0, ""},
		{"not-executable", true, 0o644, ""},
		{"installed", true, 0o755, "linux-voice-assistant.service"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			tmp := t.TempDir()
			python := filepath.Join(tmp, "python")
			release := filepath.Join(tmp, "release.env")
			write(t, release, "NABOS_VERSION=test\n")
			if scenario.installed {
				if err := os.WriteFile(python, []byte("#!/bin/sh\n"), scenario.mode); err != nil {
					t.Fatal(err)
				}
			}
			script := strings.ReplaceAll(block, "/opt/linux-voice-assistant/.venv/bin/python", python)
			script = strings.ReplaceAll(script, "/etc/nabos/release.env", release)
			run(t, "", "bash", "-euo", "pipefail", "-c", script)
			if got := read(t, release); got != "NABOS_VERSION=test\nDEVICE_CORE_LVA_UNIT="+scenario.want+"\n" {
				t.Fatalf("unexpected release environment: %q", got)
			}
		})
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
