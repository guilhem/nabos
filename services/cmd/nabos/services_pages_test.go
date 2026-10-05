package main

import (
	"bytes"
	"github.com/guilhem/nabos/services/internal/hardware"
	"github.com/guilhem/nabos/services/internal/rabbit"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/guilhem/nabos/services/internal/config"
	"github.com/guilhem/nabos/services/internal/web"
)

func serviceSession(t *testing.T, a *App) *http.Cookie {
	t.Helper()
	a.auth.ArmPresence()
	// This helper creates an HTTP session; physical button admission is tested
	// through the authenticated D-Bus fixture in wifi_test.go.
	a.auth.MarkPresence(a.auth.MonoNow())
	token, err := a.auth.Setup("carotte-42")
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: web.CookieName, Value: token}
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
	a := testApp(t) // no engine or hardware: association must not issue rfid_write
	cookie := serviceSession(t, a)
	const uid = "d0:02:18:01:02:03:04:05"
	a.lastTag = &productTag{UID: uid, App: "radio", Support: "formatted", Locked: true}
	f := url.Values{"mode": {"associate"}, "kind": {"radio"}, "value": {"https://example.com/live.mp3"}}
	w := serviceRequest(a.routes(), "POST", "/tags/write", f, cookie)
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "ok=") {
		t.Fatalf("association: %d %s", w.Code, w.Header().Get("Location"))
	}
	reopened, err := config.Open(filepath.Join(a.env.DataDir, "application.json"))
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

func TestTagWriteUsesNativeBytesAndPersistsAssociation(t *testing.T) {
	a := testApp(t)
	native := startNative(t, a)
	cookie := serviceSession(t, a)
	tag := tagFromHardware(hardware.Tag{Tech: "st25tb", UID: []byte{0xd0, 2, 0x18, 1, 2, 3, 4, 5}, Support: "empty"})
	a.lastTag = &tag
	form := url.Values{"kind": {"radio"}, "picture": {"3"}, "value": {"https://example.com/live.mp3"}}
	w := serviceRequest(a.routes(), "POST", "/tags/write", form, cookie)
	if strings.Contains(w.Header().Get("Location"), "err=") {
		t.Fatal(w.Header())
	}
	native.mu.Lock()
	writes := append([]rabbit.TagWrite(nil), native.writes...)
	native.mu.Unlock()
	if len(writes) != 1 || writes[0].App != 12 || writes[0].Picture != 3 || string(writes[0].Data) != "DATA_IN_LOCAL_DB" || !bytes.Equal(writes[0].UID, tag.RawUID) {
		t.Fatal(writes)
	}
	if got := a.store.Get().Tags[tag.UID]; got.App != "radio" || got.Value != form.Get("value") {
		t.Fatal(got)
	}
}

func TestUploadedSoundIsReadableByMediaGroup(t *testing.T) {
	a := testApp(t)
	// Image setup supplies the shared setgid media tree. Application secrets
	// retain their own permissions; only a completed sound becomes group-readable.
	serviceSession(t, a)
	private := filepath.Join(a.env.DataDir, "application.json")
	for _, filename := range []string{"shared.wav", "shared.wav"} {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		file, err := form.CreateFormFile("file", filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte("RIFFxxxxWAVEaudio")); err != nil {
			t.Fatal(err)
		}
		if err := form.Close(); err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest("POST", "/sounds/upload", &body)
		request.Header.Set("Content-Type", form.FormDataContentType())
		a.uploadSound(httptest.NewRecorder(), request)
		info, err := os.Stat(filepath.Join(a.userSoundDir(), filename))
		if err != nil || info.Mode().Perm() != 0640 {
			t.Fatal("media permissions", info, err)
		}
	}
	info, err := os.Stat(private)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("private permissions", info, err)
	}
}
