package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
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

// BootHealthFile holds the health verdict of this boot ("good A",
// "stranded B"), written by root's health check in /run.
var BootHealthFile = "/run/nabos-boot-health"

// RaucProbe reads RAUC's operation and booted slot, and the health verdict
// for that slot.
func RaucProbe() (BootState, error) {
	id, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return BootState{}, err
	}
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return BootState{}, err
	}
	defer conn.Close()
	s := BootState{BootID: strings.TrimSpace(string(id))}
	obj := conn.Object(raucDest, "/")
	for prop, dst := range map[string]*string{"Operation": &s.Operation, "BootSlot": &s.Slot} {
		v, err := obj.GetProperty(raucIface + "." + prop)
		if err != nil {
			return BootState{}, err
		}
		if *dst, _ = v.Value().(string); *dst == "" {
			return BootState{}, fmt.Errorf("RAUC %s unknown", prop)
		}
	}
	marker, _ := os.ReadFile(BootHealthFile)
	s.Health = markerHealth(string(marker), s.Slot)
	return s, nil
}

// markerHealth returns the verdict of a health marker for slot, "" when it
// concerns another slot or is malformed.
func markerHealth(marker, slot string) string {
	f := strings.Fields(marker)
	if len(f) == 2 && f[1] == slot && (f[0] == "good" || f[0] == "stranded") {
		return f[0]
	}
	return ""
}
