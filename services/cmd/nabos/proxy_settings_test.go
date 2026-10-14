package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/guilhem/nabos/services/internal/config"
	"github.com/guilhem/nabos/services/internal/web"
)

func TestProxySettingsProtectedPost(t *testing.T) {
	a := testApp(t)
	cookie := serviceSession(t, a)
	h := a.routes()
	for _, tc := range []struct {
		name   string
		modify func(*http.Request)
		want   int
	}{
		{"no session", func(r *http.Request) { r.Header.Del("Cookie") }, http.StatusUnauthorized},
		{"invalid session", func(r *http.Request) { r.Header.Set("Cookie", web.CookieName+"=wrong") }, http.StatusUnauthorized},
		{"missing proof", func(r *http.Request) { r.Header.Del("Origin") }, http.StatusForbidden},
		{"wrong origin", func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") }, http.StatusForbidden},
		{"wrong scheme", func(r *http.Request) { r.Header.Set("Origin", "https://example.com") }, http.StatusForbidden},
		{"wrong referer", func(r *http.Request) { r.Header.Del("Origin"); r.Header.Set("Referer", "http://evil.example/settings") }, http.StatusForbidden},
		{"empty origin beats referer", func(r *http.Request) {
			r.Header.Set("Origin", "")
			r.Header.Set("Referer", "http://example.com/settings")
		}, http.StatusForbidden},
		{"duplicate origin", func(r *http.Request) { r.Header.Add("Origin", "http://example.com") }, http.StatusForbidden},
		{"duplicate referer", func(r *http.Request) {
			r.Header.Del("Origin")
			r.Header["Referer"] = []string{"http://example.com/settings", "http://example.com/settings"}
		}, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/settings/proxy", strings.NewReader("trusted_proxies=192.0.2.10"))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Origin", "http://example.com")
			r.AddCookie(cookie)
			tc.modify(r)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("HTTP %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
			if _, err := os.Stat(filepath.Join(a.env.DataDir, "trusted-proxies")); !os.IsNotExist(err) {
				t.Fatal("refused request wrote policy")
			}
		})
	}
}

func TestProxySettingsRejectsMalformedForm(t *testing.T) {
	a := testApp(t)
	cookie := serviceSession(t, a)
	trustIngress(t, a)
	h := a.routes()
	for _, tc := range []struct{ name, path, body string }{
		{"missing", "/settings/proxy", ""},
		{"unknown", "/settings/proxy", "proxy=192.0.2.11"},
		{"duplicate", "/settings/proxy", "trusted_proxies=&trusted_proxies=192.0.2.11"},
		{"other setting", "/settings/proxy", "trusted_proxies=&ha_host=evil.example"},
		{"query only", "/settings/proxy?trusted_proxies=192.0.2.11", ""},
		{"malformed", "/settings/proxy", "trusted_proxies=%zz"},
		{"field limit", "/settings/proxy", "trusted_proxies=" + strings.Repeat("a", web.MaxProxyAddresses+1)},
		{"body limit", "/settings/proxy", "trusted_proxies=" + strings.Repeat("a", 16<<10)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			r.Header.Set("Origin", "http://example.com")
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.AddCookie(cookie)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("malformed form: %d %s", w.Code, w.Body.String())
			}
			if value, err := a.proxy.Current(); value != "192.0.2.10" || err != nil {
				t.Fatal("malformed form changed active policy")
			}
		})
	}
	w := serviceRequest(h, http.MethodPost, "/settings/proxy", url.Values{"trusted_proxies": {"192.0.2.11,bad"}}, cookie)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "configuration non enregistrée") {
		t.Fatalf("invalid IP not reported: %d %s", w.Code, w.Body.String())
	}
	if raw, err := os.ReadFile(filepath.Join(a.env.DataDir, "trusted-proxies")); err != nil || string(raw) != "192.0.2.10" {
		t.Fatal("invalid IP changed saved policy")
	}
}

