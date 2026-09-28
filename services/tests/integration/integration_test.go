// Package integration is the end-to-end test: real Mosquitto +
// nab-core --simulate + nab-service.
//
// It covers the MQTT contract (docs/protocol-v1.md): execution, results,
// deduplication/replay, expiration (on arrival and while queued), cancel,
// retained command refusal, bounds validation, broker restart and core
// restart, plus the HTTP side of nab-service (setup with physical presence,
// CSRF, authenticated actions, settings persistence, /healthz).
//
// Run from services/: NABOS_INTEGRATION=1 go test -count=1 ./tests/integration
// Requirements: mosquitto, mosquitto_pub, mosquitto_sub, cargo, go.
// Overrides: an existing path is used as is (spaces allowed); anything else
// is split on spaces, so emulator command lines work:
//
//	NAB_CORE_BIN="qemu-arm-static -L /sysroot /sysroot/usr/bin/nab-core"
//	NAB_SERVICE_BIN="/sysroot/usr/bin/nab-service"
//	MOSQUITTO=/usr/sbin/mosquitto
//	NABOS_TEST_ASSETS=/sysroot/usr/share/nabos
//
// Without overrides, native debug builds of core/ and services/ are made.
package integration

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const prefix = "nabos/v1"

var repo, _ = filepath.Abs("../../..")

type message struct {
	at       time.Time
	retained bool
	topic    string
	payload  any // decoded JSON object, or the raw string
}

type harness struct {
	t                   *testing.T
	tmp                 string
	mqttPort, httpPort  int
	procs               map[string]*exec.Cmd
	mu                  sync.Mutex
	log                 []message
	mosquitto, pub, sub string
	core, service       []string
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func which(t *testing.T, name string, extra ...string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, p := range extra {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Fatalf("missing %s", name)
	return ""
}

func (h *harness) fatalf(format string, args ...any) {
	h.t.Helper()
	h.t.Fatalf(format, args...)
}

func commandLine(value string) []string {
	if _, err := os.Stat(value); err == nil {
		return []string{value}
	}
	return strings.Fields(value)
}

func (h *harness) build() {
	t := h.t
	core := os.Getenv("NAB_CORE_BIN")
	if core == "" {
		cmd := exec.Command("cargo", "build", "--quiet", "--manifest-path", filepath.Join(repo, "core/Cargo.toml"))
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("cargo build: %v", err)
		}
		target := os.Getenv("CARGO_TARGET_DIR")
		if target == "" {
			target = filepath.Join(repo, "core/target")
		}
		core = filepath.Join(target, "debug/nab-core")
	}
	service := os.Getenv("NAB_SERVICE_BIN")
	if service == "" {
		service = filepath.Join(h.tmp, "nab-service")
		cmd := exec.Command("go", "build", "-o", service, "./cmd/nab-service")
		cmd.Dir = filepath.Join(repo, "services")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build: %v\n%s", err, out)
		}
	}
	h.core, h.service = commandLine(core), commandLine(service)
	for _, c := range [][]string{h.core, h.service} {
		out, err := exec.Command(c[0], append(c[1:], "--version")...).Output()
		if err != nil {
			t.Fatalf("%s --version failed: %v", c, err)
		}
		t.Logf("using %s (%s)", strings.Join(c, " "), strings.TrimSpace(string(out)))
	}
}

func (h *harness) spawn(name string, args []string, env ...string) {
	logf, err := os.OpenFile(filepath.Join(h.tmp, name+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		h.fatalf("%v", err)
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		h.fatalf("%s: %v", name, err)
	}
	logf.Close()
	h.procs[name] = cmd
}

func (h *harness) stop(name string, sig os.Signal) {
	cmd := h.procs[name]
	delete(h.procs, name)
	if cmd == nil {
		return
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	cmd.Process.Signal(sig)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		<-done
	}
}

func portOpen(port int) bool {
	c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err == nil {
		c.Close()
	}
	return err == nil
}

func (h *harness) waitFor(timeout time.Duration, what string, cond func() bool) {
	h.t.Helper()
	for end := time.Now().Add(timeout); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if cond() {
			return
		}
	}
	h.fatalf("timeout waiting for %s", what)
}

