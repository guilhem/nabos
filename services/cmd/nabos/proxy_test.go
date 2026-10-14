package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/guilhem/nabos/services/internal/config"
	"github.com/guilhem/nabos/services/internal/device"
	"github.com/guilhem/nabos/services/internal/network"
	"github.com/guilhem/nabos/services/internal/web"
)

const ingressPath = "/api/ingress/nabaztag"
const ingressOrigin = "https://amand.lettron.fr"

func ingressHTTPRequest(method, path string, values url.Values, cookies ...*http.Cookie) *http.Request {
	r := httptest.NewRequest(method, "http://amand.lettron.fr"+path, strings.NewReader(values.Encode()))
	r.RemoteAddr = "192.0.2.10:4321"
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Ingress-Path", ingressPath)
	if values != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", ingressOrigin)
	}
	for _, cookie := range cookies {
		if cookie != nil {
			r.AddCookie(cookie)
		}
	}
	return r
}

func ingressRequest(h http.Handler, method, path string, values url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, ingressHTTPRequest(method, path, values, cookies...))
	return w
}

func trustIngress(t *testing.T, a *App) {
	t.Helper()
	proxy, err := web.NewProxy("192.0.2.10")
	if err != nil {
		t.Fatal(err)
	}
	a.proxy = proxy
}

func TestIngressLogin(t *testing.T) {
	a := testApp(t)
	trustIngress(t, a)
	serviceSession(t, a)
	h := a.routes()
	root := ingressRequest(h, http.MethodGet, "/", nil, nil)
	if root.Code != http.StatusSeeOther || root.Header().Get("Location") != ingressPath+"/login" {
		t.Errorf("login redirect: %d %s", root.Code, root.Header().Get("Location"))
	}
	login := ingressRequest(h, http.MethodPost, "/login", url.Values{"password": {"carotte-42"}}, nil)
	if login.Code != http.StatusSeeOther || login.Header().Get("Location") != ingressPath+"/" {
		t.Fatalf("login: %d %s %s", login.Code, login.Header().Get("Location"), login.Body.String())
	}
	cookies := login.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login cookies: %v", cookies)
	}
	cookie := cookies[0]
	if cookie.Name != web.CookieName || cookie.Path != ingressPath+"/" || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie: %v", cookie)
	}
	checkUINavigation(t, uiDocument(t, ingressRequest(h, http.MethodGet, "/", nil, cookie)), ingressPath+"/")
	bad := ingressRequest(h, http.MethodPost, "/login", url.Values{"password": {"wrong"}}, nil)
	if bad.Code != http.StatusSeeOther || !strings.HasPrefix(bad.Header().Get("Location"), ingressPath+"/login?err=") || len(bad.Result().Cookies()) != 0 {
		t.Fatalf("bad password: %d %s", bad.Code, bad.Header().Get("Location"))
	}
	logout := ingressRequest(h, http.MethodPost, "/logout", url.Values{}, cookie)
	if logout.Code != http.StatusSeeOther || logout.Header().Get("Location") != ingressPath+"/login" {
		t.Fatalf("logout: %d %s", logout.Code, logout.Header().Get("Location"))
	}
	if after := ingressRequest(h, http.MethodGet, "/", nil, cookie); after.Header().Get("Location") != ingressPath+"/login" {
		t.Fatal("logout left session valid")
	}
}

