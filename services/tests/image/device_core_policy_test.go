package image

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

const maintenanceAgentInterface = "io.github.guilhem.DeviceCore1.Agent"
const maintenanceAgentPath = dbus.ObjectPath("/io/github/guilhem/DeviceCore1/Agent")

// Exercise the shipped policy, not the permissive session-bus policy used by
// application fixtures. Both connections use the test UID, like the two
// connections testing the device-core account policy; only the daemon owns a well-known name.
func TestDeviceCoreMaintenanceCallbackPolicy(t *testing.T) {
	policy := read(t, filepath.Join(rootfsDir, "etc/dbus-1/system.d/io.github.guilhem.DeviceCore1.conf"))
	for _, sameUser := range []bool{true, false} {
		name := "device-core-account"
		uid := os.Getuid()
		if !sameUser {
			name = "other-user"
			// Identity approximation without privileged subprocesses or host
			// accounts: map the service policy to a different numeric UID, so
			// the real caller is outside it on this second private bus.
			uid++
		}
		t.Run(name, func(t *testing.T) {
			address := maintenancePolicyBus(t, policy, uid)
			daemon := maintenancePolicyConnection(t, address)
			app := maintenancePolicyConnection(t, address)
			if sameUser {
				reply, err := daemon.RequestName("io.github.guilhem.DeviceCore1", dbus.NameFlagDoNotQueue)
				if err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
					t.Fatalf("daemon name ownership: reply %v, error %v", reply, err)
				}
			}
			target := app.Names()[0]
			if !strings.HasPrefix(target, ":") || target == daemon.Names()[0] || len(app.Names()) != 1 {
				t.Fatalf("application must have a distinct unique name only: %v", app.Names())
			}
			type invocation struct{ member, argument, sender string }
			calls := make(chan invocation, 16)
			table := map[string]interface{}{
				"Acquire": func(sender dbus.Sender, operation string) (string, *dbus.Error) {
					calls <- invocation{"Acquire", operation, string(sender)}
					return "maintenance-token", nil
				},
			}
			for _, member := range []string{"Abort", "Release", "Unrelated"} {
				table[member] = func(sender dbus.Sender, argument string) *dbus.Error {
					calls <- invocation{member, argument, string(sender)}
					return nil
				}
			}
			// Export the negative targets too: AccessDenied must come from the
			// bus policy rather than UnknownMethod/UnknownObject at the app.
			for _, target := range []struct {
				path  dbus.ObjectPath
				iface string
			}{
				{maintenanceAgentPath, maintenanceAgentInterface},
				{maintenanceAgentPath, "io.github.guilhem.DeviceCore1.Unrelated"},
				{maintenanceAgentPath + "/Unrelated", maintenanceAgentInterface},
			} {
				if err := app.ExportMethodTable(table, target.path, target.iface); err != nil {
					t.Fatal(err)
				}
			}
			for _, member := range []string{"Acquire", "Abort", "Release"} {
				for _, tc := range []struct {
					name, iface string
					path        dbus.ObjectPath
					allowed     bool
				}{
					{"callback", maintenanceAgentInterface, maintenanceAgentPath, sameUser},
					{"unrelated-interface", "io.github.guilhem.DeviceCore1.Unrelated", maintenanceAgentPath, false},
					{"unrelated-path", maintenanceAgentInterface, maintenanceAgentPath + "/Unrelated", false},
				} {
					t.Run(member+"/"+tc.name, func(t *testing.T) {
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cancel()
						argument := "update-operation"
						if member == "Release" {
							argument = "maintenance-token"
						}
						call := daemon.Object(target, tc.path).CallWithContext(ctx, tc.iface+"."+member, 0, argument)
						if !tc.allowed {
							maintenancePolicyDenied(t, call.Err)
							select {
							case got := <-calls:
								t.Fatalf("denied callback reached application: %+v", got)
							default:
							}
							return
						}
						if call.Err != nil {
							t.Fatalf("%s callback to application unique name %s: %v", member, target, call.Err)
						}
						if member == "Acquire" {
							var token string
							if err := call.Store(&token); err != nil || token != "maintenance-token" {
								t.Fatalf("Acquire reply: token %q, error %v", token, err)
							}
						}
						select {
						case got := <-calls:
							if got != (invocation{member, argument, daemon.Names()[0]}) {
								t.Fatalf("wrong callback delivered: %+v", got)
							}
						case <-ctx.Done():
							t.Fatal("callback succeeded without reaching application")
						}
					})
				}
			}
			t.Run("unrelated-member", func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				maintenancePolicyDenied(t, daemon.Object(target, maintenanceAgentPath).CallWithContext(ctx,
					maintenanceAgentInterface+".Unrelated", 0, "update-operation").Err)
			})
		})
	}
}

func maintenancePolicyDenied(t *testing.T, err error) {
	t.Helper()
	var denied dbus.Error
	if !errors.As(err, &denied) || denied.Name != "org.freedesktop.DBus.Error.AccessDenied" {
		t.Fatalf("want policy AccessDenied, got %v", err)
	}
}

func maintenancePolicyConnection(t *testing.T, address string) *dbus.Conn {
	t.Helper()
	conn, err := dbus.Connect(address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func maintenancePolicyBus(t *testing.T, production string, uid int) string {
	t.Helper()
	daemon := envOr("DBUS_DAEMON", "dbus-daemon")
	imageTools(t, daemon)
	tmp := t.TempDir()
	if !strings.Contains(production, `<policy user="device-core">`) {
		t.Fatal("production policy missing device-core service account")
	}
	write(t, filepath.Join(tmp, "production.conf"), strings.ReplaceAll(production,
		`<policy user="device-core">`, fmt.Sprintf(`<policy user="%d">`, uid)))
	// No host configuration is included. Hello and other bus control methods
	// are allowed explicitly; unsolicited method calls remain default-denied.
	// An isolated Linux abstract socket avoids Unix path-length limits when
	// TMPDIR points into a deeply nested image-test workspace.
	listen := fmt.Sprintf("unix:abstract=nabos-maintenance-policy-%x", sha256.Sum256([]byte(tmp)))
	write(t, filepath.Join(tmp, "bus.conf"), `<busconfig>
  <type>system</type>
  <listen>`+listen+`</listen>
  <auth>EXTERNAL</auth>
  <policy context="default">
    <allow user="*"/>
    <deny own="*"/>
    <deny send_type="method_call"/>
    <allow send_destination="org.freedesktop.DBus" send_interface="org.freedesktop.DBus"/>
    <allow send_type="method_return"/>
    <allow send_type="error"/>
    <allow receive_type="method_call"/>
    <allow receive_type="method_return"/>
    <allow receive_type="error"/>
  </policy>
  <include>production.conf</include>
</busconfig>`)
	log, err := os.Create(filepath.Join(tmp, "daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, daemon, "--nofork", "--nopidfile", "--print-address=1", "--config-file="+filepath.Join(tmp, "bus.conf"))
	cmd.Stderr = log
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); cmd.Wait() })
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(out).ReadString('\n')
		ready <- strings.TrimSpace(line)
	}()
	select {
	case address := <-ready:
		if address == "" {
			t.Fatalf("private dbus-daemon failed: %s", read(t, log.Name()))
		}
		return address
	case <-time.After(5 * time.Second):
		t.Fatalf("private dbus-daemon startup timed out: %s", read(t, log.Name()))
		return ""
	}
}
