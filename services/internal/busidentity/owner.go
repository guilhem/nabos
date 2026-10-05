// Package busidentity verifies service owners using bus-supplied Unix credentials.
package busidentity

import (
	"context"
	"errors"
	"os"
	"os/user"
	"strconv"
	"strings"

	"github.com/godbus/dbus/v5"
)

var ErrStale = errors.New("service owner changed")

func Owner(ctx context.Context, conn *dbus.Conn, destination string) (string, error) {
	var owner string
	err := conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, destination).Store(&owner)
	return owner, err
}

func ExpectedUser(environment, fallback string) string {
	if name, ok := os.LookupEnv(environment); ok {
		return name
	}
	return fallback
}

func Authenticate(ctx context.Context, conn *dbus.Conn, destination, owner, expectedUser string) error {
	if !strings.HasPrefix(owner, ":") {
		return errors.New("unique owner required")
	}
	account, err := user.Lookup(expectedUser)
	if err != nil {
		return err
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		return err
	}
	var actual uint32
	if err := conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetConnectionUnixUser", 0, owner).Store(&actual); err != nil {
		return err
	}
	if actual != uint32(uid) {
		return errors.New("untrusted service user")
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
