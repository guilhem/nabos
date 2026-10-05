package main

import (
	"context"
	"github.com/godbus/dbus/v5"
	"github.com/guilhem/nabos/services/internal/device"
	"github.com/guilhem/nabos/services/internal/devicetest"
	"github.com/guilhem/nabos/services/internal/rabbit"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This hook delays real UnixUser replies; credentials are never fabricated.
type deviceIdentity struct {
	mu          sync.Mutex
	afterLookup func()
}

var deviceIdentities = map[*App]*deviceIdentity{}

func installDeviceIdentity(t *testing.T, a *App, _ *dbus.Conn) {
	t.Helper()
	i := &deviceIdentity{}
	conn, err := dbus.ConnectSystemBus(devicetest.CredentialReplies(func() {
		i.mu.Lock()
		hook := i.afterLookup
		i.mu.Unlock()
		if hook != nil {
			hook()
		}
	})...)
	if err != nil {
		t.Fatal(err)
	}
	a.device.Conn.Close()
	a.device.Conn = conn
	if err := conn.Export(a.agent, device.Path("Agent"), device.Interface("Agent")); err != nil {
		t.Fatal(err)
	}
	deviceIdentities[a] = i
	t.Cleanup(func() { delete(deviceIdentities, a) })
}

func TestDeviceIdentityRejectsWrongAccountAndOwnerChange(t *testing.T) {
	a := testApp(t)
	f := appFixture(t, a)
	ctx := context.Background()
	owner, err := a.device.TrustedOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{devicetest.WrongUser(t), "nabos-no-such-test-account", ""} {
		t.Setenv("NABOS_DEVICE_USER", expected)
		if a.device.Authenticate(ctx, owner) == nil || a.agent.daemon(dbus.Sender(owner)) {
			t.Fatal("untrusted account accepted", expected)
		}
	}
	devicetest.UseCurrentUser(t, "NABOS_DEVICE_USER")
	i := deviceIdentities[a]
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
			if mode == "untrusted" {
				t.Setenv("NABOS_DEVICE_USER", devicetest.WrongUser(t))
			}
			a.publishSettings(ctx) // also used before deviceLoop at startup
			expectedReads := int32(1)
			if mode == "untrusted" {
				expectedReads = 0
			}
			if reads.Load() != expectedReads {
				t.Fatal("read from untrusted owner", reads.Load())
			}
			devicetest.UseCurrentUser(t, "NABOS_DEVICE_USER")
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
					if _, err := appFixture(t, a).Conn.ReleaseName(device.Destination); err != nil {
						t.Error(err)
					}
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
			identity.mu.Unlock()
			if _, err := appFixture(t, a).Conn.RequestName(device.Destination, dbus.NameFlagDoNotQueue); err != nil {
				t.Fatal(err)
			}
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
