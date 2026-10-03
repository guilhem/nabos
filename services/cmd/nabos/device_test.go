package main

import (
	"context"
	"github.com/godbus/dbus/v5"
	"github.com/guilhem/nabos/services/internal/device"
	"github.com/guilhem/nabos/services/internal/devicetest"
	"github.com/guilhem/nabos/services/internal/rabbit"
	"testing"
	"time"
)

func TestMaintenanceAgentKeepsGateAndAuthenticatesDaemon(t *testing.T) {
	a := testApp(t)
	f := appFixture(t, a)
	devicetest.RequireProcessFD(t, f.Conn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := a.device.Call(ctx, "Manager", "RegisterAgent", device.Path("Agent")); err != nil {
		t.Fatal(err)
	}
	// An uninitialized engine cannot admit maintenance.
	if f.Agent(ctx, "Acquire", "updates-1").Err == nil {
		t.Fatal("maintenance admitted uninitialized engine")
	}
	outsider, err := dbus.ConnectSystemBus()
	if err != nil {
		t.Fatal(err)
	}
	defer outsider.Close()
	f.Mu.Lock()
	agentSender, agentPath := f.AgentSender, f.AgentPath
	f.Mu.Unlock()
	if outsider.Object(agentSender, agentPath).Call(device.Interface("Agent")+".Acquire", 0, "updates-1").Err == nil {
		t.Fatal("untrusted sender acquired maintenance")
	}

	native := startNative(t, a)
	if err := a.do(ctx, rabbit.Command{Action: rabbit.Info, InfoID: "maintenance-test", Animation: &rabbit.Animation{Tempo: 1, Frames: [][3]rabbit.RGB{{{1, 2, 3}, {}, {}}}}}, time.Second); err != nil {
		t.Fatal(err)
	}
	pynabWait(t, "idle animation active", time.Second, func() bool { native.mu.Lock(); defer native.mu.Unlock(); return native.leds > 3 })
	// A book's silence still owns this gate even when core state is idle.
	a.mediaGate <- struct{}{}
	if f.Agent(ctx, "Acquire", "updates-1").Err == nil {
		t.Fatal("maintenance slipped between book chapters")
	}
	<-a.mediaGate
	var token, renewed string
	if err := f.Agent(ctx, "Acquire", "updates-1").Store(&token); err != nil || token == "" {
		t.Fatal(token, err)
	}
	if err := f.Agent(ctx, "Acquire", "updates-1").Store(&renewed); err != nil || renewed != token {
		t.Fatal("Acquire not idempotent", err)
	}
	identity := deviceIdentities[a]
	identity.mu.Lock()
	identity.unit = "other.service"
	identity.mu.Unlock()
	for _, call := range []struct{ method, argument string }{{"Acquire", "updates-1"}, {"Abort", "updates-1"}, {"Release", token}} {
		if f.Agent(ctx, call.method, call.argument).Err == nil {
			t.Fatal("untrusted current owner admitted", call.method)
		}
	}
	if len(a.mediaGate) != 1 {
		t.Fatal("untrusted owner reopened maintenance")
	}
	identity.mu.Lock()
	identity.unit = "device-core.service"
	identity.mu.Unlock()

	native.mu.Lock()
	frozen := native.leds
	native.mu.Unlock()
	time.Sleep(40 * time.Millisecond)
	native.mu.Lock()
	after := native.leds
	native.mu.Unlock()
	if after != frozen {
		t.Fatal("engine background ran after maintenance acceptance", frozen, after)
	}

	wait, done := context.WithTimeout(ctx, 20*time.Millisecond)
	if err := a.acquireMedia(wait); err == nil {
		t.Fatal("maintenance did not retain media gate")
	}
	done()
	f.Mu.Lock()
	f.UpdateState.State = "installing"
	f.Mu.Unlock()
	if f.Agent(ctx, "Release", token).Err == nil {
		t.Fatal("released active installation")
	}
	f.Mu.Lock()
	f.UpdateState.State = "reboot"
	f.Maintenance = true
	f.Mu.Unlock()
	if err := f.Agent(ctx, "Release", token).Err; err != nil {
		t.Fatal("terminal release", err)
	}
	if err := f.Agent(ctx, "Release", token).Err; err != nil {
		t.Fatal("lost reply retry", err)
	}
	if len(a.mediaGate) != 1 || a.rabbit.Ready() {
		t.Fatal("release resumed before all daemon agents")
	}
	f.Mu.Lock()
	f.Maintenance = false
	f.Mu.Unlock()
	identity.mu.Lock()
	identity.id = "other.service"
	identity.mu.Unlock()
	a.agent.recover(ctx)
	if len(a.mediaGate) != 1 || a.rabbit.Ready() {
		t.Fatal("untrusted daemon resumed recovery")
	}
	identity.mu.Lock()
	identity.id = "device-core.service"
	identity.mu.Unlock()
	a.agent.recover(ctx)
	if len(a.mediaGate) != 0 {
		t.Fatal("safe daemon retained media gate")
	}
	if err := f.Agent(ctx, "Acquire", "updates-2").Store(&token); err != nil {
		t.Fatal(err)
	}
	if f.Agent(ctx, "Abort", "wrong-operation").Err == nil {
		t.Fatal("Abort released another operation")
	}
	if err := f.Agent(ctx, "Abort", "updates-2").Err; err != nil {
		t.Fatal(err)
	}
	if err := f.Agent(ctx, "Abort", "updates-2").Err; err != nil {
		t.Fatal("Abort retry", err)
	}
	a.agent.recover(ctx)

	// A restarted client queries updater state before reopening its local gate.
	a.agent.holdRecovery()
	f.Mu.Lock()
	f.UpdateState.State = "uncertain"
	f.Mu.Unlock()
	a.agent.recover(ctx)
	if len(a.mediaGate) != 1 {
		t.Fatal("unknown recovery released gate")
	}
	f.Mu.Lock()
	f.UpdateState.State = "unsupported"
	f.ManagerReady = false
	f.Mu.Unlock()
	a.agent.recover(ctx)
	if len(a.mediaGate) != 1 {
		t.Fatal("released before manager ready")
	}
	f.Mu.Lock()
	f.ManagerReady, f.Maintenance = true, true
	f.Mu.Unlock()
	a.agent.recover(ctx)
	if len(a.mediaGate) != 1 {
		t.Fatal("released during daemon maintenance")
	}
	f.Mu.Lock()
	f.Maintenance = false
	f.Mu.Unlock()
	a.agent.recover(ctx)
	if len(a.mediaGate) != 0 {
		t.Fatal("optional absent updater stranded product services")
	}

}

func TestDeviceLoopRegistersOnlyWhenRequired(t *testing.T) {
	for _, tc := range []struct {
		name             string
		required, denied bool
	}{
		{"standalone", false, true},
		{"required", true, false},
		{"registration denied still observes manager", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testApp(t)
			startNative(t, a)
			f := appFixture(t, a)
			f.Mu.Lock()
			f.MaintenanceAgents, f.RejectAgent = tc.required, tc.denied
			f.UpdateState.State = "unsupported"
			f.Mu.Unlock()
			a.agent.holdRecovery()
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); a.deviceLoop(ctx) }()
			t.Cleanup(func() { cancel(); <-done })
			pynabWait(t, "manager recovery", 2*time.Second, func() bool { return len(a.mediaGate) == 0 })
			f.Mu.Lock()
			defer f.Mu.Unlock()
			if tc.required && f.AgentRegistrations == 0 || !tc.required && f.AgentRegistrations != 0 {
				t.Fatal("registration did not follow capabilities", f.AgentRegistrations)
			}
			if tc.required && !tc.denied && f.AgentSender == "" {
				t.Fatal("required agent not registered")
			}
		})
	}
}

