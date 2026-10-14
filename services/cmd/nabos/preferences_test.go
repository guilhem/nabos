package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guilhem/nabos/services/internal/web"
)

func TestPreferencesSettings(t *testing.T) {
	a := testApp(t)
	trustIngress(t, a)
	cookie := serviceSession(t, a)
	h := a.routes()
	appPath := filepath.Join(a.env.DataDir, "application.json")
	before, err := os.ReadFile(appPath)
	if err != nil {
		t.Fatal(err)
	}
	values := url.Values{"rabbit_name": {"Carotte <🐰>"}, "allowed_domains": {"AMAND.LETTRON.FR.\n\nrabbit.local"}, "session_hours": {"24"}}
	w := ingressRequest(h, http.MethodPost, "/settings/preferences", values, cookie)
	doc := uiDocument(t, w)
	checkUISaveForm(t, doc, ingressPath+"/settings/preferences", false, "rabbit_name", "allowed_domains", "session_hours")
	checkUINavigation(t, doc, ingressPath+"/settings")
	if !strings.Contains(w.Body.String(), "Carotte &lt;🐰&gt;") || !strings.Contains(w.Body.String(), "Nom et accès web enregistrés") {
		t.Fatal("name or confirmation missing/unsafe")
	}
	want := web.Preferences{Name: "Carotte <🐰>", SessionHours: 24, AllowedDomains: []string{"amand.lettron.fr", "rabbit.local"}}
	if current, err := a.prefs.Current(); err != nil || !reflect.DeepEqual(current, want) {
		t.Fatalf("saved preferences: %v %v", current, err)
	}
	if after, err := os.ReadFile(appPath); err != nil || !bytes.Equal(after, before) {
		t.Fatal("preferences modified application settings/credentials")
	}
	restarted, err := NewApp(a.env)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.device.Close)
	if got, err := restarted.prefs.Current(); err != nil || !reflect.DeepEqual(got, want) || !restarted.auth.Configured() {
		t.Fatal("restart lost preferences or password")
	}
	login := ingressRequest(h, http.MethodPost, "/login", url.Values{"password": {"carotte-42"}})
	cookies := login.Result().Cookies()
	if login.Code != http.StatusSeeOther || len(cookies) != 1 || cookies[0].MaxAge != 24*3600 || cookies[0].Path != ingressPath+"/" || !cookies[0].Secure {
		t.Fatalf("configured login cookie: %d %v", login.Code, cookies)
	}
	for _, invalid := range []url.Values{
		{"rabbit_name": {"Carotte"}, "session_hours": {"1"}},
		{"rabbit_name": {"Carotte", "other"}, "allowed_domains": {""}, "session_hours": {"1"}},
		{"rabbit_name": {strings.Repeat("x", 65)}, "allowed_domains": {""}, "session_hours": {"1"}},
		{"rabbit_name": {"Carotte"}, "allowed_domains": {"https://evil.example/path"}, "session_hours": {"1"}},
		{"rabbit_name": {"Carotte"}, "allowed_domains": {""}, "session_hours": {"2"}},
		{"rabbit_name": {"Carotte"}, "allowed_domains": {""}, "session_hours": {"invalid"}},
		{"rabbit_name": {strings.Repeat("x", 20<<10)}, "allowed_domains": {""}, "session_hours": {"1"}},
	} {
		w := ingressRequest(h, http.MethodPost, "/settings/preferences", invalid, cookie)
		if w.Code != http.StatusBadRequest && !strings.Contains(w.Body.String(), "non enregistrées") {
			t.Fatalf("invalid form succeeded: %d", w.Code)
		}
		if got, _ := a.prefs.Current(); !reflect.DeepEqual(got, want) {
			t.Fatal("invalid form changed active preferences")
		}
	}
	if w := ingressRequest(h, http.MethodPost, "/settings/preferences", values); w.Code != http.StatusUnauthorized {
		t.Fatal("anonymous preferences save")
	}
	r := ingressHTTPRequest(http.MethodPost, "/settings/preferences", values, cookie)
	r.Header.Set("Origin", "https://evil.example")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatal("cross-site preferences save")
	}
	// A trusted proxy cannot recover by presenting the rabbit's numeric address.
	r = ingressHTTPRequest(http.MethodGet, "/settings", nil, cookie)
	r.Host = "192.0.2.20"
	r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 80}))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatal("proxy bypassed domain restriction with numeric address")
	}
}

func TestConcurrentPreferencesMatchMQTTName(t *testing.T) {
	a := testApp(t)
	cookie := serviceSession(t, a)
	domains := []string{"example.com"}
	label := strings.Repeat("a", 63)
	for i := range 31 {
		domains = append(domains, fmt.Sprintf("rabbit-%d.%s.%s.%s.local", i, label, label, label))
	}
	if _, err := a.prefs.Save(web.Preferences{Name: "Rabbit 0", SessionHours: 1, AllowedDomains: domains}); err != nil {
		t.Fatal(err)
	}
	latest := haBroker(t, a)
	h := a.routes()
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			values := url.Values{"rabbit_name": {fmt.Sprintf("Rabbit %d", i%2)}, "allowed_domains": {strings.Join(domains, "\n")}, "session_hours": {"1"}}
			if w := serviceRequest(h, http.MethodPost, "/settings/preferences", values, cookie); w.Code != http.StatusOK {
				t.Errorf("save: %d %s", w.Code, w.Body.String())
			}
		})
	}
	wg.Wait()
	current, _ := a.prefs.Current()
	topic := "homeassistant/sensor/" + a.ha.Node + "/state/config"
	pynabWait(t, "discovery matches last saved name", 3*time.Second, func() bool {
		var message struct{ Device struct{ Name string } }
		return json.Unmarshal(latest(topic), &message) == nil && message.Device.Name == current.Name
	})
}

func TestInvalidPreferencesLocalRecovery(t *testing.T) {
	a := testApp(t)
	cookie := serviceSession(t, a)
	path := filepath.Join(a.env.DataDir, "preferences.json")
	if err := os.WriteFile(path, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.prefs = web.LoadPreferences(path)
	a.auth = web.NewAuth(a.store, a.prefs)
	tok, err := a.auth.Login("carotte-42")
	if err != nil {
		t.Fatal(err)
	}
	cookie.Value = tok
	h := a.routes()
	local := func(method, path string, values url.Values, authenticated bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://192.0.2.20"+path, strings.NewReader(values.Encode()))
		r.RemoteAddr = "192.0.2.30:4321"
		r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 80}))
		if values != nil {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Origin", "http://192.0.2.20")
		}
		if authenticated {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := local(http.MethodGet, "/settings", nil, true); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "préférences illisibles") {
		t.Fatal("local recovery missing load error")
	}
	values := url.Values{"rabbit_name": {"Carotte"}, "allowed_domains": {"rabbit.local"}, "session_hours": {"1"}}
	if w := local(http.MethodPost, "/settings/preferences", values, false); w.Code != http.StatusUnauthorized {
		t.Fatal("local recovery bypassed authentication")
	}
	if w := local(http.MethodPost, "/settings/preferences", values, true); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Nom et accès web enregistrés") {
		t.Fatalf("recovery failed: %d %s", w.Code, w.Body.String())
	}
	if _, err := web.LoadPreferences(path).Current(); err != nil {
		t.Fatal("recovery not persistent", err)
	}
}
