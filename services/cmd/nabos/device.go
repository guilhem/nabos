package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/guilhem/nabos/services/internal/device"
	"github.com/guilhem/nabos/services/internal/rabbit"
)

func (a *App) clockNow() time.Time { _, _, now := a.clockSnapshot(); return now }

// Acquire holds the product media gate across the complete maintenance
// operation, including silence between chapters. Generic audio/voice use the
// daemon's own gate; the parent validates this agent's Unix account.
type maintenanceAgent struct {
	app                                             *App
	mu                                              sync.Mutex
	held                                            bool
	recovering                                      bool
	token, operation, owner                         string
	releasedToken, releasedOwner, releasedOperation string
}

func (m *maintenanceAgent) daemon(sender dbus.Sender) bool {
	ctx, cancel := context.WithTimeout(context.Background(), device.Timeout)
	defer cancel()
	return m.app.device.Authenticate(ctx, string(sender)) == nil
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
	state, online := m.app.rabbit.State()
	m.app.mu.Lock()
	busy := m.app.interaction != nil || m.app.radioCancel != nil || m.app.mediaCancel != nil
	m.app.mu.Unlock()
	if !online || !m.app.rabbit.Ready() || !state.HardwareReady() || busy || state.Playing != nil || state.State != "idle" && state.State != "asleep" {
		if acquired {
			<-m.app.mediaGate
		}
		return "", dbus.MakeFailedError(errors.New("rabbit busy or hardware offline"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), device.Timeout)
	defer cancel()
	// Holding the app gate alone does not stop idle animations or queued ears.
	// Keep both gates closed if quiescence cannot be confirmed.
	m.held, m.operation, m.owner = true, operation, string(sender)
	if err := m.app.rabbit.SetMaintenance(ctx, true); err != nil {
		m.recovering = true
		return "", dbus.MakeFailedError(err)
	}
	if !m.daemon(sender) {
		m.recovering = true
		return "", dbus.NewError("org.freedesktop.DBus.Error.AccessDenied", nil)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		m.recovering = true
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
	if m.token == "" && token != "" && token == m.releasedToken && m.releasedOwner == string(sender) {
		return nil
	}
	if !m.held || token == "" || token != m.token || m.owner != string(sender) {
		return dbus.NewError("org.freedesktop.DBus.Error.AccessDenied", nil)
	}
	status, err := m.app.device.ForOwner(string(sender)).UpdateStatus(context.Background())
	if err != nil || !maintenanceSafe(status.State) || !m.daemon(sender) {
		return dbus.MakeFailedError(errors.New("updater recovery pending"))
	}
	m.releaseReservation()
	return nil
}

// Release acknowledges this agent only. Both agents stay paused until the
// coordinator has released every reservation and reopened its admission gate.
func (m *maintenanceAgent) releaseReservation() {
	m.releasedToken, m.releasedOwner, m.releasedOperation = m.token, m.owner, m.operation
	m.token, m.owner, m.operation = "", "", ""
	m.recovering = true
	kick(m.app.haKick)
}

func (m *maintenanceAgent) resume(owner string) *dbus.Error {
	// Keep the engine paused while authentication itself waits or fails.
	if !m.daemon(dbus.Sender(owner)) {
		return dbus.NewError("org.freedesktop.DBus.Error.AccessDenied", nil)
	}
	ctx, cancel := context.WithTimeout(context.Background(), device.Timeout)
	defer cancel()
	if err := m.app.rabbit.SetMaintenance(ctx, false); err != nil {
		return dbus.MakeFailedError(errors.Join(err, m.reclose()))
	}
	if !m.daemon(dbus.Sender(owner)) {
		if err := m.reclose(); err != nil {
			return dbus.MakeFailedError(fmt.Errorf("maintenance reclosure pending: %w", err))
		}
		return dbus.NewError("org.freedesktop.DBus.Error.AccessDenied", nil)
	}
	<-m.app.mediaGate
	m.held, m.recovering, m.token, m.operation, m.owner = false, false, "", "", ""
	kick(m.app.clockKick)
	go m.app.restoreRabbit()
	return nil
}

// Authentication or an ambiguous engine reply may consume its whole deadline.
// Reclosure gets a fresh budget; the reservation stays held until recovery.
func (m *maintenanceAgent) reclose() error {
	m.recovering = true
	ctx, cancel := context.WithTimeout(context.Background(), device.Timeout)
	defer cancel()
	return m.app.rabbit.SetMaintenance(ctx, true)
}

// Abort is the daemon's rollback for an Acquire whose reply was lost. It is
// called before acceptance; the coordinator owns the RAUC safety decision.
func (m *maintenanceAgent) Abort(sender dbus.Sender, operation string) *dbus.Error {
	if operation == "" || !m.daemon(sender) {
		return dbus.NewError("org.freedesktop.DBus.Error.AccessDenied", nil)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.held || m.token == "" && m.releasedOperation == operation && m.releasedOwner == string(sender) {
		return nil
	}
	if m.owner != string(sender) || m.operation != operation {
		return dbus.NewError("org.freedesktop.DBus.Error.AccessDenied", nil)
	}
	status, err := m.app.device.ForOwner(string(sender)).UpdateStatus(context.Background())
	if err != nil || !maintenanceSafe(status.State) || !m.daemon(sender) {
		return dbus.MakeFailedError(errors.New("updater recovery pending"))
	}
	m.releaseReservation()
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
	m.app.stopMedia()
	ctx, cancel := context.WithTimeout(context.Background(), device.Timeout)
	defer cancel()
	// SetMaintenance pauses the engine even while an app sequence is draining.
	_ = m.app.rabbit.SetMaintenance(ctx, true)
	m.token, m.operation, m.owner = "", "", ""
	if !m.held {
		select {
		case m.app.mediaGate <- struct{}{}:
			m.held = true
		default:
		}
	}
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
	owner, err := m.app.device.TrustedOwner(ctx)
	if err != nil {
		return
	}
	daemon := m.app.device.ForOwner(owner)
	var ready, maintenance bool
	if daemon.Property(ctx, "Manager", "Ready", &ready) != nil || !ready || daemon.Property(ctx, "Manager", "Maintenance", &maintenance) != nil || maintenance {
		return
	}
	status, err := daemon.UpdateStatus(ctx)
	safe := err == nil && maintenanceSafe(status.State)
	if err != nil {
		var capabilities []string
		if daemon.Property(ctx, "Manager", "Capabilities", &capabilities) == nil {
			safe = !slices.Contains(capabilities, "updates")
		}
	}
	if safe && m.daemon(dbus.Sender(owner)) {
		_ = m.resume(owner)
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
	lastState := rabbit.State{}
	refresh := func() {
		callCtx, cancel := context.WithTimeout(ctx, device.Timeout)
		defer cancel()
		owner, err := a.device.TrustedOwner(callCtx)
		if err != nil {
			a.agent.holdRecovery()
			registered, observed = "", ""
			a.setIndicator("")
			return
		}
		daemon := a.device.ForOwner(owner)
		if owner != observed {
			a.agent.holdRecovery()
			observed = owner
			a.setIndicator("")
			if state, err := daemon.VoiceState(ctx); err == nil && a.agent.daemon(dbus.Sender(owner)) {
				a.voiceEvent("status", map[string]any{"status": state})
			}
		}
		if owner != registered {
			var capabilities []string
			if daemon.Property(ctx, "Manager", "Capabilities", &capabilities) == nil {
				if slices.Contains(capabilities, "maintenance-agents") {
					if _, err := daemon.Call(ctx, "Manager", "RegisterAgent", device.Path("Agent")); err == nil {
						if a.agent.daemon(dbus.Sender(owner)) {
							registered = owner
						}
					}
				} else {
					if a.agent.daemon(dbus.Sender(owner)) {
						registered = owner
					}
				}
			}
		}
		a.agent.recover(ctx)
		_, settings, err := daemon.ReadConfig(callCtx)
		if err == nil {
			err = a.device.Authenticate(callCtx, owner)
		}
		if err == nil {
			changed := settings != lastSettings
			if changed {
				a.applySettings(settings)
				kick(a.clockKick)
			}
			state, _ := a.rabbit.State()
			if changed || state.State != lastState.State || state.Ears != lastState.Ears {
				a.ha.State(state.State, int(settings.Volume), state.Ears.Left, state.Ears.Right)
			}
			lastSettings, lastState = settings, state
		}
	}
	refresh()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
		case <-a.haKick:
			refresh()
		case signal := <-signals:
			if signal == nil {
				continue
			}
			if signal.Name == "org.freedesktop.DBus.NameOwnerChanged" {
				// Resolve the current owner in refresh. Buffered notifications for
				// an already recovered owner must not cancel new product media.
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
