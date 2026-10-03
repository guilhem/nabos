package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/guilhem/nabos/services/internal/clock"
	"github.com/guilhem/nabos/services/internal/config"
	"github.com/guilhem/nabos/services/internal/device"
	"github.com/guilhem/nabos/services/internal/devicetest"
	"github.com/guilhem/nabos/services/internal/rabbit"
)

func testApp(t *testing.T) *App {
	if wifiFixtures[os.Getenv("DBUS_SYSTEM_BUS_ADDRESS")] == nil {
		wifiTestBus(t)
	}
	a, err := NewApp(Env{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	installDeviceIdentity(t, a, appFixture(t, a).Conn)
	t.Cleanup(a.device.Close)
	return a
}
func appFixture(t *testing.T, a *App) *devicetest.Fixture {
	t.Helper()
	return wifiFixtures[os.Getenv("DBUS_SYSTEM_BUS_ADDRESS")].system
}
func TestClockUsesRemoteQuality(t *testing.T) {
	a := testApp(t)
	f := appFixture(t, a)
	for _, tc := range []struct {
		source  string
		quality clock.Quality
	}{{"unknown", clock.Unknown}, {"restored", clock.Coarse}, {"manual", clock.Exact}, {"ntp", clock.Exact}} {
		f.Mu.Lock()
		f.ClockQuality = tc.source
		f.Mu.Unlock()
		if quality, source := a.clockQuality(); quality != tc.quality || source != tc.source {
			t.Fatalf("snapshot %s: %v %s", tc.source, quality, source)
		}
	}
	f.Mu.Lock()
	f.ClockQuality = "restored"
	f.Mu.Unlock()
	if err := a.sayTime(); err == nil {
		t.Fatal("announced coarse clock")
	}
}

func TestRestartAppliesClockSchedule(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		hour         int
		perDay       bool
	}{
		{"night", "ntp", 2, false},
		{"restored time", "restored", 2, false},
		{"day", "ntp", 12, false},
		{"after midnight", "ntp", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testApp(t)
			now := time.Date(2026, 9, 29, tc.hour, 45, 0, 0, time.UTC)
			settings := a.store.Get().Clock
			settings.PerDay = tc.perDay
			settings.Days[0] = config.Day{Wakeup: config.HM{Hour: 8}, Sleep: config.HM{Hour: 1, Min: 30}}
			asleep := clock.ShouldSleep(settings, now)
			override := !asleep
			settings.Override = &override
			if _, err := a.store.Update(func(s *config.Settings) error { s.Clock = settings; return nil }); err != nil {
				t.Fatal(err)
			}
			f := appFixture(t, a)
			f.Mu.Lock()
			f.ClockQuality, f.ClockUnix, f.Settings.Timezone = tc.source, now.Unix(), "UTC"
			f.Mu.Unlock()
			restarted, err := NewApp(a.env)
			if err != nil {
				t.Fatal(err)
			}
			installDeviceIdentity(t, restarted, f.Conn)
			t.Cleanup(restarted.device.Close)
			if restarted.store.Get().Clock.Override != nil {
				t.Fatal("restart retained the previous manual override")
			}
			saved, err := config.Open(filepath.Join(a.env.DataDir, "application.json"))
			if err != nil || saved.Get().Clock.Override != nil {
				t.Fatal("restart did not persist the cleared override", err)
			}
			native := startNative(t, restarted)
			ctx, cancel := context.WithCancel(restarted.ctx)
			done := make(chan struct{})
			go func() { defer close(done); restarted.clockLoop(ctx) }()
			t.Cleanup(func() { cancel(); <-done })
			// Recovery already kicks the clock when hardware is ready.
			restarted.agent.holdRecovery()
			restarted.agent.recover(ctx)
			want := "idle"
			if asleep {
				want = "asleep"
			}
			pynabWait(t, "startup schedule", 3*time.Second, func() bool {
				state, _ := restarted.rabbit.State()
				return state.State == want
			})
			if native.length() != 0 {
				t.Fatal("startup played sleep or wakeup sounds")
			}
			// A new manual command still takes precedence during this boot.
			restarted.setOverride(!asleep)
			if asleep {
				want = "idle"
			} else {
				want = "asleep"
			}
			pynabWait(t, "new manual override", 3*time.Second, func() bool {
				state, _ := restarted.rabbit.State()
				return state.State == want
			})
		})
	}
}

