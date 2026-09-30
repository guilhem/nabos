// Run with NABOS_INTEGRATION=1 and DEVICE_CORE_BIN. Requires D-Bus, cargo and Mosquitto.
// NAB_CORE_BIN accepts a binary path or a command line (for example an emulator);
// without it, cargo builds core with --locked. CARGO_TARGET_DIR, MOSQUITTO,
// MOSQUITTO_PUB and MOSQUITTO_SUB override the build directory and MQTT tools.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guilhem/nabos/services/internal/bus"
	"github.com/guilhem/nabos/services/internal/config"
)

const pynabTopic = "nabos/v1/core/"

type pynabMessage struct {
	at    time.Time
	topic string
	body  map[string]any
}

type pynabMQTT struct {
	t    *testing.T
	mu   sync.Mutex
	log  []pynabMessage
	pub  string
	port int
	env  []string
}

func pynabTool(t *testing.T, env, name string, fallback ...string) string {
	t.Helper()
	if path := os.Getenv(env); path != "" {
		return path
	}
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	for _, path := range fallback {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	t.Fatalf("missing %s; set %s", name, env)
	return ""
}

func pynabCommandLine(value string) []string {
	if _, err := os.Stat(value); err == nil {
		return []string{value}
	}
	return strings.Fields(value)
}

func pynabCore(t *testing.T, repo string) []string {
	t.Helper()
	if value := os.Getenv("NAB_CORE_BIN"); value != "" {
		args := pynabCommandLine(value)
		if len(args) == 0 {
			t.Fatal("empty NAB_CORE_BIN")
		}
		return args
	}
	cmd := exec.Command("cargo", "build", "--locked", "--quiet", "--manifest-path", filepath.Join(repo, "core", "Cargo.toml"))
	cmd.Dir = repo
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("cargo build --locked: %v", err)
	}
	target := os.Getenv("CARGO_TARGET_DIR")
	if target == "" {
		target = filepath.Join(repo, "core", "target")
	} else if !filepath.IsAbs(target) {
		target = filepath.Join(repo, target)
	}
	return []string{filepath.Join(target, "debug", "nab-core")}
}

func pynabWait(t *testing.T, what string, timeout time.Duration, ready func() bool) {
	t.Helper()
	for end := time.Now().Add(timeout); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if ready() {
			return
		}
	}
	t.Fatalf("timeout waiting for %s", what)
}

func (h *pynabMQTT) start(t *testing.T, name string, args []string, extra ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(append([]string{}, h.env...), extra...)
	log, err := os.Create(filepath.Join(t.TempDir(), name+".log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		log.Close()
		t.Fatalf("start %s: %v", name, err)
	}
	log.Close()
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd
}

func (h *pynabMQTT) publish(topic string, payload map[string]any) {
	h.t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		h.t.Fatal(err)
	}
	cmd := exec.Command(h.pub, "-h", "127.0.0.1", "-p", strconv.Itoa(h.port), "-V", "mqttv5", "-q", "1", "-t", pynabTopic+"event/"+topic, "-m", string(raw))
	cmd.Env = h.env
	if out, err := cmd.CombinedOutput(); err != nil {
		h.t.Fatalf("publish %s: %v: %s", topic, err, out)
	}
}

func (h *pynabMQTT) snapshot() []pynabMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]pynabMessage(nil), h.log...)
}

func (h *pynabMQTT) command(after int, timeout time.Duration, match func(map[string]any) bool) (int, map[string]any) {
	h.t.Helper()
	var index int
	var found map[string]any
	pynabWait(h.t, "MQTT command", timeout, func() bool {
		for i, m := range h.snapshot() {
			if i >= after && m.topic == bus.TopicCmd && match(m.body) {
				index, found = i, m.body
				return true
			}
		}
		return false
	})
	return index, found
}

func (h *pynabMQTT) result(id string, timeout time.Duration) map[string]any {
	h.t.Helper()
	var found map[string]any
	pynabWait(h.t, "result for "+id, timeout, func() bool {
		for _, m := range h.snapshot() {
			if m.topic == bus.TopicResult && m.body["id"] == id {
				found = m.body
				return true
			}
		}
		return false
	})
	return found
}

func pynabAudio(command map[string]any) string {
	args, _ := command["args"].(map[string]any)
	seq, _ := args["sequence"].([]any)
	if len(seq) == 0 {
		return ""
	}
	item, _ := seq[0].(map[string]any)
	audio, _ := item["audio"].([]any)
	if len(audio) == 0 {
		return ""
	}
	value, _ := audio[0].(string)
	return value
}