func (h *harness) startBroker() {
	conf := filepath.Join(h.tmp, "mosquitto.conf")
	os.WriteFile(conf, fmt.Appendf(nil, "listener %d 127.0.0.1\nallow_anonymous true\npersistence false\n", h.mqttPort), 0o644)
	h.spawn("mosquitto", []string{h.mosquitto, "-c", conf})
	h.waitFor(10*time.Second, "broker listening", func() bool { return portOpen(h.mqttPort) })
}

func (h *harness) startSubReader() {
	cmd := exec.Command(h.sub, "-h", "127.0.0.1", "-p", strconv.Itoa(h.mqttPort), "-V", "mqttv5", "-q", "1",
		"-t", prefix+"/#", "-F", "%r|%t|%p")
	out, err := cmd.StdoutPipe()
	if err != nil {
		h.fatalf("%v", err)
	}
	if err := cmd.Start(); err != nil {
		h.fatalf("%v", err)
	}
	h.procs["sub"] = cmd
	go func() {
		scanner := bufio.NewScanner(out)
		scanner.Buffer(nil, 1<<20)
		for scanner.Scan() {
			parts := strings.SplitN(scanner.Text(), "|", 3)
			if len(parts) != 3 {
				continue
			}
			m := message{time.Now(), parts[0] == "1", parts[1], parts[2]}
			var obj map[string]any
			if strings.HasPrefix(parts[2], "{") && json.Unmarshal([]byte(parts[2]), &obj) == nil {
				m.payload = obj
			}
			h.mu.Lock()
			h.log = append(h.log, m)
			h.mu.Unlock()
		}
	}()
	time.Sleep(500 * time.Millisecond)
}

func (h *harness) startCore() {
	assets := os.Getenv("NABOS_TEST_ASSETS")
	if assets == "" {
		assets = filepath.Join(repo, "assets")
	}
	h.spawn("core", append(h.core, "--simulate"),
		"NABOS_MQTT_PORT="+strconv.Itoa(h.mqttPort), "NABOS_SOUNDS_DIRS="+filepath.Join(assets, "sounds"),
		"NABOS_CHOREOGRAPHIES_DIRS="+filepath.Join(assets, "choreographies"), "NABOS_SIM_AUDIO_MS=200", "NABOS_LOG=debug")
}

func (h *harness) startService() {
	data := filepath.Join(h.tmp, "data")
	os.MkdirAll(data, 0o755)
	cfg := filepath.Join(data, "config.json")
	if _, err := os.Stat(cfg); err != nil {
		// Never asleep, no chime: time-of-day independent test.
		midnight := map[string]int{"hour": 0, "min": 0}
		day := map[string]any{"wakeup": midnight, "sleep": midnight}
		b, _ := json.Marshal(map[string]any{"version": 1, "locale": "fr_FR", "timezone": "Europe/Paris", "volume": 70,
			"clock": map[string]any{"chime": false, "sleep_sounds": false, "wakeup": midnight, "sleep": midnight,
				"days": []any{day, day, day, day, day, day, day}},
			"weather":        map[string]any{"unit": "celsius", "animation": "weather_and_rain", "frequency": 0},
			"home_assistant": map[string]any{"port": 1883, "discovery_prefix": "homeassistant"}})
		os.WriteFile(cfg, b, 0o600)
	}
	h.spawn("service", h.service,
		"NABOS_MQTT_PORT="+strconv.Itoa(h.mqttPort), "NABOS_HTTP_ADDR=127.0.0.1:"+strconv.Itoa(h.httpPort),
		"NABOS_DATA_DIR="+data, "NABOS_LVA_UNIT=", "NABOS_TIMESYNC_FILE=none",
		"NABOS_WEATHER_URL=http://127.0.0.1:9/forecast", "NABOS_GEOCODING_URL=http://127.0.0.1:9/search",
		"NABOS_NET_PROBE=127.0.0.1:9", "NABOS_LOG=debug", "NABOS_VERSION=v0.0.1")
	h.waitFor(15*time.Second, "service listening", func() bool { return portOpen(h.httpPort) })
}