func TestVoiceSignalsRelayOnlyFromDaemon(t *testing.T) {
	a := testApp(t)
	f := appFixture(t, a)
	devicetest.RequireProcessFD(t, f.Conn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); a.deviceLoop(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	pynabWait(t, "agent registered", time.Second, func() bool { f.Mu.Lock(); defer f.Mu.Unlock(); return f.AgentSender != "" })
	if err := f.Conn.Emit(device.Path("Voice"), device.Interface("Voice")+".Event", "listening", `{}`); err != nil {
		t.Fatal(err)
	}
	pynabWait(t, "listening LEDs", time.Second, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.indicator == "listening" })
	outsider, err := dbus.ConnectSystemBus()
	if err != nil {
		t.Fatal(err)
	}
	defer outsider.Close()
	if err := outsider.Emit(device.Path("Voice"), device.Interface("Voice")+".Event", "tts_speaking", `{}`); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	a.mu.Lock()
	indicator := a.indicator
	a.mu.Unlock()
	if indicator != "listening" {
		t.Fatal("untrusted voice signal changed LEDs", indicator)
	}
	identity := deviceIdentities[a]
	identity.mu.Lock()
	identity.unit = "other.service"
	identity.mu.Unlock()
	if err := f.Conn.Emit(device.Path("Voice"), device.Interface("Voice")+".Event", "tts_speaking", `{}`); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	a.mu.Lock()
	indicator = a.indicator
	a.mu.Unlock()
	if indicator != "listening" {
		t.Fatal("voice from untrusted current owner changed LEDs", indicator)
	}
	identity.mu.Lock()
	identity.unit = "device-core.service"
	identity.mu.Unlock()

	if err := f.Conn.Emit(device.Path("Voice"), device.Interface("Voice")+".Event", "status", `{"status":"disabled"}`); err != nil {
		t.Fatal(err)
	}
	pynabWait(t, "disabled LEDs", time.Second, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.indicator == "" })
}

