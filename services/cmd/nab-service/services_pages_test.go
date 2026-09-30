package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/guilhem/nabos/services/internal/config"
	"github.com/guilhem/nabos/services/internal/network"
)

func serviceSession(t *testing.T, a *App) *http.Cookie {
	t.Helper()
	core := wifiTestBus(t)
	core.mu.Lock()
	core.status = network.Status{Mode: "client", Address: "192.0.2.10", Phase: "succeeded"}
	core.mu.Unlock()
	h := a.routes()
	if w := serviceRequest(h, "GET", "/setup", nil, nil); w.Code != http.StatusOK {
		t.Fatal("setup page", w.Code, w.Body.String())
	}
	freshDown(a)
	w := serviceRequest(h, "POST", "/setup", url.Values{"password": {"carotte-42"}, "confirm": {"carotte-42"}}, nil)
	if w.Header().Get("Location") != "/settings" || len(w.Result().Cookies()) != 1 {
		t.Fatal("setup after fresh press", w.Code, w.Header())
	}
	return w.Result().Cookies()[0]
}

func serviceRequest(h http.Handler, method, path string, values url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	var body *strings.Reader
	if values == nil {
		body = strings.NewReader("")
	} else {
		body = strings.NewReader(values.Encode())
	}
	r := httptest.NewRequest(method, path, body)
	if values != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "http://example.com")
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func validServiceForm() url.Values {
	return url.Values{
		"taichi_frequency": {"50"}, "surprise_frequency": {"125"},
		"air_index": {"pm25"}, "air_visual": {"alert"},
		"eightball": {"on"}, "books": {"on"}, "radio": {"on"},
		"webhooks": {"on"}, "ifttt": {"on"}, "air_enabled": {"on"},
	}
}

func TestServiceSettingsAuthenticationAndAtomicValidation(t *testing.T) {
	a := testApp(t)
	cookie := serviceSession(t, a)
	h := a.routes()
	form := validServiceForm()
	if w := serviceRequest(h, "GET", "/services", nil, nil); w.Code != http.StatusSeeOther {
		t.Fatalf("GET without session: %d", w.Code)
	}
	if w := serviceRequest(h, "POST", "/services/settings", form, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("POST without session: %d", w.Code)
	}
	if got := a.store.Get().Services.TaichiFrequency; got != 30 {
		t.Fatalf("unauthenticated save: %d", got)
	}

	form.Set("surprise_frequency", "31")
	w := serviceRequest(h, "POST", "/services/settings", form, cookie)
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "err=") {
		t.Fatalf("invalid form: %d %s", w.Code, w.Header().Get("Location"))
	}
	if got := a.store.Get().Services; got.TaichiFrequency != 30 || got.SurpriseFrequency != 30 {
		t.Fatalf("partial save: %+v", got)
	}
}

func TestServiceSettingsPreserveAndClearSecrets(t *testing.T) {
	a := testApp(t)
	cookie := serviceSession(t, a)
	next := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	_, err := a.store.Update(func(s *config.Settings) error {
		s.Services.IFTTTKey = "private-ifttt-key"
		s.Services.AirQuality.Token = "private-air-token"
		s.Services.NextTaichi, s.Services.NextSurprise = next, next
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	h := a.routes()
	form := validServiceForm()
	if w := serviceRequest(h, "POST", "/services/settings", form, cookie); w.Code != http.StatusSeeOther {
		t.Fatal(w.Code)
	}
	got := a.store.Get().Services
	if got.IFTTTKey != "private-ifttt-key" || got.AirQuality.Token != "private-air-token" {
		t.Fatal("blank secret replaced an existing value")
	}
	if !got.NextTaichi.IsZero() || !got.NextSurprise.IsZero() {
		t.Fatal("frequency change kept old deadlines")
	}
	page := serviceRequest(h, "GET", "/services", nil, cookie)
	if page.Code != http.StatusOK || strings.Contains(page.Body.String(), "private-ifttt-key") || strings.Contains(page.Body.String(), "private-air-token") {
		t.Fatalf("page exposes secrets or failed: %d", page.Code)
	}
	form.Set("clear_ifttt_key", "on")
	form.Set("clear_air_token", "on")
	if w := serviceRequest(h, "POST", "/services/settings", form, cookie); w.Code != http.StatusSeeOther {
		t.Fatal(w.Code)
	}
	got = a.store.Get().Services
	if got.IFTTTKey != "" || got.AirQuality.Token != "" {
		t.Fatal("explicit clear failed")
	}
}

func TestServiceAndTagPagesRenderCatalogueAndActions(t *testing.T) {
	a := testApp(t)
	cookie := serviceSession(t, a)
	root := t.TempDir()
	book := filepath.Join(root, "book", "books", "1234567890", "marie")
	if err := os.MkdirAll(book, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(book, "1.mp3"), []byte("chapter"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.env.SoundsDirs = []string{root}
	for _, path := range []string{"/services", "/tags"} {
		w := serviceRequest(a.routes(), "GET", path, nil, cookie)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "1234567890") || !strings.Contains(w.Body.String(), "</html>") {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	services := serviceRequest(a.routes(), "GET", "/services", nil, cookie).Body.String()
	for _, action := range []string{"taichi", "surprise", "eightball", "airquality", "book", "radio", "webhook", "ifttt", "/services/mastodon/connect"} {
		if !strings.Contains(services, action) {
			t.Errorf("missing %s", action)
		}
	}
	form := url.Values{"name": {"webhook"}, "value": {"https://example.org/"}}
	w := serviceRequest(a.routes(), "POST", "/services/action", form, cookie)
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "err=") {
		t.Fatalf("unassociated URL should be refused: %d %s", w.Code, w.Header().Get("Location"))
	}
}

func TestAssociateLegacyLockedTagWithoutWritingHardware(t *testing.T) {
	a := testApp(t) // no broker or hardware: association must not issue rfid_write
	cookie := serviceSession(t, a)
	const uid = "d0:02:18:01:02:03:04:05"
	a.lastTag = map[string]any{"uid": uid, "app": "radio", "support": "formatted", "locked": true}
	f := url.Values{"mode": {"associate"}, "kind": {"radio"}, "value": {"https://example.com/live.mp3"}}
	w := serviceRequest(a.routes(), "POST", "/tags/write", f, cookie)
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "ok=") {
		t.Fatalf("association: %d %s", w.Code, w.Header().Get("Location"))
	}
	reopened, err := config.Open(filepath.Join(a.env.DataDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Get().Tags[uid]; got.App != "radio" || got.Value != f.Get("value") {
		t.Fatal("association lost", got)
	}
	f.Set("kind", "webhook")
	w = serviceRequest(a.routes(), "POST", "/tags/write", f, cookie)
	if !strings.Contains(w.Header().Get("Location"), "err=") || a.store.Get().Tags[uid].App != "radio" {
		t.Fatal("mismatched app changed an old tag")
	}
}
