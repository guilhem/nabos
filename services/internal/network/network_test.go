package network

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"github.com/guilhem/nabos/services/internal/device"
	"github.com/guilhem/nabos/services/internal/devicetest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

type testCore struct {
	mu                    sync.Mutex
	ssid                  []byte
	uuid, token, password string
	cancel                uint64
	fail                  bool
	getDelay              time.Duration
}

func (c *testCore) Connect(ssid []byte, security, password, uuid, token string) (uint64, *dbus.Error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ssid, c.uuid, c.token, c.password = ssid, uuid, token, password
	if c.fail {
		return 0, dbus.NewError("io.github.guilhem.DeviceCore1.Error.Refused", []any{"secret-password"})
	}
	return 42, nil
}
func (c *testCore) Cancel(id uint64) *dbus.Error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancel = id
	return nil
}
func (c *testCore) Reserve(token string) (string, *dbus.Error) {
	if token == "" {
		return "private-core-lease", nil
	}
	return token, nil
}
func (c *testCore) Authorized(token string) (bool, *dbus.Error) {
	return token == "private-core-lease", nil
}
func (c *testCore) Release(token string) *dbus.Error { return nil }
func (c *testCore) Scan() *dbus.Error                { return nil }
func (c *testCore) Forget(uuid string) *dbus.Error   { return nil }
func (c *testCore) Get(iface, name string) (dbus.Variant, *dbus.Error) {
	properties, _ := c.GetAll(iface)
	v, ok := properties[name]
	if !ok {
		return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.InvalidArgs", nil)
	}
	return v, nil
}
func (c *testCore) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	c.mu.Lock()
	delay := c.getDelay
	c.mu.Unlock()
	time.Sleep(delay)
	return map[string]dbus.Variant{
		"Status":   dbus.MakeVariant(Status{Mode: "client", Generation: "generation:1", Ready: true, Address: "192.168.1.12", SSID: SSID{255, 0, 65}, ProfileUUID: "12345678-1234-1234-1234-123456789abc", AttemptID: 42, Phase: "succeeded"}),
		"Networks": dbus.MakeVariant([]Network{{SSID: SSID{255, 0, 65}, Strength: 89, Security: "wpa-psk"}}),
		"Profiles": dbus.MakeVariant([]Profile{{UUID: "12345678-1234-1234-1234-123456789abc", SSID: SSID{255, 0, 65}}}),
	}, nil
}

func TestClientOnIsolatedBus(t *testing.T) {
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
	if _, err = bus.RequestName(Destination, dbus.NameFlagDoNotQueue); err != nil {
		t.Fatal(err)
	}
	devicetest.UseCurrentUser(t, "NABOS_DEVICE_USER")
	if signature := dbus.SignatureOf(Status{}).String(); signature != "(ssbsaystss)" {
		t.Fatal(signature)
	}
	core := &testCore{}
	if err = bus.Export(core, Path, Interface); err != nil {
		t.Fatal(err)
	}
	if err = bus.Export(core, Path, "org.freedesktop.DBus.Properties"); err != nil {
		t.Fatal(err)
	}
	conn, err := device.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := &Client{Client: conn}
	ctx := context.Background()
	snapshot, err := client.Read(ctx)
	if err != nil || !snapshot.Status.ClientReady() || snapshot.Status.AttemptID != 42 || snapshot.Networks[0].SSID.String() != "0xff0041" {
		t.Fatalf("snapshot: %v", err)
	}
	encoded, _ := json.Marshal(snapshot)
	if !strings.Contains(string(encoded), `"ssid":[255,0,65]`) {
		t.Fatalf("SSID bytes not preserved: %s", encoded)
	}
	status, err := client.ReadStatus(ctx)
	if err != nil || status.ProfileUUID != snapshot.Profiles[0].UUID {
		t.Fatal("status/profile identity", err)
	}
	lease, err := client.Reserve(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := client.Reserve(ctx, lease)
	if err != nil || renewed != lease {
		t.Fatal("renewal rotated lease", err)
	}
	if ok, err := client.Authorized(ctx, lease); err != nil || !ok {
		t.Fatal("authorization", err)
	}
	if ok, err := client.Authorized(ctx, "forged"); err != nil || ok {
		t.Fatal("forged lease", err)
	}
	id, err := client.Connect(ctx, SSID{255, 0, 65}, "wpa-psk", "secret-password", "", lease)
	if err != nil || id != 42 {
		t.Fatal("connect", err)
	}
	if err = client.Cancel(ctx, id); err != nil {
		t.Fatal(err)
	}
	core.mu.Lock()
	if len(core.ssid) != 3 || core.ssid[0] != 255 || core.token != lease || core.cancel != id {
		t.Fatal("wire arguments changed")
	}
	core.fail = true
	core.mu.Unlock()
	_, err = client.Connect(ctx, SSID("wifi"), "wpa-psk", "secret-password", "", "")
	if !errors.Is(err, ErrRefused) || strings.Contains(err.Error(), "secret-password") {
		t.Fatal("unsafe error", err)
	}
	if err = client.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if err = client.Forget(ctx, status.ProfileUUID); err != nil {
		t.Fatal(err)
	}
	if err = client.Release(ctx, lease); err != nil {
		t.Fatal(err)
	}
	core.mu.Lock()
	core.getDelay = 100 * time.Millisecond
	core.mu.Unlock()
	delayed, done := context.WithTimeout(ctx, 10*time.Millisecond)
	start := time.Now()
	if _, err := client.ReadStatus(delayed); err == nil || time.Since(start) > 500*time.Millisecond {
		t.Fatal("delayed call was not bounded", err)
	}
	done()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	start = time.Now()
	if _, err := client.ReadStatus(cancelled); err == nil || time.Since(start) > time.Second {
		t.Fatal("cancelled call was not bounded", err)
	}
}

func TestReadinessUsesAuthoritativeSnapshot(t *testing.T) {
	if (Status{Mode: "client", Address: "192.0.2.10"}).ClientReady() {
		t.Fatal("address alone overrides daemon readiness")
	}
	if !(Status{Mode: "client", Ready: true}).ClientReady() {
		t.Fatal("daemon readiness ignored")
	}
	if (Status{Mode: "hotspot", Ready: true}).ClientReady() {
		t.Fatal("hotspot accepted as client")
	}
}