func TestMaintenanceRecoveryAfterDaemonLoss(t *testing.T) {
	a := testApp(t)
	startNative(t, a)
	f := appFixture(t, a)
	ctx, cancel := context.WithCancel(a.ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); a.deviceLoop(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	pynabWait(t, "registered safe daemon", time.Second, func() bool {
		f.Mu.Lock()
		registered := f.AgentSender != ""
		f.Mu.Unlock()
		return registered && len(a.mediaGate) == 0 && a.rabbit.Ready()
	})
	var token string
	if err := f.Agent(ctx, "Acquire", "lost-operation").Store(&token); err != nil {
		t.Fatal(err)
	}
	f.Mu.Lock()
	f.ManagerReady = false
	f.Mu.Unlock()
	if _, err := f.Conn.ReleaseName(device.Destination); err != nil {
		t.Fatal(err)
	}
	pynabWait(t, "daemon loss blocked app", time.Second, func() bool {
		a.agent.mu.Lock()
		defer a.agent.mu.Unlock()
		return a.agent.recovering && a.agent.token == ""
	})
	if len(a.mediaGate) != 1 || a.rabbit.Ready() {
		t.Fatal("daemon loss reopened product")
	}
	if _, err := f.Conn.RequestName(device.Destination, dbus.NameFlagDoNotQueue); err != nil {
		t.Fatal(err)
	}
	if f.Agent(ctx, "Release", token).Err == nil {
		t.Fatal("stale token reopened product")
	}
	pynabWait(t, "daemon registered after name recovery", time.Second, func() bool {
		f.Mu.Lock()
		defer f.Mu.Unlock()
		return f.AgentRegistrations >= 2
	})
	for _, state := range []string{"uncertain", "installing", "downloading"} {
		f.Mu.Lock()
		f.ManagerReady = true
		f.UpdateState.State = state
		f.Mu.Unlock()
		a.agent.recover(ctx)
		if len(a.mediaGate) != 1 || a.rabbit.Ready() {
			t.Fatal("unsafe updater reopened product", state)
		}
	}
	f.Mu.Lock()
	f.UpdateState.State = "idle"
	f.Mu.Unlock()
	a.agent.recover(ctx)
	pynabWait(t, "safe daemon recovery", time.Second, func() bool { return len(a.mediaGate) == 0 && a.rabbit.Ready() })
	if err := a.media(ctx, sequence("system/abort.wav", ""), time.Second); err != nil {
		t.Fatal("product did not resume", err)
	}
}
