// Package integration exercises the real Go application, Rust hardware daemon
// and pinned device-core on a private bus. No local MQTT broker is involved.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/guilhem/nabos/services/internal/busidentity"
	"github.com/guilhem/nabos/services/internal/config"
	"github.com/guilhem/nabos/services/internal/device"
	"golang.org/x/sys/unix"
)

var repo, _ = filepath.Abs("../../..")

const hardwareName = "io.github.guilhem.NabHardware1"
const hardwarePath = dbus.ObjectPath("/io/github/guilhem/NabHardware1")

type harness struct {
	t                     *testing.T
	tmp                   string
	httpPort              int
	haPort                int
	procs                 map[string]*exec.Cmd
	requireAgents         bool
	simAudioMS            int
	hardware, app, device []string
	deviceAPI             *device.Client
	bus                   *dbus.Conn
	accounts              map[string]*syscall.Credential
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func which(t *testing.T, name string, extra ...string) string {
	t.Helper()
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

func commandLine(value string) []string {
	if _, err := os.Stat(value); err == nil {
		return []string{value}
	}
	return strings.Fields(value)
}

func (h *harness) fatalf(format string, args ...any) {
	h.t.Helper()
	h.t.Fatalf(format, args...)
}

func (h *harness) check(name string, fn func(*testing.T)) {
	parent := h.t
	parent.Run(name, func(t *testing.T) {
		h.t = t
		defer func() { h.t = parent }()
		fn(t)
	})
}

func (h *harness) build() {
	hw := os.Getenv("NABOS_HARDWARE_BIN")
	if hw == "" {
		hw = filepath.Join(repo, "core/target/debug/nab-hardware")
	}
	app := os.Getenv("NABOS_BIN")
	if app == "" {
		app = filepath.Join(h.tmp, "nabos")
		cmd := exec.Command(which(h.t, "go"), "build", "-o", app, "./cmd/nabos")
		cmd.Dir = filepath.Join(repo, "services")
		if out, err := cmd.CombinedOutput(); err != nil {
			h.fatalf("go build: %v\n%s", err, out)
		}
	}
	deviceBin := os.Getenv("DEVICE_CORE_BIN")
	if deviceBin == "" {
		deviceBin = filepath.Join(repo, "build/cargo/device-core-tests/debug/device-core")
	}
	h.hardware, h.app, h.device = commandLine(hw), commandLine(app), commandLine(deviceBin)
	for _, c := range [][]string{h.hardware, h.app, h.device} {
		out, err := exec.Command(c[0], append(c[1:], "--version")...).Output()
		if err != nil {
			h.fatalf("%v --version: %v", c, err)
		}
		h.t.Logf("using %s: %s", strings.Join(c, " "), strings.TrimSpace(string(out)))
	}
}

// Only the integration launcher runs as root; product daemons drop to their
// actual dedicated accounts before exec, with no supplementary host groups.
func (h *harness) serviceAccounts() {
	h.t.Helper()
	if os.Geteuid() != 0 {
		h.fatalf("TestEndToEnd requires euid 0 and dedicated accounts in a private mount/network namespace")
	}
	h.accounts = make(map[string]*syscall.Credential)
	seen := map[uint32]bool{0: true}
	for _, service := range []struct{ role, environment, fallback string }{
		{"app", "NABOS_APP_USER", "nab-app"},
		{"hardware", "NABOS_HARDWARE_USER", "nab-hardware"},
		{"device", "NABOS_DEVICE_USER", "device-core"},
	} {
		account, err := user.Lookup(busidentity.ExpectedUser(service.environment, service.fallback))
		if err != nil {
			h.fatalf("%s account: %v", service.role, err)
		}
		uid, uidErr := strconv.ParseUint(account.Uid, 10, 32)
		gid, gidErr := strconv.ParseUint(account.Gid, 10, 32)
		if uidErr != nil || gidErr != nil || seen[uint32(uid)] {
			h.fatalf("%s needs a distinct nonroot UID", service.role)
		}
		seen[uint32(uid)] = true
		h.accounts[service.role] = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	}
	if err := os.Chmod(h.tmp, 0755); err != nil {
		h.fatalf("%v", err)
	}
}

func (h *harness) own(role, path string) {
	if account := h.accounts[role]; account != nil {
		if err := os.Chown(path, int(account.Uid), int(account.Gid)); err != nil {
			h.fatalf("%s ownership: %v", path, err)
		}
	}
}

func (h *harness) directory(role, path string) {
	if err := os.MkdirAll(path, 0700); err != nil {
		h.fatalf("%v", err)
	}
	h.own(role, path)
}

func (h *harness) spawn(name string, args []string, env ...string) {
	logf, err := os.OpenFile(filepath.Join(h.tmp, name+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		h.fatalf("%v", err)
	}
	cmd := exec.Command(args[0], args[1:]...)
	if account := h.accounts[name]; account != nil {
		runtime := filepath.Join(h.tmp, name+"-run")
		h.directory(name, runtime)
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: account}
		cmd.Dir = runtime
		env = append(env, "HOME="+runtime, "XDG_RUNTIME_DIR="+runtime)
		h.own(name, logf.Name())
	}
	cmd.Env = append(os.Environ(), env...)
	if cmd.Dir == "" {
		cmd.Dir = h.tmp
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	err = cmd.Start()
	logf.Close()
	if err != nil {
		h.fatalf("%s: %v", name, err)
	}
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

func (h *harness) waitFor(timeout time.Duration, what string, cond func() bool) {
	h.t.Helper()
	for end := time.Now().Add(timeout); time.Now().Before(end); time.Sleep(25 * time.Millisecond) {
		if cond() {
			return
		}
	}
	h.fatalf("timeout waiting for %s", what)
}

func portOpen(port int) bool {
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 200*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func (h *harness) startHardware() {
	h.spawn("hardware", append(h.hardware, "--simulate"),
		"NABOS_DEVICE_BUS_ADDRESS="+os.Getenv("DBUS_SYSTEM_BUS_ADDRESS"), "NABOS_LOG=debug")
}

func (h *harness) startApp() {
	data := filepath.Join(h.tmp, "data")
	h.directory("app", data)
	cfg := filepath.Join(data, "application.json")
	if _, err := os.Stat(cfg); os.IsNotExist(err) {
		st := config.Defaults()
		st.Clock.Chime, st.Clock.SleepSounds = false, false
		st.Clock.Wakeup, st.Clock.Sleep = config.HM{}, config.HM{}
		for i := range st.Clock.Days {
			st.Clock.Days[i] = config.Day{}
		}
		st.Services.TaichiFrequency, st.Services.SurpriseFrequency = 0, 0
		// Home Assistant being unavailable must not prevent a healthy boot.
		st.HomeAssistant.Enabled = true
		st.HomeAssistant.Host, st.HomeAssistant.Port = "127.0.0.1", h.haPort
		raw, err := json.Marshal(st)
		if err != nil {
			h.fatalf("%v", err)
		}
		if err := os.WriteFile(cfg, raw, 0o600); err != nil {
			h.fatalf("%v", err)
		}
	}
	h.own("app", cfg)
	assets := os.Getenv("NABOS_TEST_ASSETS")
	if assets == "" {
		assets = filepath.Join(repo, "assets")
	}
	h.spawn("app", h.app, "NABOS_HTTP_ADDR=127.0.0.1:"+strconv.Itoa(h.httpPort),
		"NABOS_DATA_DIR="+data, "NABOS_SOUNDS_DIRS="+filepath.Join(assets, "sounds"),
		"NABOS_CHOREOGRAPHIES_DIRS="+filepath.Join(assets, "choreographies"),
		"NABOS_WEATHER_URL=http://127.0.0.1:9/forecast", "NABOS_GEOCODING_URL=http://127.0.0.1:9/search",
		"NABOS_LOG=debug", "NABOS_VERSION=v0.0.1")
	h.waitFor(20*time.Second, "app listening", func() bool { return portOpen(h.httpPort) })
}

var client = &http.Client{Timeout: 90 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (h *harness) http(method, path string, form url.Values, cookie string, origin bool) (int, http.Header, string) {
	h.t.Helper()
	base := fmt.Sprintf("http://127.0.0.1:%d", h.httpPort)
	req, err := http.NewRequest(method, base+path, strings.NewReader(form.Encode()))
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
		req.Header.Set("Origin", base)
	}
	resp, err := client.Do(req)
	if err != nil {
		h.fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		h.fatalf("%v", err)
	}
	return resp.StatusCode, resp.Header, string(raw)
}

func (h *harness) healthz() int {
	code, _, _ := h.http(http.MethodGet, "/healthz", nil, "", true)
	return code
}

func monotonic(t *testing.T) uint64 {
	t.Helper()
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		t.Fatal(err)
	}
	return uint64(ts.Nano())
}

func (h *harness) button(gesture string, edge uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	call := h.bus.Object(hardwareName, hardwarePath).CallWithContext(ctx, hardwareName+".Simulation.Button", 0, gesture, edge)
	if call.Err != nil {
		h.fatalf("button injection: %v", call.Err)
	}
}

func (h *harness) cleanup() {
	for _, name := range []string{"app", "hardware", "device", "ha-broker", "dbus"} {
		h.stop(name, syscall.SIGTERM)
	}
	// Collect volatile logs before removing a successful test's disposable data.
	if h.t.Failed() {
		for _, name := range []string{"app", "hardware", "device", "ha-broker"} {
			if raw, err := os.ReadFile(filepath.Join(h.tmp, name+".log")); err == nil {
				lines := strings.Split(string(raw), "\n")
				h.t.Logf("%s.log\n%s", name, strings.Join(lines[max(0, len(lines)-60):], "\n"))
			}
		}
		h.t.Logf("diagnostics retained at %s", h.tmp)
	} else {
		os.RemoveAll(h.tmp)
	}
}

func TestEndToEnd(t *testing.T) {
	if os.Getenv("NABOS_INTEGRATION") != "1" {
		t.Skip("set NABOS_INTEGRATION=1")
	}

	tmp, err := os.MkdirTemp("", "nabos-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, tmp: tmp, procs: map[string]*exec.Cmd{}, requireAgents: true}
	defer h.cleanup()
	h.serviceAccounts()
	h.httpPort, h.haPort = freePort(t), freePort(t)
	h.build()
	h.startDeviceCore(t)
	h.bus, err = dbus.ConnectSystemBus()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.bus.Close() })
	h.startHardware()
	h.waitFor(10*time.Second, "hardware D-Bus readiness", func() bool {
		var ready dbus.Variant
		err := h.bus.Object(hardwareName, hardwarePath).Call("org.freedesktop.DBus.Properties.Get", 0, hardwareName, "Ready").Store(&ready)
		return err == nil && ready.Value() == true
	})
	var status dbus.Variant
	if err := h.bus.Object(hardwareName, hardwarePath).Call("org.freedesktop.DBus.Properties.Get", 0, hardwareName, "Status").Store(&status); err != nil {
		t.Fatal("hardware snapshot", err)
	}
	t.Logf("hardware Status %s: %v", status.Signature(), status.Value())
	h.startApp()
	h.waitFor(25*time.Second, "healthy without Home Assistant or local broker", func() bool { return h.healthz() == 200 })

	h.check("physical button timestamp remains uint64", func(t *testing.T) {
		match := []dbus.MatchOption{dbus.WithMatchObjectPath(hardwarePath), dbus.WithMatchInterface(hardwareName), dbus.WithMatchMember("Button")}
		if err := h.bus.AddMatchSignal(match...); err != nil {
			t.Fatal(err)
		}
		defer h.bus.RemoveMatchSignal(match...)
		signals := make(chan *dbus.Signal, 8)
		h.bus.Signal(signals)
		defer h.bus.RemoveSignal(signals)
		edge := monotonic(t)
		h.button("down", edge)
		select {
		case signal := <-signals:
			var gesture string
			var got uint64
			if err := dbus.Store(signal.Body, &gesture, &got); err != nil || gesture != "down" || got != edge {
				t.Fatal("physical timestamp changed on D-Bus", signal.Body, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("missing hardware button event")
		}
	})

	h.check("hardware rejects an untrusted account", func(t *testing.T) {
		if err := h.bus.Object(hardwareName, hardwarePath).Call(hardwareName+".Claim", 0).Err; err == nil {
			t.Fatal("test process obtained hardware control")
		}
		cmd := exec.Command(which(t, "dbus-send"), "--bus="+os.Getenv("DBUS_SYSTEM_BUS_ADDRESS"), "--print-reply", "--reply-timeout=3000",
			"--dest="+device.Destination, string(device.Path("Network")), device.Interface("Network")+".ReportPresence", "uint64:"+strconv.FormatUint(monotonic(t), 10))
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: h.accounts["app"]}
		output, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(output), "org.freedesktop.DBus.Error.AccessDenied") {
			t.Fatal("application account presence rejection", err, string(output))
		}
	})
	password := url.Values{"password": {"carotte-42"}, "confirm": {"carotte-42"}}
	oldEdge := monotonic(t)
	h.button("down", oldEdge)
	_, _, _ = h.http(http.MethodGet, "/setup", nil, "", true)
	for _, ev := range []struct {
		gesture string
		edge    uint64
	}{
		{"down", oldEdge}, {"down", 0}, {"down", monotonic(t) + uint64(time.Hour)},
		{"up", 0}, {"hold", 0},
	} {
		h.button(ev.gesture, ev.edge)
	}
	time.Sleep(100 * time.Millisecond)
	if _, header, _ := h.http(http.MethodPost, "/setup", password, "", true); !strings.Contains(header.Get("Location"), "err=") {
		t.Fatal("setup accepted stale, future or non-down presence")
	}
	h.button("down", monotonic(t))
	h.waitFor(3*time.Second, "fresh physical presence", func() bool {
		_, _, body := h.http(http.MethodGet, "/setup", nil, "", true)
		return strings.Contains(body, "carotte") || strings.Contains(body, "password")
	})
	time.Sleep(100 * time.Millisecond)
	code, header, _ := h.http(http.MethodPost, "/setup", password, "", true)
	if code != 303 || header.Get("Location") != "/settings" {
		t.Fatalf("setup: %d %v", code, header)
	}
	cookie, _, _ := strings.Cut(header.Get("Set-Cookie"), ";")
	action := func(name string, extra url.Values) (int, http.Header, string) {
		if extra == nil {
			extra = url.Values{}
		}
		extra.Set("name", name)
		return h.http(http.MethodPost, "/action", extra, cookie, true)
	}
	h.check("HTTP authentication and CSRF", func(t *testing.T) {
		if code, _, _ := h.http(http.MethodPost, "/action", url.Values{"name": {"play"}}, "", true); code != 401 {
			t.Fatal(code)
		}
		if code, _, _ := h.http(http.MethodPost, "/action", url.Values{"name": {"play"}}, cookie, false); code != 403 {
			t.Fatal(code)
		}
	})
	h.check("playback and boundary validation", func(t *testing.T) {
		if code, header, _ := action("play", url.Values{"resource": {"system/abort.wav"}}); code != 303 || !strings.Contains(header.Get("Location"), "ok=") {
			t.Fatalf("play: %d %v", code, header)
		}
		if _, header, _ := action("play", url.Values{"resource": {"../../etc/passwd"}}); !strings.Contains(header.Get("Location"), "err=") {
			t.Fatal("resource escaped media root")
		}
	})
	h.check("sleep and wake up", func(t *testing.T) {
		action("sleep", nil)
		h.waitFor(5*time.Second, "sleep", func() bool {
			_, _, body := h.http(http.MethodGet, "/", nil, cookie, true)
			return strings.Contains(body, "endormi")
		})
		action("wakeup", nil)
		h.waitFor(5*time.Second, "wake", func() bool {
			_, _, body := h.http(http.MethodGet, "/", nil, cookie, true)
			return strings.Contains(body, "réveillé")
		})
	})
	h.check("Home Assistant uses its own broker and never gates health", func(t *testing.T) {
		broker := os.Getenv("MOSQUITTO")
		if broker == "" {
			broker = which(t, "mosquitto", "/usr/sbin/mosquitto")
		}
		// A disposable executable avoids the distro AppArmor profile for the
		// production broker and its restricted configuration paths.
		raw, err := os.ReadFile(broker)
		if err != nil {
			t.Fatal(err)
		}
		isolated := filepath.Join(h.tmp, "ha-mosquitto")
		if err := os.WriteFile(isolated, raw, 0o700); err != nil {
			t.Fatal(err)
		}
		cfg := filepath.Join(h.tmp, "ha-mosquitto.conf")
		if err := os.WriteFile(cfg, fmt.Appendf(nil, "listener %d 127.0.0.1\nallow_anonymous true\npersistence false\n", h.haPort), 0o600); err != nil {
			t.Fatal(err)
		}
		h.spawn("ha-broker", []string{isolated, "-c", cfg})
		h.waitFor(5*time.Second, "Home Assistant broker", func() bool { return portOpen(h.haPort) })
		mqtt := func(binary string, args ...string) string {
			cmd := exec.Command(which(t, binary), append([]string{"-h", "127.0.0.1", "-p", strconv.Itoa(h.haPort), "-V", "mqttv5"}, args...)...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s: %v\n%s", binary, err, out)
			}
			return strings.TrimSpace(string(out))
		}
		discovery := mqtt("mosquitto_sub", "-t", "homeassistant/switch/+/sleep/config", "-C", "1", "-W", "15")
		var entity struct {
			CommandTopic string `json:"command_topic"`
			StateTopic   string `json:"state_topic"`
		}
		if err := json.Unmarshal([]byte(discovery), &entity); err != nil || !strings.HasSuffix(entity.CommandTopic, "/sleep/set") {
			t.Fatal("retained Home Assistant discovery", discovery, err)
		}
		mqtt("mosquitto_pub", "-t", entity.CommandTopic, "-m", "ON", "-q", "1")
		h.waitFor(5*time.Second, "Home Assistant sleep command", func() bool {
			_, _, body := h.http(http.MethodGet, "/", nil, cookie, true)
			return strings.Contains(body, "endormi")
		})
		mqtt("mosquitto_pub", "-t", entity.CommandTopic, "-m", "OFF", "-q", "1")
		h.waitFor(5*time.Second, "Home Assistant wake command", func() bool {
			_, _, body := h.http(http.MethodGet, "/", nil, cookie, true)
			return strings.Contains(body, "réveillé")
		})
		h.waitFor(5*time.Second, "retained Home Assistant state", func() bool {
			state := mqtt("mosquitto_sub", "-t", entity.StateTopic, "-C", "1", "-W", "5")
			var retained struct {
				State string `json:"state"`
			}
			return json.Unmarshal([]byte(state), &retained) == nil && retained.State == "idle"
		})
		h.stop("ha-broker", syscall.SIGTERM)
		if h.healthz() != 200 {
			t.Fatal("Home Assistant outage blocks healthy boot")
		}
		if _, header, _ := action("play", url.Values{"resource": {"system/abort.wav"}}); !strings.Contains(header.Get("Location"), "ok=") {
			t.Fatal("Home Assistant outage prevents local playback", header)
		}
	})
	h.check("hardware death blocks health and refuses outage playback", func(t *testing.T) {
		h.stop("hardware", syscall.SIGKILL)
		h.waitFor(5*time.Second, "hardware outage health", func() bool { return h.healthz() == 503 })
		if _, header, _ := action("play", url.Values{"resource": {"system/abort.wav"}}); !strings.Contains(header.Get("Location"), "err=") {
			t.Fatal("play accepted during hardware outage")
		}
		h.startHardware()
		h.waitFor(25*time.Second, "hardware recovery", func() bool { return h.healthz() == 200 })
	})
	h.check("system daemon restart and durable configuration", func(t *testing.T) {
		revision, settings, err := h.deviceAPI.ReadConfig(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		settings.Volume = 42
		if _, err := h.deviceAPI.UpdateConfig(context.Background(), revision, settings); err != nil {
			t.Fatal(err)
		}
		h.stop("device", syscall.SIGTERM)
		h.waitFor(5*time.Second, "device outage health", func() bool { return h.healthz() == 503 })
		h.startDeviceDaemon()
		h.waitFor(25*time.Second, "device recovery", func() bool { return h.healthz() == 200 })
		_, got, err := h.deviceAPI.ReadConfig(context.Background())
		if err != nil || got.Volume != 42 {
			t.Fatal(got, err)
		}
		if _, header, _ := action("play", url.Values{"resource": {"system/abort.wav"}}); !strings.Contains(header.Get("Location"), "ok=") {
			t.Fatal(header)
		}
	})
	h.check("application restart retains administrator", func(t *testing.T) {
		h.stop("app", syscall.SIGKILL)
		h.startApp()
		h.waitFor(25*time.Second, "app recovery", func() bool { return h.healthz() == 200 })
		code, header, _ := h.http(http.MethodGet, "/", nil, "", true)
		if code != 303 || header.Get("Location") != "/login" {
			t.Fatal(code, header)
		}
		raw, err := os.ReadFile(filepath.Join(tmp, "data/application.json"))
		if err != nil || !strings.Contains(string(raw), "hash") || strings.Contains(string(raw), "carotte-42") {
			t.Fatal("administrator storage", err)
		}
	})
}
