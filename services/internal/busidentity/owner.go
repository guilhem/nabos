// Package busidentity verifies service owners using bus-supplied process FDs.
package busidentity

import (
	"context"
	"errors"
	"strings"
	"syscall"

	"github.com/godbus/dbus/v5"
)

var ErrStale = errors.New("service owner changed")

func Owner(ctx context.Context, conn *dbus.Conn, destination string) (string, error) {
	var owner string
	err := conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, destination).Store(&owner)
	return owner, err
}

func Authenticate(ctx context.Context, conn *dbus.Conn, destination, owner, expectedUnit string) error {
	if !strings.HasPrefix(owner, ":") {
		return errors.New("unique owner required")
	}
	var credentials map[string]dbus.Variant
	if err := conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetConnectionCredentials", 0, owner).Store(&credentials); err != nil {
		return err
	}
	v, ok := credentials["ProcessFD"]
	if !ok {
		return errors.New("service owner has no ProcessFD")
	}
	var fd dbus.UnixFD
	if err := v.Store(&fd); err != nil {
		return err
	}
	defer syscall.Close(int(fd))
	var unit dbus.ObjectPath
	var name string
	var invocation []byte
	if err := conn.Object("org.freedesktop.systemd1", "/org/freedesktop/systemd1").CallWithContext(ctx, "org.freedesktop.systemd1.Manager.GetUnitByPIDFD", 0, fd).Store(&unit, &name, &invocation); err != nil {
		return err
	}
	if name != expectedUnit {
		return errors.New("untrusted service unit")
	}
	var id dbus.Variant
	if err := conn.Object("org.freedesktop.systemd1", unit).CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, "org.freedesktop.systemd1.Unit", "Id").Store(&id); err != nil {
		return err
	}
	if id.Value() != name {
		return errors.New("service unit identity mismatch")
	}
	current, err := Owner(ctx, conn, destination)
	if err != nil {
		return err
	}
	if current != owner {
		return ErrStale
	}
	return nil
}
