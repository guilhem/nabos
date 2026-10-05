package devicetest

import (
	"context"
	"os"
	"testing"

	"github.com/guilhem/nabos/services/internal/busidentity"
	"github.com/guilhem/nabos/services/internal/device"
)

func TestUnixUserAdmission(t *testing.T) {
	fixture := New(t)
	ctx := context.Background()
	owner := fixture.Conn.Names()[0]
	expected := os.Getenv("NABOS_DEVICE_USER")
	if err := busidentity.Authenticate(ctx, fixture.Conn, device.Destination, owner, expected); err != nil {
		t.Fatal(err)
	}
	for _, account := range []string{WrongUser(t), "nabos-no-such-test-account", ""} {
		if busidentity.Authenticate(ctx, fixture.Conn, device.Destination, owner, account) == nil {
			t.Fatal("untrusted account admitted", account)
		}
	}
	if busidentity.Authenticate(ctx, fixture.Conn, device.Destination, device.Destination, expected) == nil {
		t.Fatal("well-known sender admitted")
	}
	fixture.Conn.Close()
	if busidentity.Authenticate(ctx, fixture.Conn, device.Destination, owner, expected) == nil {
		t.Fatal("disconnected owner admitted")
	}
}