func TestManualClockOverrideSurvivesTimeRecovery(t *testing.T) {
	a := testApp(t)
	f := appFixture(t, a)
	now := time.Date(2026, 9, 29, 2, 45, 0, 0, time.UTC)
	f.Mu.Lock()
	f.ClockUnix, f.Settings.Timezone = now.Unix(), "UTC"
	f.Mu.Unlock()
	for _, source := range []string{"unknown", "restored", "ntp"} {
		f.Mu.Lock()
		f.ClockQuality = source
		f.Mu.Unlock()
		if source == "unknown" {
			a.setOverride(false)
		}
		a.onState(rabbit.State{State: "idle"})
		a.clockTick(now)
		if override := a.store.Get().Clock.Override; override == nil || *override {
			t.Fatalf("%s: cleared a manual wakeup requested during this boot", source)
		}
	}
}

func TestSystemAndApplicationSettingsSaveIndependently(t *testing.T) {
	a := testApp(t)
	cookie := serviceSession(t, a)
	h := a.routes()
	revision, _, err := a.device.ReadConfig(a.ctx)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"revision": {revision}, "locale": {"en_US"}, "timezone": {"UTC"}, "volume": {"42"}, "voice": {"on"}}
	w := serviceRequest(h, "POST", "/settings/system", form, cookie)
	if strings.Contains(w.Header().Get("Location"), "err=") {
		t.Fatal(w.Header())
	}
	if _, err := os.Stat(filepath.Join(a.env.DataDir, "settings.json")); !os.IsNotExist(err) {
		t.Fatal("application wrote system file")
	}
	raw, _ := os.ReadFile(filepath.Join(a.env.DataDir, "application.json"))
	if strings.Contains(string(raw), "locale") || strings.Contains(string(raw), "volume") || strings.Contains(string(raw), "updates") {
		t.Fatal("mixed application settings", string(raw))
	}
	_, settings, err := a.device.ReadConfig(a.ctx)
	if err != nil || settings.Locale != "en_US" || settings.Volume != 42 || !settings.VoiceEnabled {
		t.Fatal("remote save", settings, err)
	}
	if w = serviceRequest(h, "POST", "/settings/system", form, cookie); !strings.Contains(w.Header().Get("Location"), "err=") {
		t.Fatal("stale revision accepted")
	}
}

func TestHomeAllowsProductStateWhileDeviceReadIsBlocked(t *testing.T) {
	a := testApp(t)
	revision, settings, err := a.device.ReadConfig(a.ctx)
	if err != nil {
		t.Fatal(err)
	}
	entered, resume := make(chan struct{}, 1), make(chan struct{})
	read := func() (string, device.Settings, *dbus.Error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-resume
		return revision, settings, nil
	}
	if err := appFixture(t, a).Conn.ExportMethodTable(map[string]interface{}{"Read": read}, device.Path("Config"), device.Interface("Config")); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	w := httptest.NewRecorder()
	go func() {
		a.home(w, httptest.NewRequest("GET", "/", nil))
		close(done)
	}()
	defer func() {
		close(resume)
		select {
		case <-done:
			if w.Code != http.StatusOK {
				t.Error("home did not complete", w.Code)
			}
		case <-time.After(5 * time.Second):
			t.Error("home did not resume after device reply")
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("home did not reach device read")
	}
	if !a.mu.TryLock() {
		t.Fatal("blocked device read holds the product state mutex")
	}
	a.mu.Unlock()
}

func TestEveryPageRenders(t *testing.T) {
	a := testApp(t)
	cookie := serviceSession(t, a)
	h := a.routes()
	for _, p := range []string{"/", "/settings", "/updates", "/tags", "/sounds"} {
		r := httptest.NewRequest("GET", p, nil)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "</html>") {
			t.Fatalf("%s: %d %s", p, w.Code, w.Body.String())
		}
	}
	for _, p := range []string{"/login", "/setup"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != 200 && w.Code != http.StatusSeeOther {
			t.Fatalf("%s: %d", p, w.Code)
		}
	}
}

