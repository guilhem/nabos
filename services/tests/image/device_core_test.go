package image

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Generated unit and account contracts run in image/test-service-accounts.py
// against the exact composed image; policy and data-safety tests stay local.
func TestHardwareDeviceAccessRules(t *testing.T) {
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