func TestIngressPages(t *testing.T) {
	a := testApp(t)
	trustIngress(t, a)
	cookie := serviceSession(t, a)
	f := appFixture(t, a)
	f.Mu.Lock()
	f.Configured = true
	f.Catalog = []device.Release{{Tag: "v1.1.0", Ready: true}}
	f.Mu.Unlock()
	if _, err := a.store.Update(func(s *config.Settings) error {
		s.Tags["d0:02:18:01:02:03:04:05"] = config.TagAction{App: "radio", Value: "https://example.org/live.mp3"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(a.userSoundDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.userSoundDir(), "example.mp3"), []byte("ID3"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := a.routes()
	for _, path := range []string{"/", "/settings", "/wifi", "/services", "/tags", "/sounds", "/updates", "/login"} {
		t.Run(path, func(t *testing.T) {
			doc := uiDocument(t, ingressRequest(h, http.MethodGet, path, nil, cookie))
			for _, n := range uiElements(doc, "") {
				for _, attr := range []string{"href", "src", "action"} {
					value := uiAttribute(n, attr)
					if strings.HasPrefix(value, "/") && !strings.HasPrefix(value, ingressPath+"/") {
						t.Errorf("%s %s escapes ingress: %s", n.Data, attr, value)
					}
				}
				if uiAttribute(n, "id") == "wifi" && uiAttribute(n, "data-prefix") != ingressPath {
					t.Error("Wi-Fi JS has no ingress prefix")
				}
				if uiAttribute(n, "name") == "back" {
					value := uiAttribute(n, "value")
					if !strings.HasPrefix(value, "/") || strings.HasPrefix(value, ingressPath) {
						t.Error("hidden back path must stay internal")
					}
				}
			}
		})
	}
	if js := ingressRequest(h, http.MethodGet, "/wifi.js", nil); js.Code != http.StatusOK || !strings.Contains(js.Header().Get("Content-Type"), "javascript") {
		t.Fatalf("Wi-Fi static script: %d", js.Code)
	}
}

func TestIngressWiFiCookies(t *testing.T) {
	a := testApp(t)
	trustIngress(t, a)
	cookie := serviceSession(t, a)
	h := a.routes()
	reserve := ingressRequest(h, http.MethodPost, "/wifi/reserve", url.Values{}, cookie)
	if reserve.Code != http.StatusSeeOther || reserve.Header().Get("Location") != ingressPath+"/wifi" {
		t.Fatalf("reserve: %d %s", reserve.Code, reserve.Body.String())
	}
	cookies := reserve.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != wifiCookie || cookies[0].Path != ingressPath+"/" || !cookies[0].Secure {
		t.Fatalf("Wi-Fi cookie: %v", cookies)
	}
	release := ingressRequest(h, http.MethodPost, "/wifi/release", url.Values{}, cookie, cookies[0])
	uiDocument(t, release)
	deleted := release.Result().Cookies()
	if len(deleted) != 1 || deleted[0].Path != cookies[0].Path || deleted[0].MaxAge != -1 || !deleted[0].Secure {
		t.Fatalf("Wi-Fi cookie deletion: %v", deleted)
	}
}

func TestProxyEnvironmentAndDirectAccess(t *testing.T) {
	t.Setenv("NABOS_TRUSTED_PROXIES", "")
	if loadEnv().TrustedProxies != "" {
		t.Fatal("proxy trust enabled by default")
	}
	t.Setenv("NABOS_TRUSTED_PROXIES", "192.0.2.10")
	if loadEnv().TrustedProxies != "192.0.2.10" {
		t.Fatal("proxy environment setting ignored")
	}
	if _, err := NewApp(Env{DataDir: t.TempDir(), TrustedProxies: "192.0.2.10,bad"}); err == nil {
		t.Fatal("startup accepted an invalid trusted proxy")
	}
	a := testApp(t)
	serviceSession(t, a)
	h := a.routes()
	spoofed := ingressRequest(h, http.MethodPost, "/login", url.Values{"password": {"carotte-42"}})
	if spoofed.Code != http.StatusForbidden {
		t.Fatalf("default trusted forwarded HTTPS: %d", spoofed.Code)
	}
	root := ingressRequest(h, http.MethodGet, "/", nil)
	if root.Header().Get("Location") != "/login" {
		t.Fatal("default trusted forwarded prefix")
	}
	login := serviceRequest(h, http.MethodPost, "/login", url.Values{"password": {"carotte-42"}}, nil)
	if login.Code != http.StatusSeeOther || login.Header().Get("Location") != "/" {
		t.Fatalf("direct login: %d %s", login.Code, login.Body.String())
	}
	cookies := login.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Path != "/" || cookies[0].Secure {
		t.Fatalf("direct HTTP cookie: %v", cookies)
	}
	checkUINavigation(t, uiDocument(t, serviceRequest(h, http.MethodGet, "/", nil, cookies[0])), "/")
	trustIngress(t, a)
	if w := serviceRequest(a.routes(), http.MethodPost, "/login", url.Values{"password": {"carotte-42"}}, nil); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatal("configured proxy trust broke direct login")
	}
}

func TestIngressMastodonOAuth(t *testing.T) {
	wantCallback := ingressOrigin + ingressPath + "/services/mastodon/callback"
	var exchanges atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/apps":
			if got := r.FormValue("redirect_uris"); got != wantCallback {
				t.Errorf("registered callback: %s", got)
			}
			w.Write([]byte(`{"client_id":"client","client_secret":"secret"}`))
		case "/oauth/token":
			exchanges.Add(1)
			if r.FormValue("redirect_uri") != wantCallback || r.FormValue("code") != "code-1" {
				t.Error("code exchanged with wrong callback or code")
			}
			w.Write([]byte(`{"access_token":"private-token"}`))
		case "/api/v1/accounts/verify_credentials":
			w.Write([]byte(`{"id":"account-1","username":"rabbit"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	a := testApp(t)
	trustIngress(t, a)
	cookie := serviceSession(t, a)
	h := a.routes()
	connect := func() string {
		t.Helper()
		w := ingressRequest(h, http.MethodPost, "/services/mastodon/connect", url.Values{"instance": {server.URL}}, cookie)
		u, err := url.Parse(w.Header().Get("Location"))
		if err != nil || w.Code != http.StatusSeeOther || u.Host != strings.TrimPrefix(server.URL, "http://") || u.Query().Get("redirect_uri") != wantCallback || u.Query().Get("state") == "" {
			t.Fatalf("OAuth authorization: %d %s", w.Code, w.Header().Get("Location"))
		}
		return u.Query().Get("state")
	}
	callback := func(state string) *http.Request {
		return ingressHTTPRequest(http.MethodGet, "/services/mastodon/callback?code=code-1&state="+url.QueryEscape(state), nil)
	}
	for _, changed := range []string{"scheme", "prefix"} {
		r := callback(connect())
		if changed == "scheme" {
			r.Header.Set("X-Forwarded-Proto", "http")
		} else {
			r.Header.Set("X-Ingress-Path", "/api/ingress/other")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest || exchanges.Load() != 0 {
			t.Fatalf("changed callback %s accepted: %d", changed, w.Code)
		}
	}
	state := connect()
	bad := httptest.NewRecorder()
	h.ServeHTTP(bad, callback("wrong"))
	if bad.Code != http.StatusBadRequest || exchanges.Load() != 0 {
		t.Fatal("invalid OAuth state exchanged a code")
	}
	good := httptest.NewRecorder()
	h.ServeHTTP(good, callback(state))
	if good.Code != http.StatusSeeOther || !strings.HasPrefix(good.Header().Get("Location"), ingressPath+"/services?ok=") || exchanges.Load() != 1 {
		t.Fatalf("OAuth callback: %d %s", good.Code, good.Header().Get("Location"))
	}
	if got := a.store.Get().Mastodon; got.RedirectURI != wantCallback || got.AccessToken != "private-token" {
		t.Fatalf("stored OAuth state: %+v", got)
	}
	replay := httptest.NewRecorder()
	h.ServeHTTP(replay, callback(state))
	if replay.Code != http.StatusBadRequest || exchanges.Load() != 1 {
		t.Fatal("OAuth callback replay accepted")
	}
}

func TestTrustedProxyCannotGrantLocalAdmission(t *testing.T) {
	core := wifiTestBus(t)
	core.mu.Lock()
	core.status = network.Status{Mode: "hotspot", Address: hotspotAddress, Generation: "network:1", Phase: "idle"}
	core.mu.Unlock()
	a := testApp(t)
	trustIngress(t, a)
	h := a.routes()
	for _, tc := range []struct {
		path string
		want int
	}{{"/healthz", http.StatusNotFound}, {"/wifi", http.StatusForbidden}} {
		r := ingressHTTPRequest(http.MethodGet, tc.path, nil)
		r.Header.Set("X-Forwarded-For", "127.0.0.1")
		r.Header.Set("X-Forwarded-Host", hotspotAddress)
		r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 80}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("forwarded local admission on %s: %d", tc.path, w.Code)
		}
	}
}
