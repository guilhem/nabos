// Package system wraps the OS services used by nab-service: logind, systemd,
// WirePlumber volume and network reachability.
package system

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

func systemBus() (*dbus.Conn, error) { return dbus.ConnectSystemBus() }

func call(dest, path, method string, args ...any) error {
	conn, err := systemBus()
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Object(dest, dbus.ObjectPath(path)).Call(method, 0, args...).Err
}

// Reboot or power off through logind (polkit rule for user nabos).
func Reboot() error {
	return call("org.freedesktop.login1", "/org/freedesktop/login1", "org.freedesktop.login1.Manager.Reboot", false)
}

func PowerOff() error {
	return call("org.freedesktop.login1", "/org/freedesktop/login1", "org.freedesktop.login1.Manager.PowerOff", false)
}

// StartUnit / StopUnit through systemd (polkit restricts allowed units).
func StartUnit(name string) error {
	return call("org.freedesktop.systemd1", "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager.StartUnit", name, "replace")
}

func StopUnit(name string) error {
	return call("org.freedesktop.systemd1", "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager.StopUnit", name, "replace")
}

// Pause active NTP while setting the clock. Use runtime jobs: timedated.SetNTP
// changes unit enablement under /etc, which is read-only on the appliance.
func SetTime(t time.Time) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := systemBus()
	if err != nil {
		return err
	}
	defer conn.Close()
	obj := conn.Object("org.freedesktop.timedate1", "/org/freedesktop/timedate1")
	const unit = "systemd-timesyncd.service"
	if exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", unit).Run() == nil {
		if err := exec.CommandContext(ctx, "systemctl", "--no-ask-password", "stop", unit).Run(); err != nil {
			return fmt.Errorf("pause NTP: %w", err)
		}
		// systemctl waits for the stop job, so timedated sees NTP inactive.
		defer func() {
			restore, done := context.WithTimeout(context.Background(), 10*time.Second)
			defer done()
			if startErr := exec.CommandContext(restore, "systemctl", "--no-ask-password", "start", unit).Run(); startErr != nil && err == nil {
				err = fmt.Errorf("resume NTP: %w", startErr)
			}
		}()
	}
	return obj.CallWithContext(ctx, "org.freedesktop.timedate1.SetTime", 0, t.UnixMicro(), false, false).Err
}

// BootID identifies the current boot (a manual time is only valid until reboot).
func BootID() string {
	b, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(b))
}

// SetVolume sets the default PipeWire sink volume (0-100) with wpctl.
func SetVolume(ctx context.Context, percent int) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "wpctl", "set-volume", "@DEFAULT_AUDIO_SINK@", fmt.Sprintf("%.2f", float64(percent)/100)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("wpctl: %v: %s", err, out)
	}
	return nil
}

// Network returns "ok" (Internet reachable), "lan" (address but no Internet)
// or "offline" (no usable address).
func Network(ctx context.Context, probe string) string {
	ifaces, _ := net.Interfaces()
	hasAddr := false
	for _, i := range ifaces {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if ip, ok := a.(*net.IPNet); ok && ip.IP.IsGlobalUnicast() {
				hasAddr = true
			}
		}
	}
	if !hasAddr {
		return "offline"
	}
	d := net.Dialer{Timeout: 5 * time.Second}
	c, err := d.DialContext(ctx, "tcp", probe)
	if err != nil {
		return "lan"
	}
	c.Close()
	return "ok"
}
