package integration

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/prop"
)

// Only process-to-unit identity is supplied by this fixture. The real daemon
// still checks bus credentials and authorizes each maintenance agent itself.
type systemdIdentity struct{ harness *harness }

func unitPath(unit string) dbus.ObjectPath {
	if unit == "nab-core.service" {
		return "/org/freedesktop/systemd1/unit/nab_2dcore_2eservice"
	}
	return "/org/freedesktop/systemd1/unit/nab_2dservice_2eservice"
}

func (s systemdIdentity) GetUnitByPIDFD(fd dbus.UnixFD) (dbus.ObjectPath, string, []byte, *dbus.Error) {
	defer syscall.Close(int(fd))
	info, err := os.ReadFile(fmt.Sprintf("/proc/self/fdinfo/%d", fd))
	if err != nil {
		return "", "", nil, dbus.MakeFailedError(err)
	}
	var pid uint64
	for _, line := range strings.Split(string(info), "\n") {
		if value, ok := strings.CutPrefix(line, "Pid:\t"); ok {
			pid, err = strconv.ParseUint(value, 10, 32)
			break
		}
	}
	if err != nil || pid == 0 {
		return "", "", nil, dbus.NewError("org.freedesktop.systemd1.NoUnitForPID", nil)
	}
	s.harness.mu.Lock()
	unit := s.harness.units[uint32(pid)]
	s.harness.mu.Unlock()
	if unit == "" {
		return "", "", nil, dbus.NewError("org.freedesktop.systemd1.NoUnitForPID", nil)
	}
	return unitPath(unit), unit, make([]byte, 16), nil
}

func (h *harness) startSystemdIdentity(t *testing.T) {
	t.Helper()
	h.units = make(map[uint32]string)
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.Export(systemdIdentity{h}, "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager"); err != nil {
		t.Fatal(err)
	}
	for _, unit := range []string{"nab-core.service", "nab-service.service"} {
		if _, err := prop.Export(conn, unitPath(unit), prop.Map{
			"org.freedesktop.systemd1.Unit": {"Id": {Value: unit}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if reply, err := conn.RequestName("org.freedesktop.systemd1", dbus.NameFlagDoNotQueue); err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("systemd identity ownership: %v %v", reply, err)
	}
}