// MQTT helpers

func (h *harness) publish(topic, payload string, retain bool) bool {
	args := []string{"-h", "127.0.0.1", "-p", strconv.Itoa(h.mqttPort), "-V", "mqttv5", "-q", "1", "-t", topic, "-m", payload}
	if retain {
		args = append(args, "-r")
	}
	return exec.Command(h.pub, args...).Run() == nil
}

func in(d time.Duration) time.Time { return time.Now().Add(d) }

func (h *harness) command(id, action string, args any, expires time.Time, retain bool) {
	if args == nil {
		args = map[string]any{}
	}
	b, _ := json.Marshal(map[string]any{"v": 1, "id": id, "expires_at": expires.Unix(), "action": action, "args": args})
	h.publish(prefix+"/core/cmd", string(b), retain)
}

func (h *harness) messages(topic string, since time.Time) []message {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []message
	for _, m := range h.log {
		if m.topic == topic && !m.at.Before(since) {
			out = append(out, m)
		}
	}
	return out
}

// objects returns the JSON object payloads published on topic since a time.
func (h *harness) objects(topic string, since time.Time) []map[string]any {
	var out []map[string]any
	for _, m := range h.messages(topic, since) {
		if obj, ok := m.payload.(map[string]any); ok {
			out = append(out, obj)
		}
	}
	return out
}

func (h *harness) results(id string) []map[string]any {
	var out []map[string]any
	for _, m := range h.objects(prefix+"/core/result", time.Time{}) {
		if m["id"] == id {
			out = append(out, m)
		}
	}
	return out
}

func (h *harness) result(id string, timeout time.Duration) map[string]any {
	h.t.Helper()
	h.waitFor(timeout, "result of "+id, func() bool { return len(h.results(id)) > 0 })
	return h.results(id)[0]
}

func (h *harness) status(id string) any {
	h.t.Helper()
	return h.result(id, 10*time.Second)["status"]
}

func (h *harness) stateSince(since time.Time, state string, timeout time.Duration) {
	h.t.Helper()
	h.waitFor(timeout, "state "+state, func() bool {
		for _, m := range h.objects(prefix+"/core/state", since) {
			if m["state"] == state {
				return true
			}
		}
		return false
	})
}

func (h *harness) onlineSince(since time.Time, value string, timeout time.Duration) {
	h.t.Helper()
	h.waitFor(timeout, "core "+value, func() bool {
		for _, m := range h.messages(prefix+"/core/availability", since) {
			if m.payload == value {
				return true
			}
		}
		return false
	})
}

// HTTP helpers

