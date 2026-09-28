package update

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	raucDest  = "de.pengutronix.rauc"
	raucIface = "de.pengutronix.rauc.Installer"
)

// RaucInstall installs a local bundle with RAUC (signature and compatibility
// are enforced by RAUC) and waits for completion.
func RaucInstall(ctx context.Context, path string, progress func(int)) error {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.AddMatchSignal(dbus.WithMatchInterface(raucIface), dbus.WithMatchMember("Completed")); err != nil {
		return err
	}
	signals := make(chan *dbus.Signal, 4)
	conn.Signal(signals)
	obj := conn.Object(raucDest, "/")
	if err := obj.CallWithContext(ctx, raucIface+".InstallBundle", 0, path, map[string]dbus.Variant{}).Err; err != nil {
		return fmt.Errorf("RAUC refused the bundle: %w", err)
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case s := <-signals:
			if s.Name != raucIface+".Completed" || len(s.Body) == 0 {
				continue
			}
			if code, _ := s.Body[0].(int32); code == 0 {
				return nil
			}
			v, _ := obj.GetProperty(raucIface + ".LastError")
			msg, _ := v.Value().(string)
			if msg == "" {
				msg = "installation failed"
			}
			return errors.New(msg)
		case <-tick.C:
			if v, err := obj.GetProperty(raucIface + ".Progress"); err == nil {
				if p, ok := v.Value().([]any); ok && len(p) > 0 {
					if pc, ok := p[0].(int32); ok {
						progress(int(pc))
					}
				}
			}
		}
	}
}

// SlotInfo returns RAUC's compatible string and booted slot for diagnostics.
func SlotInfo() (compatible, bootSlot string) {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return "", ""
	}
	defer conn.Close()
	obj := conn.Object(raucDest, "/")
	if v, err := obj.GetProperty(raucIface + ".Compatible"); err == nil {
		compatible, _ = v.Value().(string)
	}
	if v, err := obj.GetProperty(raucIface + ".BootSlot"); err == nil {
		bootSlot, _ = v.Value().(string)
	}
	return
}
