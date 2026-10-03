package main

import (
	"context"
	"fmt"
	"github.com/godbus/dbus/v5"
	"github.com/guilhem/nabos/services/internal/device"
	"github.com/guilhem/nabos/services/internal/devicetest"
	"github.com/guilhem/nabos/services/internal/rabbit"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// The real bus supplies ProcessFD. Only the systemd unit attribution is mocked.
type deviceIdentity struct {
	mu          sync.Mutex
	requester   string
	conn        *dbus.Conn
	unit, id    string
	afterLookup func()
}
type identityUnit struct {
	identity *deviceIdentity
	hardware bool
}

func (u identityUnit) Get(iface, name string) (dbus.Variant, *dbus.Error) {
	if iface != "org.freedesktop.systemd1.Unit" || name != "Id" {
		return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.UnknownProperty", nil)
	}
	if u.hardware {
		return dbus.MakeVariant("nab-hardware.service"), nil
	}
	u.identity.mu.Lock()
	defer u.identity.mu.Unlock()
	return dbus.MakeVariant(u.identity.id), nil
}
func (i *deviceIdentity) GetUnitByPIDFD(sender dbus.Sender, fd dbus.UnixFD) (dbus.ObjectPath, string, []byte, *dbus.Error) {
	defer syscall.Close(int(fd))
	if _, err := os.ReadFile(fmt.Sprintf("/proc/self/fdinfo/%d", fd)); err != nil {
		return "", "", nil, dbus.MakeFailedError(err)
	}
	i.mu.Lock()
	unit, hook := i.unit, i.afterLookup
	i.mu.Unlock()
	if string(sender) != i.requester {
		// Both daemons are fixtures in this process. Use the requester's
		// installed owner subscription to distinguish hardware from audio.
		ctx, cancel := context.WithTimeout(context.Background(), device.Timeout)
		defer cancel()
		var rules map[string][]string
		if err := i.conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.Debug.Stats.GetAllMatchRules", 0).Store(&rules); err != nil {
			return "", "", nil, dbus.MakeFailedError(err)
		}
		for _, rule := range rules[string(sender)] {
			if strings.Contains(rule, "io.github.guilhem.NabHardware1") {
				return "/org/freedesktop/systemd1/unit/hardware", "nab-hardware.service", []byte{1}, nil
			}
		}
	}
	if hook != nil {
		hook()
	}
	return "/org/freedesktop/systemd1/unit/device", unit, []byte{1}, nil
}

var deviceIdentities = map[*App]*deviceIdentity{}

func installDeviceIdentity(t *testing.T, a *App, conn *dbus.Conn) {
	t.Helper()
	i := &deviceIdentity{requester: a.device.Conn.Names()[0], conn: conn, unit: "device-core.service", id: "device-core.service"}
	if reply, err := conn.RequestName("org.freedesktop.systemd1", dbus.NameFlagDoNotQueue); err != nil || reply != dbus.RequestNameReplyPrimaryOwner && reply != dbus.RequestNameReplyAlreadyOwner {
		t.Fatal(reply, err)
	}
	if err := conn.Export(i, "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager"); err != nil {
		t.Fatal(err)
	}
	for path, hardware := range map[dbus.ObjectPath]bool{"/org/freedesktop/systemd1/unit/device": false, "/org/freedesktop/systemd1/unit/hardware": true} {
		if err := conn.Export(identityUnit{i, hardware}, path, "org.freedesktop.DBus.Properties"); err != nil {
			t.Fatal(err)
		}
	}
	deviceIdentities[a] = i
	t.Cleanup(func() { delete(deviceIdentities, a) })
}

