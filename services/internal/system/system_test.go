package system

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

func TestSetVolumeWaitsForPipeWireSink(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	const wpctl = `#!/bin/sh
[ "$1 $2 $3" = "set-volume @DEFAULT_AUDIO_SINK@ 0.42" ] || exit 2
if [ ! -e "$NABOS_TEST_WPCTL_READY" ]; then
  : > "$NABOS_TEST_WPCTL_READY"
  exit 1
fi
`
	if err := os.WriteFile(filepath.Join(dir, "wpctl"), []byte(wpctl), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("NABOS_TEST_WPCTL_READY", ready)
	if err := SetVolume(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ready); err != nil {
		t.Fatal("first attempt did not fail", err)
	}
}

type testClock struct {
	ntp   string
	fail  atomic.Bool
	value atomic.Int64
}

func (c *testClock) SetTime(usec int64, relative, interactive bool) *dbus.Error {
	if _, err := os.Stat(c.ntp); err == nil || relative || interactive {
		return dbus.MakeFailedError(os.ErrPermission)
	}
	if c.fail.Load() {
		return dbus.MakeFailedError(os.ErrInvalid)
	}
	c.value.Store(usec)
	return nil
}

// Private bus and fake systemctl: this test can never change the host clock.
func TestManualClockPreservesRuntimeNTP(t *testing.T) {
	if _, err := exec.LookPath("dbus-daemon"); err != nil {
		t.Skip("dbus-daemon not installed")
	}
	daemon := exec.Command("dbus-daemon", "--session", "--nofork", "--print-address=1")
	out, err := daemon.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { daemon.Process.Kill(); daemon.Wait() })
	address, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DBUS_SYSTEM_BUS_ADDRESS", strings.TrimSpace(address))
	bus, err := dbus.ConnectSystemBus()
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close()
	if _, err = bus.RequestName("org.freedesktop.timedate1", dbus.NameFlagDoNotQueue); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	clock := &testClock{ntp: filepath.Join(dir, "ntp")}
	if err = bus.Export(clock, "/org/freedesktop/timedate1", "org.freedesktop.timedate1"); err != nil {
		t.Fatal(err)
	}
	const control = `#!/bin/sh
[ "$3" = systemd-timesyncd.service ] || exit 2
case "$1" in
is-active) test -f "$NABOS_TEST_NTP" ;;
--no-ask-password)
  printf '%s\n' "$2" >> "$NABOS_TEST_NTP.log"
  case "$2" in
  stop) /bin/rm -f "$NABOS_TEST_NTP" ;;
  start) : > "$NABOS_TEST_NTP" ;;
  *) exit 2 ;;
  esac ;;
*) exit 2 ;;
esac
`
	if err = os.WriteFile(filepath.Join(dir, "systemctl"), []byte(control), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("NABOS_TEST_NTP", clock.ntp)
	want := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name         string
		active, fail bool
	}{{"active", true, false}, {"failure", true, true}, {"inactive", false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			os.Remove(clock.ntp)
			os.Remove(clock.ntp + ".log")
			if tc.active {
				if err := os.WriteFile(clock.ntp, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			clock.fail.Store(tc.fail)
			if err := SetTime(want); (err != nil) != tc.fail {
				t.Fatalf("SetTime: %v", err)
			}
			if !tc.fail && clock.value.Load() != want.UnixMicro() {
				t.Fatal("clock not set")
			}
			_, err := os.Stat(clock.ntp)
			if (err == nil) != tc.active {
				t.Fatal("NTP state was changed")
			}
			log, _ := os.ReadFile(clock.ntp + ".log")
			wantLog := ""
			if tc.active {
				wantLog = "stop\nstart\n"
			}
			if string(log) != wantLog {
				t.Fatalf("unexpected jobs: %q", log)
			}
		})
	}
}