func (h *pynabMQTT) play(after int, suffix string) (int, map[string]any) {
	h.t.Helper()
	return h.command(after, 12*time.Second, func(c map[string]any) bool {
		return c["action"] == "play" && strings.HasSuffix(pynabAudio(c), suffix)
	})
}

func (h *pynabMQTT) playing(a *App, id string) {
	h.t.Helper()
	pynabWait(h.t, "core playing "+id, 5*time.Second, func() bool {
		state, online := a.bus.State()
		return online && state.State == "playing" && state.Playing != nil && *state.Playing == id
	})
}

func (h *pynabMQTT) canceled(target string, after int) {
	h.t.Helper()
	_, cancel := h.command(after, 5*time.Second, func(c map[string]any) bool {
		if c["action"] != "cancel" {
			return false
		}
		args, _ := c["args"].(map[string]any)
		return args["target"] == target
	})
	if got := h.result(target, 5*time.Second)["status"]; got != "canceled" {
		for _, m := range h.snapshot() {
			if m.topic == bus.TopicCmd || m.topic == bus.TopicResult || strings.Contains(m.topic, "event/ear_moved") {
				h.t.Logf("%s %s id=%v action=%v status=%v audio=%q args=%v", m.at.Format("15:04:05.000"), m.topic, m.body["id"], m.body["action"], m.body["status"], pynabAudio(m.body), m.body["args"])
			}
		}
		h.t.Fatalf("targeted cancel of %s: %v", target, got)
	}
	if got := h.result(cancel["id"].(string), 5*time.Second)["status"]; got != "ok" {
		h.t.Fatalf("cancel command: %v", got)
	}
}

