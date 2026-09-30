// Package devicetest supplies native D-Bus structures on an isolated bus.
// It exercises clients; the real daemon is used for end-to-end tests.
package devicetest

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/guilhem/nabos/services/internal/device"
)

type Fixture struct {
	Mu                             sync.Mutex
	Conn                           *dbus.Conn
	Revision                       string
	Settings                       device.Settings
	ClockQuality                   string
	ClockUnix                      int64
	ConnectivityState              string
	Keys                           string
	SSHRevision                    string
	FailSSH, RejectConfig          bool
	VoiceSupported                 bool
	VoiceStatus                    string
	LastVoiceCommand               string
	UpdateState                    device.UpdateStatus
	Configured                     bool
	Catalog                        []device.Release
	Checked                        int
	CheckDelay                     time.Duration
	InstallTag, InstallChannel     string
	InstallAutomatic, InstallRetry bool
	AgentSender                    string
	AgentPath                      dbus.ObjectPath
	MaintenanceAgents              bool
	ManagerReady, Maintenance      bool
	AgentRegistrations             int
	RejectAgent                    bool
	Audio                          device.AudioStatus
}

// RequireProcessFD probes the fixture connection, not a service's authentication.
// Integration CI requires this capability; older unit-test hosts may skip.
func RequireProcessFD(t *testing.T, conn *dbus.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), device.Timeout)
	defer cancel()
	var credentials map[string]dbus.Variant
	if err := conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetConnectionCredentials", 0, conn.Names()[0]).Store(&credentials); err != nil {
		t.Fatal(err)
	}
	v, ok := credentials["ProcessFD"]
	if !ok {
		if os.Getenv("NABOS_INTEGRATION") == "1" {
			t.Fatal("integration fixtures require D-Bus ProcessFD credentials")
		}
		t.Skip("host D-Bus does not provide ProcessFD credentials")
	}
	var fd dbus.UnixFD
	if err := v.Store(&fd); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Close(int(fd)); err != nil {
		t.Fatal(err)
	}
}

