package rabbit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