func TestProxySettingsApplyAndRevokeIngress(t *testing.T) {
	a := testApp(t)
	cookie := serviceSession(t, a)
	if _, err := a.store.Update(func(s *config.Settings) error {
		s.Ears = [2]int{4, 5}
		s.Weather.Location = "Paris"
		s.HomeAssistant.Host = "broker.local"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	appPath := filepath.Join(a.env.DataDir, "application.json")
	before, err := os.ReadFile(appPath)
	if err != nil {
		t.Fatal(err)
	}
	settings := a.store.Get()
	h := a.routes()
	if w := ingressRequest(h, http.MethodPost, "/login", url.Values{"password": {"carotte-42"}}); w.Code != http.StatusForbidden {
		t.Fatal("missing file trusted ingress")
	}
	w := serviceRequest(h, http.MethodPost, "/settings/proxy", url.Values{"trusted_proxies": {"192.0.2.10"}}, cookie)
	doc := uiDocument(t, w)
	if w.Header().Get("Location") != "" || !strings.Contains(w.Body.String(), "enregistré et appliqué") {
		t.Fatal("save must render confirmation without redirect")
	}
	checkUIProxyCard(t, doc, "", "192.0.2.10")
	login := ingressRequest(h, http.MethodPost, "/login", url.Values{"password": {"carotte-42"}})
	if login.Code != http.StatusSeeOther || login.Header().Get("Location") != ingressPath+"/" {
		t.Fatal("already built handler did not authorize saved ingress")
	}
	proxyCookie := login.Result().Cookies()[0]
	w = ingressRequest(h, http.MethodPost, "/settings/proxy", url.Values{"trusted_proxies": {""}}, proxyCookie)
	doc = uiDocument(t, w)
	if w.Header().Get("Location") != "" || !strings.Contains(w.Body.String(), "enregistré et appliqué") {
		t.Fatal("revoking own proxy must render confirmation without redirect")
	}
	checkUINavigation(t, doc, ingressPath+"/settings")
	checkUIProxyCard(t, doc, ingressPath, "") // Response retains this request's original snapshot.
	if w := ingressRequest(h, http.MethodPost, "/settings/proxy", url.Values{"trusted_proxies": {"192.0.2.10"}}, proxyCookie); w.Code != http.StatusForbidden {
		t.Fatal("subsequent request retained revoked ingress trust")
	}
	if w := ingressRequest(h, http.MethodGet, "/", nil); w.Header().Get("Location") != "/login" {
		t.Fatal("subsequent request retained revoked prefix")
	}
	// Direct LAN recovery also accepts a strict, same-origin Referer without Origin.
	r := httptest.NewRequest(http.MethodPost, "/settings/proxy", strings.NewReader("trusted_proxies=192.0.2.10"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Referer", "http://example.com/settings")
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	uiDocument(t, w)
	if w := ingressRequest(h, http.MethodGet, "/", nil); w.Header().Get("Location") != ingressPath+"/login" {
		t.Fatal("direct recovery did not reauthorize ingress")
	}
	after, err := os.ReadFile(appPath)
	if err != nil || !bytes.Equal(before, after) || !reflect.DeepEqual(settings, a.store.Get()) {
		t.Fatal("proxy saves changed application settings or credentials")
	}
	// Freeze the old top-level schema: rollback decoders reject unknown fields.
	var oldSettings struct {
		Version       int             `json:"version"`
		Ears          json.RawMessage `json:"ears"`
		Admin         json.RawMessage `json:"admin"`
		Clock         json.RawMessage `json:"clock"`
		Weather       json.RawMessage `json:"weather"`
		HomeAssistant json.RawMessage `json:"home_assistant"`
		Services      json.RawMessage `json:"services"`
		Tags          json.RawMessage `json:"tags"`
		Mastodon      json.RawMessage `json:"mastodon"`
	}
	decoder := json.NewDecoder(bytes.NewReader(after))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&oldSettings); err != nil {
		t.Fatal("old schema rejected application settings", err)
	}
	reopened, err := config.Open(appPath)
	if err != nil || reopened.Recovered != "" || !reflect.DeepEqual(reopened.Get(), settings) {
		t.Fatal("proxy save broke application reload or rollback", err)
	}
}

func TestProxySettingsInvalidLoadPreservesAdmin(t *testing.T) {
	a := testApp(t)
	serviceSession(t, a)
	before := a.store.Get()
	path := filepath.Join(a.env.DataDir, "trusted-proxies")
	if err := os.WriteFile(path, []byte("192.0.2.10,bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewApp(a.env)
	if err != nil {
		t.Fatal("invalid policy prevented startup", err)
	}
	t.Cleanup(reloaded.device.Close)
	installDeviceIdentity(t, reloaded, appFixture(t, a).Conn)
	if reloaded.store.Recovered != "" || !reflect.DeepEqual(before, reloaded.store.Get()) {
		t.Fatal("invalid policy reset admin or other settings")
	}
	token, err := reloaded.auth.Login("carotte-42")
	if err != nil {
		t.Fatal("invalid policy damaged admin password", err)
	}
	cookie := &http.Cookie{Name: web.CookieName, Value: token}
	h := reloaded.routes()
	w := serviceRequest(h, http.MethodGet, "/settings", nil, cookie)
	uiDocument(t, w)
	if !strings.Contains(w.Body.String(), "impossible de charger la configuration") {
		t.Fatal("load error is not visible in local Settings")
	}
	if w := ingressRequest(h, http.MethodPost, "/login", url.Values{"password": {"carotte-42"}}); w.Code != http.StatusForbidden {
		t.Fatal("invalid file partially authorized ingress")
	}
	w = serviceRequest(h, http.MethodPost, "/settings/proxy", url.Values{"trusted_proxies": {"192.0.2.10"}}, cookie)
	uiDocument(t, w)
	if strings.Contains(w.Body.String(), "impossible de charger la configuration") {
		t.Fatal("UI repair retained load error")
	}
}
