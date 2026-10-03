package devicetest

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/godbus/dbus/v5"
)

// InstallIdentity emulates only systemd unit attribution. The private bus must
// supply a real ProcessFD; each received descriptor is checked and closed.
// All fixture daemons share the test process, so unitFor may distinguish their
// requesting connections. A nil resolver attributes device-core.service.
// Attach deliberately does not install this: cmd/nabos owns its identity fixture.
func InstallIdentity(t *testing.T, conn *dbus.Conn, unitFor func(dbus.Sender) (string, error)) {
	t.Helper()
	RequireProcessFD(t, conn)
	if reply, err := conn.RequestName("org.freedesktop.systemd1", dbus.NameFlagDoNotQueue); err != nil || reply != dbus.RequestNameReplyPrimaryOwner && reply != dbus.RequestNameReplyAlreadyOwner {
		t.Fatalf("systemd fixture ownership: %v %v", reply, err)
	}
	if unitFor == nil {
		unitFor = func(dbus.Sender) (string, error) { return "device-core.service", nil }
	}
	if err := conn.Export(identityManager{conn, unitFor}, "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager"); err != nil {
		t.Fatal(err)
	}
}

type identityManager struct {
	conn    *dbus.Conn
	unitFor func(dbus.Sender) (string, error)
}

func (i identityManager) GetUnitByPIDFD(sender dbus.Sender, fd dbus.UnixFD) (dbus.ObjectPath, string, []byte, *dbus.Error) {
	defer syscall.Close(int(fd))
	info, err := os.ReadFile(fmt.Sprintf("/proc/self/fdinfo/%d", fd))
	if err != nil {
		return "", "", nil, dbus.MakeFailedError(err)
	}
	if !strings.Contains(string(info), "Pid:\t"+strconv.Itoa(os.Getpid())+"\n") {
		return "", "", nil, dbus.MakeFailedError(errors.New("wrong process fd"))
	}
	unit, err := i.unitFor(sender)
	if err != nil {
		return "", "", nil, dbus.MakeFailedError(err)
	}
	path := dbus.ObjectPath("/org/freedesktop/systemd1/unit/" + hex.EncodeToString([]byte(unit)))
	if err := i.conn.Export(identityProperties{unit}, path, "org.freedesktop.DBus.Properties"); err != nil {
		return "", "", nil, dbus.MakeFailedError(err)
	}
	return path, unit, make([]byte, 16), nil
}

type identityProperties struct{ unit string }

func (i identityProperties) Get(iface, name string) (dbus.Variant, *dbus.Error) {
	if iface != "org.freedesktop.systemd1.Unit" || name != "Id" {
		return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.UnknownProperty", nil)
	}
	return dbus.MakeVariant(i.unit), nil
}
