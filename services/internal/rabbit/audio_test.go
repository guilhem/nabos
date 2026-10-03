package rabbit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/godbus/dbus/v5"
	"testing"

	"github.com/guilhem/nabos/services/internal/device"
)

func TestAudioUsesDedicatedConnectionOnExplicitBus(t *testing.T) {
	system := newBus(t)
	systemAddress := os.Getenv("NABOS_DEVICE_BUS_ADDRESS")
	private := newBus(t)
	t.Setenv("DBUS_SYSTEM_BUS_ADDRESS", systemAddress)
	client, err := device.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	a := &audio{}
	defer a.close()
	ctx := context.Background()
	id, err := a.start(ctx, source{"file", "/audio/test.mp3"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.wait(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := a.start(ctx, source{"file", "/audio/next.mp3"}); err != nil {
		t.Fatal(err)
	}
	if err := a.stop(); err != nil {
		t.Fatal(err)
	}
	private.mu.Lock()
	if len(private.starts) != 2 || len(private.waits) != 1 || len(private.stops) != 1 || private.startOwners[0] != private.startOwners[1] || private.startOwners[0] != private.waitOwners[0] || private.startOwners[0] != private.stopOwners[0] || private.startOwners[0] == client.Conn.Names()[0] {
		t.Error("audio did not retain its dedicated owner", private.startOwners, private.waitOwners, private.stopOwners)
	}
	private.mu.Unlock()
	t.Setenv("NABOS_DEVICE_BUS_ADDRESS", "unix:path="+filepath.Join(t.TempDir(), "missing-bus"))
	unavailable := &audio{}
	defer unavailable.close()
	if _, err := unavailable.start(ctx, source{"file", "/audio/host.mp3"}); !errors.Is(err, device.ErrUnavailable) {
		t.Fatal("unavailable explicit bus was not refused", err)
	}
	system.mu.Lock()
	defer system.mu.Unlock()
	if len(system.starts) != 0 {
		t.Fatal("audio reached the system bus", system.starts)
	}
}

// Delay the owner-loss notification deliberately: each operation must fence its
// daemon independently of the watcher before sending an ID or starting work.
func TestAudioLifecycleNeverMovesToReplacementDaemon(t *testing.T) {
	for _, operation := range []string{"stop", "wait", "start"} {
		t.Run(operation, func(t *testing.T) {
			fixture := newBus(t)
			a := &audio{}
			defer a.close()
			ctx := context.Background()
			id, err := a.start(ctx, source{"file", "/audio/first.mp3"})
			if err != nil {
				t.Fatal(err)
			}
			a.mu.Lock()
			conn := a.client.Conn
			a.mu.Unlock()
			if err := conn.RemoveMatchSignal(dbus.WithMatchInterface("org.freedesktop.DBus"), dbus.WithMatchMember("NameOwnerChanged"), dbus.WithMatchArg(0, device.Destination)); err != nil {
				t.Fatal(err)
			}
			replacement, err := dbus.Connect(os.Getenv("NABOS_DEVICE_BUS_ADDRESS"))
			if err != nil {
				t.Fatal(err)
			}
			defer replacement.Close()
			var calls atomic.Int32
			if err := replacement.ExportMethodTable(map[string]interface{}{
				"Start": func(kind, source string) (string, *dbus.Error) { calls.Add(1); return "new-id", nil },
				"Stop":  func(id string) *dbus.Error { calls.Add(1); return nil },
				"Wait":  func(id string) (string, *dbus.Error) { calls.Add(1); return "completed", nil },
			}, device.Path("Audio"), device.Interface("Audio")); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.conn.ReleaseName(device.Destination); err != nil {
				t.Fatal(err)
			}
			if reply, err := replacement.RequestName(device.Destination, dbus.NameFlagDoNotQueue); err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
				t.Fatal(reply, err)
			}
			switch operation {
			case "stop":
				err = a.stop()
			case "wait":
				err = a.wait(ctx, id)
			case "start":
				_, err = a.start(ctx, source{"file", "/audio/next.mp3"})
			}
			if !errors.Is(err, device.ErrUnavailable) || calls.Load() != 0 {
				t.Fatal("old audio lifecycle reached replacement daemon", err, calls.Load())
			}
		})
	}
}
