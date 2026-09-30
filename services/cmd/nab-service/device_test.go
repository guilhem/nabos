package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/guilhem/nabos/services/internal/bus"
	"github.com/guilhem/nabos/services/internal/device"
)

func TestMaintenanceAgentKeepsGateAndAuthenticatesDaemon(t *testing.T) {
	a := testApp(t)
	f := appFixture(t, a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := a.device.Call(ctx, "Manager", "RegisterAgent", device.Path("Agent")); err != nil {
		t.Fatal(err)
	}
	// Before MQTT is online, even the authenticated daemon cannot acquire.
	if f.Agent(ctx, "Acquire", "updates-1").Err == nil {
		t.Fatal("maintenance admitted offline MQTT")
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

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "mosquitto.conf")
	os.WriteFile(configPath, fmt.Appendf(nil, "listener %d 127.0.0.1\nallow_anonymous true\npersistence false\n", port), 0600)
	broker := exec.Command(pynabTool(t, "MOSQUITTO", "mosquitto", "../../../build/tools/root/usr/sbin/mosquitto"), "-c", configPath)
	if err := broker.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { broker.Process.Kill(); broker.Wait() })
	a.bus = bus.New("127.0.0.1", port, "agent-check", bus.Handlers{OnState: a.onState})
	if err := a.bus.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		stop, done := context.WithTimeout(context.Background(), time.Second)
		defer done()
		a.bus.Stop(stop)
	})
	pub := pynabTool(t, "MOSQUITTO_PUB", "mosquitto_pub", "../../../build/tools/root/usr/bin/mosquitto_pub")
	publish := func(topic, body string) {
		t.Helper()
		cmd := exec.Command(pub, "-h", "127.0.0.1", "-p", strconv.Itoa(port), "-q", "1", "-r", "-t", topic, "-m", body)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("publish: %v %s", err, out)
		}
	}
	pynabWait(t, "MQTT connected", 5*time.Second, func() bool { connected, _ := a.bus.Healthy(); return connected })
	publish(bus.TopicState, `{"v":1,"state":"idle","ears":{"left":0,"right":0}}`)
	publish(bus.TopicCoreAvail, "online")
	pynabWait(t, "core online", 5*time.Second, func() bool {
		connected, online := a.bus.Healthy()
		state, _ := a.bus.State()
		return connected && online && state.State == "idle"
	})
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
	f.Mu.Unlock()
	if err := f.Agent(ctx, "Release", token).Err; err != nil {
		t.Fatal("terminal release", err)
	}
	if err := f.Agent(ctx, "Release", token).Err; err != nil {
		t.Fatal("lost reply retry", err)
	}
	if len(a.mediaGate) != 0 {
		t.Fatal("release retained media gate")
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
	t.Run("HA state stays current after a delayed device read", func(t *testing.T) {
		cfg := a.store.Get().HomeAssistant
		cfg.Enabled, cfg.Host, cfg.Port = true, "127.0.0.1", port
		if err := a.ha.Start(cfg); err != nil {
			t.Fatal(err)
		}
		defer a.ha.Stop()
		pynabWait(t, "HA broker connected", 5*time.Second, a.ha.Connected)
		revision, settings, err := a.device.ReadConfig(ctx)
		if err != nil {
			t.Fatal(err)
		}
		entered, resume, overlap := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
		var blockNext, inFlight atomic.Bool
		blockNext.Store(true)
		read := func() (string, device.Settings, *dbus.Error) {
			if blockNext.CompareAndSwap(true, false) {
				inFlight.Store(true)
				close(entered)
				<-resume
				inFlight.Store(false)
			} else if inFlight.Load() {
				select {
				case overlap <- struct{}{}:
				default:
				}
			}
			return revision, settings, nil
		}
		if err := f.Conn.ExportMethodTable(map[string]interface{}{"Read": read}, device.Path("Config"), device.Interface("Config")); err != nil {
			t.Fatal(err)
		}
		worker, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); a.deviceLoop(worker) }()
		released := false
		defer func() {
			if !released {
				close(resume)
			}
			stop()
			<-done
		}()
		publish(bus.TopicState, `{"v":1,"state":"playing","ears":{"left":1,"right":2}}`)
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("device read did not start")
		}
		publish(bus.TopicState, `{"v":1,"state":"idle","ears":{"left":4,"right":5}}`)
		pynabWait(t, "new core state received", time.Second, func() bool { state, _ := a.bus.State(); return state.State == "idle" && state.Ears.Left == 4 })
		select {
		case <-overlap:
			t.Fatal("state forwarding started concurrent device reads")
		case <-time.After(250 * time.Millisecond):
		}
		close(resume)
		released = true
		sub := pynabTool(t, "MOSQUITTO_SUB", "mosquitto_sub", "../../../build/tools/root/usr/bin/mosquitto_sub")
		out, err := exec.Command(sub, "-h", "127.0.0.1", "-p", strconv.Itoa(port), "-q", "1", "-t", "nabos/"+a.ha.Node+"/state", "-C", "1", "-W", "5").CombinedOutput()
		var payload struct {
			State  string `json:"state"`
			Volume int    `json:"volume"`
			Left   int    `json:"left_ear"`
			Right  int    `json:"right_ear"`
		}
		if err != nil || json.Unmarshal(out, &payload) != nil || payload.State != "idle" || payload.Volume != int(settings.Volume) || payload.Left != 4 || payload.Right != 5 {
			t.Fatalf("retained HA state after delayed reply: %s (%v)", out, err)
		}
	})
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.deviceLoop(ctx)
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
	if err := f.Conn.Emit(device.Path("Voice"), device.Interface("Voice")+".Event", "status", `{"status":"disabled"}`); err != nil {
		t.Fatal(err)
	}
	pynabWait(t, "disabled LEDs", time.Second, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.indicator == "" })
}