func New(t *testing.T) *Fixture {
	t.Helper()
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
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if reply, err := conn.RequestName(device.Destination, dbus.NameFlagDoNotQueue); err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("bus ownership: %v %v", reply, err)
	}
	return Attach(t, conn)
}
func Attach(t *testing.T, conn *dbus.Conn) *Fixture {
	t.Helper()
	f := &Fixture{Conn: conn, Revision: "instance:1", Settings: device.Settings{Locale: "fr_FR", Timezone: "Europe/Paris", Volume: 100, AutoCheck: true, Updates: device.Updates{Channel: "stable", Start: device.HM{Hour: 3}, End: device.HM{Hour: 5}}}, ClockQuality: "ntp", ClockUnix: time.Now().Unix(), ConnectivityState: "ok", VoiceSupported: true, VoiceStatus: "idle", UpdateState: device.UpdateStatus{Current: "v1.0.0", State: "idle"}, Audio: device.AudioStatus{State: "idle", Volume: 100}}
	f.MaintenanceAgents, f.ManagerReady = true, true
	f.SSHRevision = "ssh-instance:1"
	for _, domain := range []string{"Config", "System", "Voice", "Updates", "Manager", "Audio"} {
		if err := conn.Export(f, device.Path(domain), device.Interface(domain)); err != nil {
			t.Fatal(err)
		}
		if err := conn.Export(properties{f, domain}, device.Path(domain), "org.freedesktop.DBus.Properties"); err != nil {
			t.Fatal(err)
		}
	}
	return f
}
func (f *Fixture) Read() (string, device.Settings, *dbus.Error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return f.Revision, f.Settings, nil
}
func (f *Fixture) Update(revision string, settings device.Settings) (string, *dbus.Error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if revision != f.Revision || f.RejectConfig {
		return "", dbus.MakeFailedError(errors.New("rejected"))
	}
	f.Settings = settings
	f.Revision += ":1"
	return f.Revision, nil
}
func (f *Fixture) Reboot() *dbus.Error   { return nil }
func (f *Fixture) PowerOff() *dbus.Error { return nil }
func (f *Fixture) SetTime(unix int64) *dbus.Error {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.ClockUnix, f.ClockQuality = unix/1e6, "manual"
	return nil
}
func (f *Fixture) Clock() (string, int64, *dbus.Error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return f.ClockQuality, f.ClockUnix, nil
}
func (f *Fixture) Connectivity() (string, *dbus.Error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return f.ConnectivityState, nil
}
func (f *Fixture) GetSSHKeys() (string, string, *dbus.Error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return f.SSHRevision, f.Keys, nil
}
func (f *Fixture) SetSSHKeys(revision, keys string) (string, *dbus.Error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if f.FailSSH {
		return "", dbus.MakeFailedError(errors.New("secret remote key error"))
	}
	if revision != f.SSHRevision {
		return "", dbus.MakeFailedError(errors.New("stale SSH revision"))
	}
	f.Keys = keys
	f.SSHRevision += ":1"
	return f.SSHRevision, nil
}
func (f *Fixture) Enable(on bool) *dbus.Error {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.Settings.VoiceEnabled = on
	return nil
}
func (f *Fixture) Command(command string) *dbus.Error {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.LastVoiceCommand = command
	return nil
}
func (f *Fixture) RegisterAgent(sender dbus.Sender, path dbus.ObjectPath) *dbus.Error {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.AgentRegistrations++
	if f.RejectAgent {
		return dbus.NewError("org.freedesktop.DBus.Error.AccessDenied", nil)
	}
	f.AgentSender, f.AgentPath = string(sender), path
	return nil
}
func (f *Fixture) Releases(channel string) ([]device.Release, *dbus.Error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return f.Catalog, nil
}
func (f *Fixture) Check() ([]device.Release, *dbus.Error) {
	f.Mu.Lock()
	delay := f.CheckDelay
	f.Mu.Unlock()
	time.Sleep(delay)
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.Checked++
	return f.Catalog, nil
}
func (f *Fixture) Install(tag, channel string, automatic, retry bool) (string, *dbus.Error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.InstallTag, f.InstallChannel, f.InstallAutomatic, f.InstallRetry = tag, channel, automatic, retry
	f.UpdateState.OperationID = "operation:" + strconv.Itoa(f.Checked)
	return f.UpdateState.OperationID, nil
}
func (f *Fixture) Start(kind, source string) (string, *dbus.Error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.Audio.ID, f.Audio.State = "audio-id", "playing"
	return f.Audio.ID, nil
}
func (f *Fixture) Stop(id string) *dbus.Error {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.Audio.State = "idle"
	return nil
}
func (f *Fixture) Wait(id string) (string, *dbus.Error) { return "completed", nil }
func (f *Fixture) SetVolume(volume uint32) *dbus.Error {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.Audio.Volume = volume
	return nil
}

type properties struct {
	f      *Fixture
	domain string
}

func (p properties) Get(iface, name string) (dbus.Variant, *dbus.Error) {
	p.f.Mu.Lock()
	defer p.f.Mu.Unlock()
	var value any
	switch p.domain + "." + name {
	case "Voice.Supported":
		value = p.f.VoiceSupported
	case "Voice.Status":
		value = p.f.VoiceStatus
	case "Updates.Status":
		value = p.f.UpdateState
	case "Updates.Configured":
		value = p.f.Configured
	case "Manager.Capabilities":
		value = []string{"network", "audio", "config", "system"}
		if p.f.Configured {
			value = append(value.([]string), "updates")
		}
		if p.f.MaintenanceAgents {
			value = append(value.([]string), "maintenance-agents")
		}
	case "Manager.Ready":
		value = p.f.ManagerReady
	case "Manager.Maintenance":
		value = p.f.Maintenance
	case "Audio.Status":
		value = p.f.Audio
	default:
		return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.UnknownProperty", nil)
	}
	return dbus.MakeVariant(value), nil
}
func (f *Fixture) Agent(ctx context.Context, method string, args ...any) *dbus.Call {
	f.Mu.Lock()
	sender, path := f.AgentSender, f.AgentPath
	f.Mu.Unlock()
	return f.Conn.Object(sender, path).CallWithContext(ctx, device.Interface("Agent")+"."+method, 0, args...)
}