func TestDeviceIdentityRejectsWrongUnitAndOwnerChange(t *testing.T) {
	a := testApp(t)
	f := appFixture(t, a)
	devicetest.RequireProcessFD(t, f.Conn)
	ctx := context.Background()
	owner, err := a.device.TrustedOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	i := deviceIdentities[a]
	for _, tc := range []struct{ unit, id string }{{"nabos.service", "nabos.service"}, {"device-core.service", "wrong.service"}} {
		i.mu.Lock()
		i.unit, i.id = tc.unit, tc.id
		i.mu.Unlock()
		if a.device.Authenticate(ctx, owner) == nil || a.agent.daemon(dbus.Sender(owner)) {
			t.Fatal("untrusted unit accepted", tc)
		}
	}
	i.mu.Lock()
	i.unit, i.id = "device-core.service", "device-core.service"
	i.mu.Unlock()
	i.mu.Lock()
	i.afterLookup = func() {
		if _, err := f.Conn.ReleaseName(device.Destination); err != nil {
			t.Error(err)
		}
	}
	i.mu.Unlock()
	if a.device.Authenticate(ctx, owner) == nil {
		t.Fatal("ownership change during authentication accepted")
	}
	i.mu.Lock()
	i.afterLookup = nil
	i.mu.Unlock()
	if _, err := f.Conn.RequestName(device.Destination, dbus.NameFlagDoNotQueue); err != nil {
		t.Fatal(err)
	}
	if a.device.Authenticate(ctx, owner) != nil {
		t.Fatal("trusted owner did not recover")
	}
}

func TestPublishedSettingsAuthenticateAndFenceConfigRead(t *testing.T) {
	for _, mode := range []string{"trusted", "untrusted", "owner changed during read"} {
		t.Run(mode, func(t *testing.T) {
			a := testApp(t)
			native := startNative(t, a)
			f := appFixture(t, a)
			ctx := context.Background()
			for _, locale := range []string{"fr_FR", "en_US"} {
				path := filepath.Join(a.env.SoundsDirs[0], locale, "settings-probe.wav")
				if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("audio fixture"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			replacement, err := dbus.ConnectSystemBus()
			if err != nil {
				t.Fatal(err)
			}
			defer replacement.Close()
			var reads atomic.Int32
			read := func(message dbus.Message) (string, device.Settings, *dbus.Error) {
				reads.Add(1)
				if message.Headers[dbus.FieldDestination].Value() != f.Conn.Names()[0] {
					t.Error("config read used public name")
				}
				f.Mu.Lock()
				settings := f.Settings
				f.Mu.Unlock()
				settings.Locale = "en_US"
				if mode == "owner changed during read" {
					if _, err := f.Conn.ReleaseName(device.Destination); err != nil {
						t.Error(err)
					}
					if reply, err := replacement.RequestName(device.Destination, dbus.NameFlagDoNotQueue); err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
						t.Error(reply, err)
					}
				}
				return "settings:1", settings, nil
			}
			if err := f.Conn.ExportMethodTable(map[string]interface{}{"Read": read}, device.Path("Config"), device.Interface("Config")); err != nil {
				t.Fatal(err)
			}
			identity := deviceIdentities[a]
			if mode == "untrusted" {
				identity.mu.Lock()
				identity.unit = "other.service"
				identity.mu.Unlock()
			}
			a.publishSettings(ctx) // also used before deviceLoop at startup
			expectedReads := int32(1)
			if mode == "untrusted" {
				expectedReads = 0
			}
			if reads.Load() != expectedReads {
				t.Fatal("read from untrusted owner", reads.Load())
			}
			identity.mu.Lock()
			identity.unit = "device-core.service"
			identity.mu.Unlock()
			if mode == "owner changed during read" {
				if _, err := replacement.ReleaseName(device.Destination); err != nil {
					t.Fatal(err)
				}
				if _, err := f.Conn.RequestName(device.Destination, dbus.NameFlagDoNotQueue); err != nil {
					t.Fatal(err)
				}
			}
			if result, err := a.rabbit.Do(ctx, sequence("settings-probe.wav", "")); err != nil || result.Err() != nil {
				t.Fatal(result, err)
			}
			locale := "fr_FR"
			if mode == "trusted" {
				locale = "en_US"
			}
			native.mu.Lock()
			defer native.mu.Unlock()
			if len(native.plays) == 0 || !strings.HasSuffix(native.plays[len(native.plays)-1].Source, locale+"/settings-probe.wav") {
				t.Fatal("published unfenced locale", native.plays)
			}
		})
	}
}

func TestDeviceLoopPinsAllConfigurationReads(t *testing.T) {
	a := testApp(t)
	startNative(t, a)
	f := appFixture(t, a)
	var reads atomic.Int32
	read := func(message dbus.Message) (string, device.Settings, *dbus.Error) {
		reads.Add(1)
		if message.Headers[dbus.FieldDestination].Value() != f.Conn.Names()[0] {
			t.Error("refresh or publication reread config through public name")
		}
		f.Mu.Lock()
		defer f.Mu.Unlock()
		return f.Revision, f.Settings, nil
	}
	if err := f.Conn.ExportMethodTable(map[string]interface{}{"Read": read}, device.Path("Config"), device.Interface("Config")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.deviceLoop(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	pynabWait(t, "authenticated config refresh", time.Second, func() bool { return reads.Load() > 0 })
}

func TestMaintenanceResumeAuthenticationFailureNeverUnpausesEngine(t *testing.T) {
	for _, mode := range []string{"denied", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			a := testApp(t)
			native := startNative(t, a)
			ctx := context.Background()
			if err := a.do(ctx, rabbit.Command{Action: rabbit.Info, InfoID: "resume-test", Animation: &rabbit.Animation{Tempo: 1, Frames: [][3]rabbit.RGB{{{1, 2, 3}, {}, {}}}}}, time.Second); err != nil {
				t.Fatal(err)
			}
			pynabWait(t, "background animation running", time.Second, func() bool { native.mu.Lock(); defer native.mu.Unlock(); return native.leds > 3 })
			a.agent.holdRecovery()
			native.mu.Lock()
			frozen := native.leds
			native.mu.Unlock()
			owner, err := a.device.TrustedOwner(ctx)
			if err != nil {
				t.Fatal(err)
			}
			identity := deviceIdentities[a]
			entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			identity.mu.Lock()
			identity.afterLookup = func() {
				close(entered)
				<-release
				if mode == "denied" {
					identity.mu.Lock()
					identity.id = "wrong.service"
					identity.mu.Unlock()
				}
				close(returned)
			}
			identity.mu.Unlock()
			done := make(chan *dbus.Error, 1)
			go func() { a.agent.mu.Lock(); defer a.agent.mu.Unlock(); done <- a.agent.resume(owner) }()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("resume did not reach authentication")
			}
			if a.rabbit.Ready() {
				t.Error("engine reopened during resume-time authentication")
			}
			if mode == "denied" {
				unblock()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Error("failed authentication resumed engine")
				}
			case <-time.After(device.Timeout + 2*time.Second):
				t.Fatal("resume timeout not bounded")
			}
			unblock()
			<-returned
			time.Sleep(50 * time.Millisecond)
			if a.rabbit.Ready() || len(a.mediaGate) != 1 {
				t.Error("failed resume reopened admission")
			}
			native.mu.Lock()
			after := native.leds
			native.mu.Unlock()
			if after != frozen {
				t.Error("background work ran during rejected resume", frozen, after)
			}
			identity.mu.Lock()
			identity.afterLookup = nil
			identity.id = "device-core.service"
			identity.mu.Unlock()
			a.agent.mu.Lock()
			retryErr := a.agent.resume(owner)
			a.agent.mu.Unlock()
			if retryErr != nil {
				t.Fatal("trusted retry failed", retryErr)
			}
			pynabWait(t, "trusted retry resumes engine", time.Second, a.rabbit.Ready)
		})
	}
}