var client = &http.Client{Timeout: 90 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (h *harness) http(method, path string, form url.Values, cookie string, origin bool) (int, http.Header, string) {
	h.t.Helper()
	req, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", h.httpPort, path), strings.NewReader(form.Encode()))
	if err != nil {
		h.fatalf("%v", err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if origin && method == http.MethodPost {
		req.Header.Set("Origin", fmt.Sprintf("http://127.0.0.1:%d", h.httpPort))
	}
	resp, err := client.Do(req)
	if err != nil {
		h.fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var body strings.Builder
	bufio.NewReader(resp.Body).WriteTo(&body)
	return resp.StatusCode, resp.Header, body.String()
}

func (h *harness) healthz() int {
	code, _, _ := h.http(http.MethodGet, "/healthz", nil, "", true)
	return code
}

// check runs one scenario; a failure is reported and the next one still runs.
func (h *harness) check(name string, fn func()) {
	parent := h.t
	parent.Run(name, func(t *testing.T) {
		h.t = t
		fn()
	})
	h.t = parent
}

func TestEndToEnd(t *testing.T) {
	if os.Getenv("NABOS_INTEGRATION") != "1" {
		t.Skip("set NABOS_INTEGRATION=1 to run the end-to-end test")
	}
	tmp, err := os.MkdirTemp("", "nabos-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, tmp: tmp, mqttPort: freePort(t), httpPort: freePort(t), procs: map[string]*exec.Cmd{}}
	h.mosquitto = os.Getenv("MOSQUITTO")
	if h.mosquitto == "" {
		h.mosquitto = which(t, "mosquitto", "/usr/sbin/mosquitto")
	}
	h.pub, h.sub = which(t, "mosquitto_pub"), which(t, "mosquitto_sub")
	h.build()
	t.Cleanup(func() {
		for _, name := range []string{"service", "core", "sub", "mosquitto"} {
			h.stop(name, syscall.SIGTERM)
		}
		t.Logf("core: %s\nservice: %s\nbroker: %s", strings.Join(h.core, " "), strings.Join(h.service, " "), h.mosquitto)
		if !t.Failed() {
			os.RemoveAll(tmp)
			return
		}
		for _, name := range []string{"core", "service", "mosquitto"} {
			if data, err := os.ReadFile(filepath.Join(tmp, name+".log")); err == nil {
				lines := strings.Split(string(data), "\n")
				t.Logf("--- %s.log (tail)\n%s", name, strings.Join(lines[max(0, len(lines)-40):], "\n"))
			}
		}
	})

	h.startBroker()
	h.startSubReader()
	t0 := time.Now()
	h.startCore()
	h.onlineSince(t0, "online", 15*time.Second)
	h.stateSince(t0, "idle", 10*time.Second)
	abort := map[string]any{"sequence": []any{map[string]any{"audio": []string{"system/abort.wav"}}}}
	expect := func(got, want any, context ...any) {
		h.t.Helper()
		if got != want {
			h.fatalf("got %v, want %v %v", got, want, context)
		}
	}

	h.check("play executes and returns to idle", func() {
		t := time.Now()
		h.command("p1", "play", map[string]any{"sequence": []any{map[string]any{"audio": []string{"system/abort.wav"},
			"choreography": "system/rfid.chor"}}}, in(time.Minute), false)
		expect(h.status("p1"), "ok")
		h.stateSince(t, "playing", 10*time.Second)
		h.stateSince(t, "idle", 10*time.Second)
	})
	h.check("duplicate id is not executed again", func() {
		h.command("p1", "play", abort, in(time.Minute), false)
		h.waitFor(5*time.Second, "second answer", func() bool { return len(h.results("p1")) == 2 })
		expect(h.results("p1")[1]["status"], "duplicate", h.results("p1"))
	})
	h.check("expired command refused", func() {
		h.command("old", "play", abort, in(-30*time.Second), false)
		expect(h.status("old"), "expired")
	})
	h.check("traversal, bounds and garbage refused", func() {
		h.command("trav", "play", map[string]any{"sequence": []any{map[string]any{"audio": []string{"../../etc/passwd"}}}}, in(time.Minute), false)
		expect(h.status("trav"), "rejected")
		h.command("far", "sleep", nil, in(7*24*time.Hour), false)
		expect(h.status("far"), "rejected")
		h.publish(prefix+"/core/cmd", "not json", false)
		h.command("ears", "ears", map[string]any{"left": 99}, in(time.Minute), false)
		expect(h.status("ears"), "rejected")
	})
	h.check("retained command refused and cleared", func() {
		h.command("ret", "play", abort, in(time.Minute), true)
		r := h.result("ret", 10*time.Second)
		if r["status"] != "rejected" || r["error"] != "retained" {
			h.fatalf("%v", r)
		}
		out, _ := exec.Command(h.sub, "-h", "127.0.0.1", "-p", strconv.Itoa(h.mqttPort), "-t", prefix+"/core/cmd",
			"--retained-only", "-W", "2").Output()
		if s := strings.TrimSpace(string(out)); s != "" {
			h.fatalf("retained command left on broker: %q", s)
		}
	})
	long := map[string]any{"sequence": []any{map[string]any{"audio": []string{"system/abort.wav", "system/abort.wav",
		"system/abort.wav", "system/abort.wav", "system/abort.wav", "system/abort.wav", "system/abort.wav",
		"system/abort.wav", "system/abort.wav", "system/abort.wav"}}}}
	h.check("cancel running and queued commands", func() {
		h.command("long", "play", long, in(time.Minute), false)
		h.command("queued", "play", long, in(time.Minute), false)
		time.Sleep(500 * time.Millisecond)
		h.command("c1", "cancel", map[string]any{"target": "queued"}, in(time.Minute), false)
		expect(h.status("c1"), "ok")
		expect(h.status("queued"), "canceled")
		h.command("c2", "cancel", map[string]any{"target": "long"}, in(time.Minute), false)
		expect(h.status("c2"), "ok")
		expect(h.status("long"), "canceled")
		h.command("c3", "cancel", nil, in(time.Minute), false)
		expect(h.result("c3", 10*time.Second)["error"], "not_playing")
	})
	h.check("only a targeted cancel stops noncancelable playback", func() {
		owned := map[string]any{"sequence": long["sequence"], "cancelable": false}
		at := time.Now()
		h.command("owned", "play", owned, in(time.Minute), false)
		h.stateSince(at, "playing", 5*time.Second)
		h.command("untargeted", "cancel", nil, in(time.Minute), false)
		expect(h.result("untargeted", 5*time.Second)["error"], "not_cancelable")
		h.command("targeted", "cancel", map[string]any{"target": "owned"}, in(time.Minute), false)
		expect(h.status("targeted"), "ok")
		expect(h.status("owned"), "canceled")
	})
	h.check("command expires while queued", func() {
		h.command("blocker", "play", long, in(time.Minute), false)
		h.command("short", "play", abort, in(time.Second), false)
		expect(h.result("short", 5*time.Second)["status"], "expired")
		expect(h.status("blocker"), "ok")
	})

	h.startService()
	var cookie string
	post := func(path string, form url.Values, cookie string, origin bool) (int, http.Header) {
		code, header, _ := h.http(http.MethodPost, path, form, cookie, origin)
		return code, header
	}
	h.check("healthz, setup needs a button press", func() {
		h.waitFor(15*time.Second, "healthz 200", func() bool { return h.healthz() == 200 })
		code, header, _ := h.http(http.MethodGet, "/", nil, "", true)
		if code != 303 || header.Get("Location") != "/setup" {
			h.fatalf("%d %v", code, header)
		}
		password := url.Values{"password": {"carotte-42"}, "confirm": {"carotte-42"}}
		if _, header := post("/setup", password, "", true); !strings.Contains(header.Get("Location"), "err=") {
			h.fatalf("setup accepted without a button press")
		}
		h.publish(prefix+"/core/event/button", fmt.Sprintf(`{"v": 1, "event": "click", "time": %f}`,
			float64(time.Now().UnixNano())/1e9), false)
		time.Sleep(500 * time.Millisecond)
		code, header = post("/setup", password, "", true)
		if code != 303 || header.Get("Location") != "/settings" {
			h.fatalf("%d %v", code, header)
		}
		cookie, _, _ = strings.Cut(header.Get("Set-Cookie"), ";")
		code, _, body := h.http(http.MethodGet, "/", nil, cookie, true)
		if code != 200 || !strings.Contains(body, "réveillé") {
			h.fatalf("home page %d", code)
		}
	})
	h.check("authenticated, same-origin UI action reaches the core", func() {
		if code, _ := post("/action", url.Values{"name": {"time"}}, "", true); code != 401 {
			h.fatalf("anonymous action: %d", code)
		}
		if code, _ := post("/action", url.Values{"name": {"time"}}, cookie, false); code != 403 {
			h.fatalf("cross-site action: %d", code)
		}
		t := time.Now()
		code, header := post("/action", url.Values{"name": {"play"}, "resource": {"system/abort.wav"}}, cookie, true)
		if code != 303 || !strings.Contains(header.Get("Location"), "ok=") {
			h.fatalf("%d %v", code, header)
		}
		// The HTTP response and this independent MQTT subscriber can arrive in
		// either order; wait for the observer as with other core results.
		h.waitFor(5*time.Second, "UI command result on MQTT", func() bool {
			for _, m := range h.objects(prefix+"/core/result", t) {
				if id, _ := m["id"].(string); strings.HasPrefix(id, "svc-") && m["status"] == "ok" {
					return true
				}
			}
			return false
		})
	})
	h.check("settings saved atomically and published", func() {
		form := url.Values{"locale": {"en_US"}, "timezone": {"Europe/Paris"}, "volume": {"50"}, "wakeup": {"00:00"},
			"sleep": {"00:00"}, "location": {""}, "unit": {"celsius"}, "animation": {"nothing"}, "frequency": {"0"},
			"ha_host": {""}, "ha_port": {"1883"}, "ha_prefix": {"homeassistant"}}
		for i := range 7 {
			form.Set(fmt.Sprintf("wakeup_%d", i), "00:00")
			form.Set(fmt.Sprintf("sleep_%d", i), "00:00")
		}
		if _, header := post("/settings", form, cookie, true); !strings.Contains(header.Get("Location"), "ok=") {
			h.fatalf("%v", header)
		}
		var saved struct {
			Volume float64
			Locale string
			Admin  struct{ Hash string }
		}
		data, _ := os.ReadFile(filepath.Join(tmp, "data/config.json"))
		if json.Unmarshal(data, &saved) != nil || saved.Volume != 50 || saved.Locale != "en_US" || saved.Admin.Hash == "" {
			h.fatalf("saved %s", data)
		}
		settings := h.objects(prefix+"/service/settings", time.Time{})
		if len(settings) == 0 || settings[len(settings)-1]["locale"] != "en_US" {
			h.fatalf("published %v", settings)
		}
	})
	h.check("sleep and wake up from the UI", func() {
		t := time.Now()
		post("/action", url.Values{"name": {"sleep"}}, cookie, true)
		h.stateSince(t, "asleep", 10*time.Second)
		t = time.Now()
		post("/action", url.Values{"name": {"wakeup"}}, cookie, true)
		h.stateSince(t, "idle", 10*time.Second)
	})

	h.check("broker restart: reconnect without replay", func() {
		h.command("before", "play", abort, in(time.Minute), false)
		expect(h.status("before"), "ok")
		h.stop("mosquitto", syscall.SIGTERM)
		h.stop("sub", syscall.SIGTERM)
		time.Sleep(time.Second)
		if h.publish(prefix+"/core/cmd", "{}", false) {
			h.fatalf("broker still up")
		}
		if code := h.healthz(); code != 503 {
			h.fatalf("healthz must fail without broker: %d", code)
		}
		if _, header := post("/action", url.Values{"name": {"play"}, "resource": {"system/abort.wav"}}, cookie, true); !strings.Contains(header.Get("Location"), "err=") {
			h.fatalf("UI must report the outage")
		}
		t := time.Now()
		h.startBroker()
		h.startSubReader()
		h.onlineSince(t, "online", 15*time.Second)
		h.waitFor(20*time.Second, "healthz back", func() bool { return h.healthz() == 200 })
		h.command("after", "play", abort, in(time.Minute), false)
		expect(h.status("after"), "ok")
		time.Sleep(1500 * time.Millisecond)
		for _, m := range h.objects(prefix+"/core/result", t) {
			if m["id"] == "before" || m["id"] == "p1" {
				h.fatalf("command replayed after reconnect: %v", m)
			}
		}
		// The service resynchronises (ears, infos) after reconnecting; nothing
		// else may be played: the play refused during the outage stays dropped.
		for _, m := range h.objects(prefix+"/core/state", t) {
			if p, _ := m["playing"].(string); p != "" && p != "after" {
				h.fatalf("command replayed after reconnect: %s", p)
			}
		}
	})
	h.check("core crash: LWT, restart and resync", func() {
		t := time.Now()
		h.stop("core", syscall.SIGKILL) // LWT path
		h.onlineSince(t, "offline", 15*time.Second)
		h.waitFor(10*time.Second, "healthz 503", func() bool { return h.healthz() == 503 })
		t = time.Now()
		h.startCore()
		h.onlineSince(t, "online", 15*time.Second)
		h.waitFor(15*time.Second, "healthz 200", func() bool { return h.healthz() == 200 })
		h.waitFor(10*time.Second, "service resync commands", func() bool {
			for _, m := range h.objects(prefix+"/core/result", t) {
				if id, _ := m["id"].(string); strings.HasPrefix(id, "svc-") {
					return true
				}
			}
			return false
		})
	})
}