func TestPynabMQTTIntegration(t *testing.T) {
	if os.Getenv("NABOS_INTEGRATION") != "1" {
		t.Skip("set NABOS_INTEGRATION=1 for Mosquitto and simulated core")
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	assets := filepath.Join(repo, "assets")
	core := pynabCore(t, repo)
	broker := pynabTool(t, "MOSQUITTO", "mosquitto", "/usr/sbin/mosquitto")
	pub := pynabTool(t, "MOSQUITTO_PUB", "mosquitto_pub")
	sub := pynabTool(t, "MOSQUITTO_SUB", "mosquitto_sub")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	h := &pynabMQTT{t: t, pub: pub, port: port, env: os.Environ()}
	conf := filepath.Join(t.TempDir(), "mosquitto.conf")
	if err := os.WriteFile(conf, []byte(fmt.Sprintf("listener %d 127.0.0.1\nallow_anonymous true\npersistence false\n", port)), 0o644); err != nil {
		t.Fatal(err)
	}
	h.start(t, "broker", []string{broker, "-c", conf})
	pynabWait(t, "broker", 5*time.Second, func() bool {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err == nil {
			c.Close()
		}
		return err == nil
	})
	subCmd := exec.Command(sub, "-h", "127.0.0.1", "-p", strconv.Itoa(port), "-V", "mqttv5", "-q", "1", "-t", "nabos/v1/#", "-F", "%t|%p")
	subCmd.Env = h.env
	pipe, err := subCmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := subCmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = subCmd.Process.Kill(); _ = subCmd.Wait() })
	go func() {
		scan := bufio.NewScanner(pipe)
		scan.Buffer(nil, 1<<20)
		for scan.Scan() {
			topic, raw, ok := strings.Cut(scan.Text(), "|")
			if !ok {
				continue
			}
			var body map[string]any
			if json.Unmarshal([]byte(raw), &body) != nil {
				continue
			}
			h.mu.Lock()
			h.log = append(h.log, pynabMessage{time.Now(), topic, body})
			h.mu.Unlock()
		}
	}()
	time.Sleep(200 * time.Millisecond) // let the subscriber attach before starting the core
	privateBus := exec.Command("dbus-daemon", "--session", "--nofork", "--print-address=1")
	busOut, err := privateBus.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := privateBus.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { privateBus.Process.Kill(); privateBus.Wait() })
	address, err := bufio.NewReader(busOut).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	address = strings.TrimSpace(address)
	t.Setenv("DBUS_SYSTEM_BUS_ADDRESS", address)
	h.env = append(h.env, "DBUS_SYSTEM_BUS_ADDRESS="+address)
	deviceBin := os.Getenv("DEVICE_CORE_BIN")
	if deviceBin == "" {
		t.Fatal("set DEVICE_CORE_BIN to the current device-core binary")
	}
	data := t.TempDir()
	h.start(t, "device", append(pynabCommandLine(deviceBin), "--simulate"),
		"DEVICE_CORE_BUS_ADDRESS="+address, "DEVICE_CORE_DATA_DIR="+data,
		"DEVICE_CORE_NETWORK_GUARD="+filepath.Join(data, "network.lock"),
		"DEVICE_CORE_MAINTENANCE_UNITS=", "DEVICE_CORE_AUDIO_ROOTS="+filepath.Join(assets, "sounds"),
		"DEVICE_CORE_SIM_AUDIO_MS=2500")
	coreEnv := []string{"NABOS_DEVICE_BUS_ADDRESS=" + address, "NABOS_MQTT_PORT=" + strconv.Itoa(port), "NABOS_SOUNDS_DIRS=" + filepath.Join(assets, "sounds"),
		"NABOS_CHOREOGRAPHIES_DIRS=" + filepath.Join(assets, "choreographies")}
	coreProcess := h.start(t, "core", append(core, "--simulate"), coreEnv...)
	a, err := NewApp(Env{MQTTHost: "127.0.0.1", MQTTPort: port, DataDir: t.TempDir(), SoundsDirs: []string{filepath.Join(assets, "sounds")}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.device.Close)
	ctx, stop := context.WithCancel(context.Background())
	a.ctx = ctx
	go a.eventLoop(ctx)
	if err := a.bus.Start(ctx); err != nil {
		stop()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stop()
		c, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		a.bus.Stop(c)
	})
	pynabWait(t, "service and core online", 10*time.Second, func() bool {
		connected, online := a.bus.Healthy()
		state, ok := a.bus.State()
		return connected && online && ok && state.State == "idle"
	})
	a.publishSettings(ctx)
	go a.servicesLoop(ctx)
	serviceSession(t, a)
	adminHash := a.store.Get().Admin.Hash

	t.Run("book ear navigation, targeted click cancel, exclusive chime", func(t *testing.T) {
		h.t = t
		defer a.stopInteraction()
		begin := len(h.snapshot())
		h.publish("rfid", map[string]any{"event": "detected", "app": "book", "data": "default/9782092512593", "uid": "d0:02:18:01:02:03:04:05"})
		firstIndex, first := h.play(begin, "book/books/9782092512593/default/1.mp3")
		if args := first["args"].(map[string]any); args["cancelable"] != false {
			t.Fatalf("chapter must be owned: %v", args)
		}
		h.playing(a, first["id"].(string))
		a.chime(12)
		h.publish("ear_moved", map[string]any{"ear": "right"})
		h.canceled(first["id"].(string), firstIndex)
		h.play(firstIndex+1, "book/next.mp3")
		// A second driver notification during the same gesture is ignored
		// while the navigation feedback plays; it must not cancel chapter 2.
		delayed := appEvent{kind: "ear_moved", data: map[string]any{"ear": "right"}, received: time.Now()}
		h.publish("ear_moved", map[string]any{"ear": "right"})
		secondIndex, second := h.play(firstIndex+1, "book/books/9782092512593/default/2.mp3")
		h.playing(a, second["id"].(string))
		// Simulate an event held in the app queue by a slow HA publication.
		// Its reception predates the chapter even though delivery comes later.
		a.events <- delayed
		time.Sleep(100 * time.Millisecond)
		if state, _ := a.bus.State(); state.Playing == nil || *state.Playing != second["id"] {
			t.Fatal("duplicate ear notification skipped chapter 2")
		}
		h.publish("ear_moved", map[string]any{"ear": "left"})
		h.canceled(second["id"].(string), secondIndex)
		thirdIndex, third := h.play(secondIndex+1, "book/books/9782092512593/default/1.mp3")
		h.playing(a, third["id"].(string))
		for i, m := range h.snapshot() {
			if i > firstIndex && i < thirdIndex && m.topic == bus.TopicCmd && m.body["action"] == "message" {
				t.Fatal("chime interleaved between book chapters")
			}
		}
		h.publish("button", map[string]any{"event": "click"})
		h.canceled(third["id"].(string), thirdIndex)
		promptIndex, prompt := h.play(thirdIndex+1, "system/abort.wav")
		promptArgs, _ := prompt["args"].(map[string]any)
		items, _ := promptArgs["sequence"].([]any)
		if len(items) != 2 {
			t.Fatalf("chapter stop should be one two-part play: %v", promptArgs)
		}
		spoken, _ := items[1].(map[string]any)
		audio, _ := spoken["audio"].([]any)
		if len(audio) != 1 || audio[0] != "fr_FR/book/interrupt.mp3" || spoken["choreography"] != "fr_FR/book/interrupt.chor" {
			t.Fatalf("chapter stop prompt: %v", spoken)
		}
		if got := h.result(prompt["id"].(string), 12*time.Second)["status"]; got != "ok" {
			t.Fatalf("interrupt prompt: %v", got)
		}
		_, chime := h.command(promptIndex+1, 12*time.Second, func(c map[string]any) bool { return c["action"] == "message" })
		if got := h.result(chime["id"].(string), 12*time.Second)["status"]; got != "ok" {
			t.Fatalf("chime: %v", got)
		}
	})

	t.Run("eightball hold and release keeps admin password", func(t *testing.T) {
		h.t = t
		pynabWait(t, "book interaction released", 5*time.Second, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.interaction == nil })
		pynabWait(t, "core idle after book", 8*time.Second, func() bool { state, online := a.bus.State(); return online && state.State == "idle" })
		begin := len(h.snapshot())
		h.publish("button", map[string]any{"event": "click_and_hold"})
		h.publish("button", map[string]any{"event": "up"})
		listenIndex, listen := h.play(begin, "eightball/listen.mp3")
		h.canceled(listen["id"].(string), listenIndex)
		acquiredIndex, acquired := h.play(listenIndex+1, "eightball/acquired.mp3")
		if got := h.result(acquired["id"].(string), 12*time.Second)["status"]; got != "ok" {
			t.Fatalf("acquired: %v", got)
		}
		_, answer := h.command(acquiredIndex+1, 12*time.Second, func(c map[string]any) bool { return c["action"] == "message" })
		a.mu.Lock()
		active := a.interaction != nil
		a.mu.Unlock()
		if !active {
			t.Fatal("eightball released before answering")
		}
		if got := h.result(answer["id"].(string), 12*time.Second)["status"]; got != "ok" {
			t.Fatalf("answer: %v", got)
		}
		pynabWait(t, "eightball released after answer", 5*time.Second, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.interaction == nil })
		if got := a.store.Get().Admin.Hash; got != adminHash || !a.auth.Configured() {
			t.Fatal("click_and_hold reset the admin password")
		}
	})

	t.Run("RFID webhook uses saved association and respects disable", func(t *testing.T) {
		h.t = t
		hits := make(chan struct{}, 2)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("webhook method: %s", r.Method)
			}
			hits <- struct{}{}
		}))
		defer server.Close()
		const uid = "d0:02:18:01:02:03:04:06"
		_, err := a.store.Update(func(s *config.Settings) error {
			s.Tags[uid] = config.TagAction{App: "webhook", Value: server.URL}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		tag := map[string]any{"event": "detected", "app": "webhook", "uid": uid}
		a.mu.Lock()
		a.serviceErrors["webhook"] = "pending"
		a.mu.Unlock()
		h.publish("rfid", tag)
		select {
		case <-hits:
		case <-time.After(5 * time.Second):
			t.Fatal("associated webhook not called")
		}
		pynabWait(t, "webhook completed", 5*time.Second, func() bool {
			a.mu.Lock()
			defer a.mu.Unlock()
			_, pending := a.serviceErrors["webhook"]
			return !pending
		})
		_, err = a.store.Update(func(s *config.Settings) error { s.Services.Webhooks = false; return nil })
		if err != nil {
			t.Fatal(err)
		}
		h.publish("rfid", tag)
		pynabWait(t, "disabled webhook rejected", 5*time.Second, func() bool {
			a.mu.Lock()
			defer a.mu.Unlock()
			return strings.Contains(a.serviceErrors["webhook"], "désactivés")
		})
		select {
		case <-hits:
			t.Fatal("disabled webhook was called")
		default:
		}
	})
	for _, reason := range []string{"resync", "core offline"} {
		t.Run("ordinary media releases on "+reason, func(t *testing.T) {
			h.t = t
			begin := len(h.snapshot())
			done := make(chan error, 1)
			go func() {
				done <- a.media(ctx, "play", map[string]any{"sequence": []any{map[string]any{"audio": []string{
					"system/abort.wav", "system/abort.wav", "system/abort.wav",
				}}}}, 10*time.Minute)
			}()
			_, playing := h.play(begin, "system/abort.wav")
			h.playing(a, playing["id"].(string))
			if reason == "resync" {
				a.resync()
			} else if err := coreProcess.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("lost playback: %v", err)
				}
			case <-time.After(6 * time.Second):
				t.Fatal("lost core left ordinary media holding the gate")
			}
			if reason == "core offline" {
				begin = len(h.snapshot())
				h.start(t, "core-restarted", append(core, "--simulate"), coreEnv...)
				h.command(begin, 5*time.Second, func(c map[string]any) bool { return c["action"] == "ears" })
			}
			pynabWait(t, "core ready after cancellation", 5*time.Second, func() bool {
				state, online := a.bus.State()
				return online && state.State == "idle"
			})
			if err := a.media(ctx, "play", sequence("system/abort.wav", ""), 5*time.Second); err != nil {
				t.Fatalf("next playback remained blocked: %v", err)
			}
		})
	}
}
