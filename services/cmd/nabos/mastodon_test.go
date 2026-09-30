package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guilhem/nabos/services/internal/config"
)

const mastodonTestOrigin = "http://nabaztag.local:8080"

// mastodonRouteRequest exercises the real route and auth middleware while
// keeping Mastodon itself on a local httptest server.
func mastodonRouteRequest(h http.Handler, method, path string, form url.Values, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
	var body *strings.Reader
	if form == nil {
		body = strings.NewReader("")
	} else {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, mastodonTestOrigin+path, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestMastodonOAuthRoutesAndOneTimeCallback(t *testing.T) {
	var mu sync.Mutex
	apps, exchanges, verifications := 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/apps":
			apps++
			if r.Method != http.MethodPost || r.FormValue("redirect_uris") != mastodonTestOrigin+"/services/mastodon/callback" {
				t.Errorf("app registration: %s %s", r.Method, r.FormValue("redirect_uris"))
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"client_id":"client","client_secret":"secret"}`))
		case "/oauth/token":
			exchanges++
			if r.Method != http.MethodPost || r.FormValue("code") != "code-1" || r.FormValue("redirect_uri") != mastodonTestOrigin+"/services/mastodon/callback" {
				t.Errorf("code exchange: %s %s", r.Method, r.FormValue("redirect_uri"))
			}
			w.Write([]byte(`{"access_token":"private-token"}`))
		case "/api/v1/accounts/verify_credentials":
			verifications++
			if r.Header.Get("Authorization") != "Bearer private-token" {
				t.Error("missing account token")
			}
			w.Write([]byte(`{"id":"account-1","username":"rabbit","display_name":"Rabbit","avatar":"https://example.org/avatar.png"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	a := testApp(t)
	cookie := serviceSession(t, a)
	h := a.routes()
	connectForm := url.Values{"instance": {server.URL}}
	connect := func() string {
		t.Helper()
		w := mastodonRouteRequest(h, http.MethodPost, "/services/mastodon/connect", connectForm, cookie, mastodonTestOrigin)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("connect: %d %s", w.Code, w.Body.String())
		}
		u, err := url.Parse(w.Header().Get("Location"))
		if err != nil || u.Path != "/oauth/authorize" || u.Query().Get("client_id") != "client" || u.Query().Get("redirect_uri") != mastodonTestOrigin+"/services/mastodon/callback" || u.Query().Get("state") == "" {
			t.Fatalf("authorize URL: %s %v", w.Header().Get("Location"), err)
		}
		return u.Query().Get("state")
	}
	callback := func(state, host string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, mastodonTestOrigin+"/services/mastodon/callback?code=code-1&state="+url.QueryEscape(state), nil)
		if host != "" {
			r.Host = host
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	state := connect()
	for _, bad := range []string{"", "wrong-state"} {
		if w := callback(bad, ""); w.Code != http.StatusBadRequest {
			t.Fatalf("callback state %q: %d", bad, w.Code)
		}
	}
	mu.Lock()
	gotExchanges := exchanges
	mu.Unlock()
	if gotExchanges != 0 {
		t.Fatal("invalid state exchanged a code")
	}
	a.mu.Lock()
	a.oauth.expires = time.Now().Add(-time.Second)
	a.mu.Unlock()
	if w := callback(state, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("expired callback: %d", w.Code)
	}
	state = connect()
	if w := callback(state, "elsewhere.local:8080"); w.Code != http.StatusBadRequest {
		t.Fatalf("wrong callback host: %d", w.Code)
	}
	state = connect()
	if w := callback(state, ""); w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "ok=") {
		t.Fatalf("valid callback: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := callback(state, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("replayed callback: %d", w.Code)
	}
	mu.Lock()
	gotApps, gotExchanges, gotVerifications := apps, exchanges, verifications
	mu.Unlock()
	if gotApps != 3 || gotExchanges != 1 || gotVerifications != 1 {
		t.Fatalf("mock calls: apps=%d exchanges=%d verifies=%d", gotApps, gotExchanges, gotVerifications)
	}
	s := a.store.Get().Mastodon
	if s.Instance != server.URL || s.ClientID != "client" || s.ClientSecret != "secret" || s.RedirectURI != mastodonTestOrigin+"/services/mastodon/callback" || s.AccessToken != "private-token" || s.AccountID != "account-1" || s.Username != "rabbit" {
		t.Fatalf("OAuth state not saved: %+v", s)
	}
	reloaded, err := config.Open(filepath.Join(a.env.DataDir, "application.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Get().Mastodon; got.AccessToken != "private-token" || got.AccountID != "account-1" {
		t.Fatalf("OAuth state not persisted: %+v", got)
	}
}

func TestMastodonRoutesAuthCSRFAndCancelProposal(t *testing.T) {
	var mu sync.Mutex
	posts := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/apps":
			w.Write([]byte(`{"client_id":"client","client_secret":"secret"}`))
		case "/api/v1/statuses":
			if r.Header.Get("Authorization") != "Bearer token" || r.FormValue("visibility") != "direct" {
				t.Error("non-direct or unauthenticated status")
			}
			posts = append(posts, r.FormValue("status"))
			w.Write([]byte(`{"id":"42","created_at":"2026-01-01T00:00:00Z"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	a := testApp(t)
	cookie := serviceSession(t, a)
	h := a.routes()
	connect := url.Values{"instance": {server.URL}}
	action := url.Values{"action": {"propose"}, "spouse": {"peer"}}
	for _, pathAndForm := range []struct {
		path string
		form url.Values
	}{{"/services/mastodon/connect", connect}, {"/services/mastodon/action", action}} {
		if w := mastodonRouteRequest(h, http.MethodPost, pathAndForm.path, pathAndForm.form, nil, mastodonTestOrigin); w.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s: %d", pathAndForm.path, w.Code)
		}
		if w := mastodonRouteRequest(h, http.MethodPost, pathAndForm.path, pathAndForm.form, cookie, "http://evil.example"); w.Code != http.StatusForbidden {
			t.Fatalf("cross-site %s: %d", pathAndForm.path, w.Code)
		}
	}
	mu.Lock()
	before := len(posts)
	mu.Unlock()
	if before != 0 {
		t.Fatal("unauthorized action posted a DM")
	}
	if w := mastodonRouteRequest(h, http.MethodPost, "/services/mastodon/connect", connect, cookie, mastodonTestOrigin); w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "/oauth/authorize") {
		t.Fatalf("authenticated connect: %d %s", w.Code, w.Header().Get("Location"))
	}
	_, err := a.store.Update(func(s *config.Settings) error {
		s.Mastodon.Instance = server.URL
		s.Mastodon.AccessToken = "token"
		s.Mastodon.AccountID = "self"
		s.Mastodon.Username = "rabbit"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if w := mastodonRouteRequest(h, http.MethodPost, "/services/mastodon/action", action, cookie, mastodonTestOrigin); w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "ok=") {
		t.Fatalf("proposal: %d %s", w.Code, w.Header().Get("Location"))
	}
	if got := a.store.Get().Mastodon; got.PairingState != "proposed" || got.SpouseHandle != "peer@127.0.0.1" {
		t.Fatalf("proposal state: %+v", got)
	}
	page := mastodonRouteRequest(h, http.MethodGet, "/services", nil, cookie, "")
	buttonAt := strings.Index(page.Body.String(), `name="action" value="divorce"`)
	button := ""
	if buttonAt >= 0 {
		button = strings.SplitN(page.Body.String()[buttonAt:], ">", 2)[0]
	}
	if page.Code != http.StatusOK || button == "" || strings.Contains(button, "disabled") {
		t.Fatalf("proposal cannot be cancelled in UI: %d", page.Code)
	}
	if w := mastodonRouteRequest(h, http.MethodPost, "/services/mastodon/action", url.Values{"action": {"divorce"}}, cookie, mastodonTestOrigin); w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "ok=") {
		t.Fatalf("cancel proposal: %d %s", w.Code, w.Header().Get("Location"))
	}
	if got := a.store.Get().Mastodon; got.PairingState != "" || got.SpouseHandle != "" {
		t.Fatalf("proposal not cleared: %+v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(posts) != 2 || !strings.Contains(posts[0], "NabPairing Proposal") || !strings.Contains(posts[1], "NabPairing Divorce") {
		t.Fatalf("outgoing protocol: %v", posts)
	}
}
