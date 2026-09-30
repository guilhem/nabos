package integration

import (
	"bufio"
	"os/exec"
	"strings"
	"testing"

	"github.com/godbus/dbus/v5"
	"github.com/guilhem/nabos/services/internal/network"
)

// The simulated Rust core deliberately never claims the production bus name.
// This fixture supplies only the already-connected Wi-Fi status on a private
// bus so MQTT integration can exercise subsequent administrator setup. The
// initial hotspot/physical authorization flow is exercised by wifi_test.go.
type readyNetwork struct{}

func (readyNetwork) Get(iface, name string) (dbus.Variant, *dbus.Error) {
	if iface != network.Interface || name != "Status" {
		return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.InvalidArgs", nil)
	}
	return dbus.MakeVariant(`{"mode":"client","address":"192.0.2.10","ssid":[84,101,115,116],"profile_uuid":"12345678-1234-1234-1234-123456789abc","attempt_id":1,"phase":"succeeded","error":""}`), nil
}

func readyNetworkBus(t *testing.T) {
	t.Helper()
	daemon := exec.Command(which(t, "dbus-daemon"), "--session", "--nofork", "--print-address=1")
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
	if _, err = conn.RequestName(network.Destination, dbus.NameFlagDoNotQueue); err != nil {
		t.Fatal(err)
	}
	if err = conn.Export(readyNetwork{}, network.Path, "org.freedesktop.DBus.Properties"); err != nil {
		t.Fatal(err)
	}
}
