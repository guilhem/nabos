package devicetest

import (
	"os"
	"os/user"
	"strconv"
	"sync"
	"testing"

	"github.com/godbus/dbus/v5"
)

// UseCurrentUser configures real account expectations for private-bus fixtures.
// Every connection still authenticates using credentials supplied by the bus.
func UseCurrentUser(t *testing.T, environments ...string) string {
	t.Helper()
	account, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	for _, environment := range environments {
		t.Setenv(environment, account.Username)
	}
	return account.Username
}

func WrongUser(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"root", "nobody"} {
		account, err := user.Lookup(name)
		if err == nil && account.Uid != strconv.Itoa(os.Getuid()) {
			return name
		}
	}
	t.Fatal("wrong-account test requires a second Unix account")
	return ""
}

// CredentialReplies can delay a real bus reply or change name ownership before
// it is delivered. It never fabricates or changes the reply's Unix credentials.
func CredentialReplies(after func()) []dbus.ConnOption {
	var mu sync.Mutex
	pending := map[uint32]bool{}
	return []dbus.ConnOption{
		dbus.WithOutgoingInterceptor(func(message *dbus.Message) {
			if message.Type == dbus.TypeMethodCall &&
				message.Headers[dbus.FieldDestination].Value() == "org.freedesktop.DBus" &&
				message.Headers[dbus.FieldMember].Value() == "GetConnectionUnixUser" {
				mu.Lock()
				pending[message.Serial()] = true
				mu.Unlock()
			}
		}),
		dbus.WithIncomingInterceptor(func(message *dbus.Message) {
			if message.Type != dbus.TypeMethodReply && message.Type != dbus.TypeError {
				return
			}
			serial, ok := message.Headers[dbus.FieldReplySerial].Value().(uint32)
			if !ok {
				return
			}
			mu.Lock()
			credential := pending[serial]
			delete(pending, serial)
			mu.Unlock()
			if credential {
				after()
			}
		}),
	}
}