func TestOldConfigIsNotMigrated(t *testing.T) {
	a := testApp(t)
	oldPath := filepath.Join(a.env.DataDir, "config.json")
	raw := []byte(`{"version":1,"locale":"en_US","admin":{"hash":"old-hash"}}`)
	if err := os.WriteFile(oldPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewApp(a.env)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.device.Close()
	if reopened.auth.Configured() {
		t.Fatal("old administrator migrated")
	}
	preserved, err := os.ReadFile(oldPath)
	if err != nil || string(preserved) != string(raw) {
		t.Fatal("old file changed", err)
	}
}

func TestHealthRequiresEngineHardwareAndDeviceOnly(t *testing.T) {
	a := testApp(t)
	check := func(remote string, want int) map[string]any {
		t.Helper()
		r := httptest.NewRequest("GET", "/healthz", nil)
		r.RemoteAddr = remote
		w := httptest.NewRecorder()
		a.healthz(w, r)
		if w.Code != want {
			t.Fatalf("health: %d %s", w.Code, w.Body.String())
		}
		var data map[string]any
		if want != 404 {
			if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
				t.Fatal(err)
			}
			if len(data) != 4 {
				t.Fatal("health shape", data)
			}
		}
		return data
	}
	data := check("127.0.0.1:1234", 503)
	if data["engine"] != false || data["device"] != true {
		t.Fatal(data)
	}
	startNative(t, a)
	data = check("127.0.0.1:1234", 200)
	if data["hardware"] != true || data["engine"] != true || a.ha.Connected() {
		t.Fatal("HA gates health", data)
	}
	check("192.0.2.1:1234", 404)
	f := appFixture(t, a)
	f.Mu.Lock()
	f.ManagerReady = false
	f.Mu.Unlock()
	data = check("[::1]:1234", 503)
	if data["device"] != false {
		t.Fatal(data)
	}
}

func TestVersionAndHelpReturnBeforeStartup(t *testing.T) {
	t.Setenv("DBUS_SYSTEM_BUS_ADDRESS", "unix:path=/missing/system/bus")
	data := filepath.Join(t.TempDir(), "missing")
	t.Setenv("NABOS_DATA_DIR", data)
	for _, arg := range []string{"--version", "--help"} {
		args, stdout := os.Args, os.Stdout
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Args, os.Stdout = []string{"nabos", arg}, w
		main()
		os.Args, os.Stdout = args, stdout
		w.Close()
		out, err := io.ReadAll(r)
		r.Close()
		if err != nil || !strings.Contains(string(out), "nabos") {
			t.Fatal(string(out), err)
		}
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Fatal("early option touched application data", err)
	}
}

func TestProductMediaPreservesAbsentAndEmptyAudio(t *testing.T) {
	a := testApp(t)
	f := startNative(t, a)
	root := a.env.SoundsDirs[0]
	path := filepath.Join(root, "taichi", "taichi.chor")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	// Wait 100 ms, then change LED 4; an explicit empty audio item cancels it.
	if err := os.WriteFile(path, []byte{0, 1, 1, 10, 7, 0, 1, 2, 3, 0, 0, 0, 255}, 0644); err != nil {
		t.Fatal(err)
	}
	colored := func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, v := range f.frames {
			if v.Red == 1 && v.Green == 2 && v.Blue == 3 {
				return true
			}
		}
		return false
	}
	command := sequence("", "taichi/taichi.chor")
	if err := a.media(a.ctx, command, time.Second); err != nil {
		t.Fatal(err)
	}
	if !colored() {
		t.Fatal("choreography-only item did not complete")
	}
	f.mu.Lock()
	f.frames = nil
	f.mu.Unlock()
	command.Sequence[0].Audio = []string{}
	if err := a.media(a.ctx, command, time.Second); err != nil {
		t.Fatal(err)
	}
	if colored() {
		t.Fatal("explicit empty audio behaved as absent audio")
	}
}
