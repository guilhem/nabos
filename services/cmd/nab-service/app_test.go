package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guilhem/nabos/services/internal/clock"
	"github.com/guilhem/nabos/services/internal/devicetest"
)

func testApp(t *testing.T) *App {
	if wifiFixtures[os.Getenv("DBUS_SYSTEM_BUS_ADDRESS")] == nil {
		wifiTestBus(t)
	}
	a, err := NewApp(Env{MQTTHost: "127.0.0.1", MQTTPort: 1, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
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