func TestMaintenanceResumeReclosesAfterFinalAuthenticationTimeout(t *testing.T) {
	a := testApp(t)
	startNative(t, a)
	a.agent.holdRecovery()
	owner, err := a.device.TrustedOwner(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	i := deviceIdentities[a]
	entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	i.mu.Lock()
	i.afterLookup = func() {
		if calls.Add(1) == 2 {
			close(entered)
			<-release
			close(returned)
		}
	}
	i.mu.Unlock()
	done := make(chan *dbus.Error, 1)
	go func() {
		a.agent.mu.Lock()
		defer a.agent.mu.Unlock()
		done <- a.agent.resume(owner)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("final authentication did not run")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("final authentication timeout accepted")
		}
	case <-time.After(device.Timeout + 2*time.Second):
		t.Fatal("final authentication timeout was not bounded")
	}
	if a.rabbit.Ready() || len(a.mediaGate) != 1 {
		t.Fatal("final authentication failure left the engine or admission reopened")
	}
	unblock()
	<-returned
	i.mu.Lock()
	i.afterLookup = nil
	i.mu.Unlock()
	a.agent.mu.Lock()
	retryErr := a.agent.resume(owner)
	a.agent.mu.Unlock()
	if retryErr != nil {
		t.Fatal("trusted retry failed", retryErr)
	}
	pynabWait(t, "trusted retry resumes engine", time.Second, a.rabbit.Ready)
}
