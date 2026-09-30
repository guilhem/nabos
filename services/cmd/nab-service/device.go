package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/guilhem/nabos/services/internal/device"
)

func (a *App) clockNow() time.Time { _, _, now := a.clockSnapshot(); return now }

// Acquire holds the product media gate across the complete maintenance
// operation, including silence between chapters. Generic audio/voice use the
// daemon's own gate; the parent validates this agent's systemd unit.
type maintenanceAgent struct {
	app                          *App
	mu                           sync.Mutex
	held                         bool
	recovering                   bool
	token, operation, owner      string
	releasedToken, releasedOwner string
}

func (m *maintenanceAgent) daemon(sender dbus.Sender) bool {
	ctx, cancel := context.WithTimeout(context.Background(), device.Timeout)
	defer cancel()
	var owner string
	return m.app.device.Conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, device.Destination).Store(&owner) == nil && string(sender) == owner
}
func (m *maintenanceAgent) Acquire(sender dbus.Sender, operation string) (string, *dbus.Error) {
	if operation == "" || !m.daemon(sender) {
		return "", dbus.NewError("org.freedesktop.DBus.Error.AccessDenied", nil)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.held && m.token != "" {
		if m.operation == operation && m.owner == string(sender) {
			return m.token, nil
		}
		return "", dbus.MakeFailedError(errors.New("maintenance already held"))
	}
	acquired := false
	if !m.held {
		select {
		case m.app.mediaGate <- struct{}{}:
			acquired = true
		default:
			return "", dbus.MakeFailedError(errors.New("media busy"))
		}
	}
	connected, online := m.app.bus.Healthy()
	state, _ := m.app.bus.State()
	m.app.mu.Lock()
	busy := m.app.interaction != nil || m.app.radioCancel != nil || m.app.mediaCancel != nil
	m.app.mu.Unlock()
	if !connected || !online || busy || state.Playing != nil || state.State != "idle" && state.State != "asleep" {
		if acquired {
			<-m.app.mediaGate
		}
		return "", dbus.MakeFailedError(errors.New("rabbit busy or MQTT offline"))
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		if acquired {
			<-m.app.mediaGate
		}
		return "", dbus.MakeFailedError(err)
	}
	m.held, m.token, m.operation, m.owner = true, hex.EncodeToString(raw), operation, string(sender)
	return m.token, nil
}
func (m *maintenanceAgent) Release(sender dbus.Sender, token string) *dbus.Error {
	if !m.daemon(sender) {
		return dbus.NewError("org.freedesktop.DBus.Error.AccessDenied", nil)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.held && token != "" && token == m.releasedToken && m.releasedOwner == string(sender) {
		return nil
	}
	if !m.held || token == "" || token != m.token || m.owner != string(sender) {
		return dbus.NewError("org.freedesktop.DBus.Error.AccessDenied", nil)
	}
	status, err := m.app.device.UpdateStatus(context.Background())
	if err != nil || status.State == "installing" || status.State == "downloading" {
		return dbus.MakeFailedError(errors.New("updater recovery pending"))
	}
	m.release()
	return nil
}
func (m *maintenanceAgent) release() {
	m.releasedToken, m.releasedOwner = m.token, m.owner
	<-m.app.mediaGate
	m.held, m.recovering, m.token, m.operation, m.owner = false, false, "", "", ""
}

// Abort is the daemon's rollback for an Acquire whose reply was lost. It is
// called before acceptance; the coordinator owns the RAUC safety decision.
func (m *maintenanceAgent) Abort(sender dbus.Sender, operation string) *dbus.Error {
	if operation == "" || !m.daemon(sender) {
		return dbus.NewError("org.freedesktop.DBus.Error.AccessDenied", nil)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.held {
		return nil
	}
	if m.owner != string(sender) || m.operation != operation {
		return dbus.NewError("org.freedesktop.DBus.Error.AccessDenied", nil)
	}
	m.release()
	return nil
}
func maintenanceSafe(state string) bool {
	switch state {
	case "idle", "error", "reboot", "confirming", "unsupported":
		return true
	}
	return false
}
func (m *maintenanceAgent) holdRecovery() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recovering = true
	if !m.held {
		select {
		case m.app.mediaGate <- struct{}{}:
			m.held = true
		default:
			return // the active media drains; retry before releasing recovery
		}
	}
	m.token, m.operation, m.owner = "", "", ""
}
func (m *maintenanceAgent) recover(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.token != "" || !m.recovering {
		return
	}
	if !m.held {
		select {
		case m.app.mediaGate <- struct{}{}:
			m.held = true
		default:
			return
		}
	}
	var ready, maintenance bool
	if m.app.device.Property(ctx, "Manager", "Ready", &ready) != nil || !ready || m.app.device.Property(ctx, "Manager", "Maintenance", &maintenance) != nil || maintenance {
		return
	}
	status, err := m.app.device.UpdateStatus(ctx)
	safe := err == nil && maintenanceSafe(status.State)
	if err != nil {
		var capabilities []string
		if m.app.device.Property(ctx, "Manager", "Capabilities", &capabilities) == nil {
			safe = !slices.Contains(capabilities, "updates")
		}
	}
	if safe {
		<-m.app.mediaGate
		m.held, m.recovering = false, false
	}
}

func (a *App) deviceLoop(ctx context.Context) {
	signals := make(chan *dbus.Signal, 64)
	a.device.Conn.Signal(signals)
	defer a.device.Conn.RemoveSignal(signals)
	voiceMatch := []dbus.MatchOption{dbus.WithMatchSender(device.Destination), dbus.WithMatchInterface(device.Interface("Voice")), dbus.WithMatchObjectPath(device.Path("Voice")), dbus.WithMatchMember("Event")}
	ownerMatch := []dbus.MatchOption{dbus.WithMatchSender("org.freedesktop.DBus"), dbus.WithMatchInterface("org.freedesktop.DBus"), dbus.WithMatchMember("NameOwnerChanged"), dbus.WithMatchArg(0, device.Destination)}
	if a.device.Conn.AddMatchSignal(voiceMatch...) != nil || a.device.Conn.AddMatchSignal(ownerMatch...) != nil {
		return
	}
	defer a.device.Conn.RemoveMatchSignal(voiceMatch...)
	defer a.device.Conn.RemoveMatchSignal(ownerMatch...)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	registered, observed := "", ""
	lastSettings := device.Settings{}
	refresh := func() {
		var owner string
		callCtx, cancel := context.WithTimeout(ctx, device.Timeout)
		defer cancel()
		if a.device.Conn.BusObject().CallWithContext(callCtx, "org.freedesktop.DBus.GetNameOwner", 0, device.Destination).Store(&owner) != nil {
			a.agent.holdRecovery()
			registered, observed = "", ""
			a.setIndicator("")
			return
		}
		if owner != observed {
			a.agent.holdRecovery()
			observed = owner
			if state, err := a.device.VoiceState(ctx); err == nil {
				a.voiceEvent("status", map[string]any{"status": state})
			}
		}
		if owner != registered {
			var capabilities []string
			if a.device.Property(ctx, "Manager", "Capabilities", &capabilities) == nil {
				if slices.Contains(capabilities, "maintenance-agents") {
					if _, err := a.device.Call(ctx, "Manager", "RegisterAgent", device.Path("Agent")); err == nil {
						registered = owner
					}
				} else {
					registered = owner
				}
			}
		}
		a.agent.recover(ctx)
		settings, err := a.systemSettings(ctx)
		if err == nil && settings != lastSettings {
			lastSettings = settings
			a.publishSettings(ctx)
			state, _ := a.bus.State()
			a.ha.State(state.State, int(settings.Volume), state.Ears.Left, state.Ears.Right)
			kick(a.clockKick)
		}
	}
	refresh()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
		case signal := <-signals:
			if signal == nil {
				continue
			}
			if signal.Name == "org.freedesktop.DBus.NameOwnerChanged" {
				a.agent.holdRecovery()
				registered, observed = "", ""
				a.setIndicator("")
				refresh()
			} else if signal.Name == device.Interface("Voice")+".Event" && a.agent.daemon(dbus.Sender(signal.Sender)) {
				var event, raw string
				if dbus.Store(signal.Body, &event, &raw) != nil {
					continue
				}
				var data map[string]any
				if json.Unmarshal([]byte(raw), &data) != nil {
					continue
				}
				a.voiceEvent(event, data)
			}
		}
	}
}
