package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guilhem/nabos/services/internal/config"
)

func TestPreferencesPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "preferences.json")
	p := LoadPreferences(path)
	if value, err := p.Current(); err != nil || !reflect.DeepEqual(value, defaultPreferences()) {
		t.Fatalf("missing file: %v %v", value, err)
	}
	value := Preferences{Name: "  Carotte 🥕  ", AllowedDomains: []string{"AMAND.Lettron.FR."}, SessionHours: 24}
	if applied, err := p.Save(value); !applied || err != nil {
		t.Fatalf("save: %v %v", applied, err)
	}
	value.Name, value.AllowedDomains[0] = "Carotte 🥕", "amand.lettron.fr"
	got, err := LoadPreferences(path).Current()
	if err != nil || !reflect.DeepEqual(got, value) {
		t.Fatalf("reload: %v %v", got, err)
	}
	got.AllowedDomains[0] = "evil.example"
	if current, _ := p.Current(); current.AllowedDomains[0] != value.AllowedDomains[0] {
		t.Fatal("mutable snapshot")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("private file: %v %v", info, err)
	}
	for _, tc := range []Preferences{
		{Name: "", SessionHours: 1}, {Name: "a\nb", SessionHours: 1}, {Name: strings.Repeat("x", 65), SessionHours: 1},
		{Name: "lapin", SessionHours: 2}, {Name: "lapin", SessionHours: 1, AllowedDomains: []string{"https://example.com"}},
		{Name: "lapin", SessionHours: 1, AllowedDomains: []string{"*.example.com"}},
	} {
		if applied, err := p.Save(tc); applied || err == nil {
			t.Fatalf("invalid preferences saved: %+v", tc)
		}
	}
	if current, _ := p.Current(); !reflect.DeepEqual(current, value) {
		t.Fatal("invalid candidate replaced preferences")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if applied, err := p.Save(defaultPreferences()); applied || err == nil {
		t.Fatal("failed persistence reported success")
	}
	if current, _ := p.Current(); !reflect.DeepEqual(current, value) {
		t.Fatal("failed write changed active preferences")
	}
}

func TestPreferencesInvalidLoad(t *testing.T) {
	for _, raw := range []string{"", "{}", "null", `{"name":"Rabbit","session_hours":2}`, `{"name":"Rabbit","session_hours":1,"unknown":true}`, `{"name":"Rabbit","session_hours":1} {}`, strings.Repeat("x", maxPreferences+1)} {
		path := filepath.Join(t.TempDir(), "preferences.json")
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		p := LoadPreferences(path)
		if _, err := p.Current(); err == nil || p.valid {
			t.Fatalf("invalid file accepted: %q", raw[:min(len(raw), 80)])
		}
		if stored, err := os.ReadFile(path); err != nil || string(stored) != raw {
			t.Fatal("invalid file changed")
		}
		if applied, err := p.Save(defaultPreferences()); !applied || err != nil || !p.valid {
			t.Fatalf("repair: %v %v", applied, err)
		}
	}
	if p := LoadPreferences(t.TempDir()); p.valid || p.err == nil {
		t.Fatal("unreadable file enabled unrestricted access")
	}
}

func TestDomainValidation(t *testing.T) {
	for _, domain := range []string{"", "example..com", "-rabbit.local", "rabbit-.local", "a_b.local", "example.com:80", "example.com/path", "192.0.2.20", "[::1]", "*.local", "éxample.com", strings.Repeat("a", 64) + ".com"} {
		p := defaultPreferences()
		p.AllowedDomains = []string{domain}
		if p.validate() == nil {
			t.Errorf("invalid domain accepted: %q", domain)
		}
	}
}

func domainRequest(host, peer, destination, path string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://"+host+path, nil)
	r.RemoteAddr = peer
	if destination != "" {
		r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP(destination), Port: 80}))
	}
	return r
}

func TestDomainAdmission(t *testing.T) {
	p := LoadPreferences(filepath.Join(t.TempDir(), "preferences.json"))
	value := defaultPreferences()
	value.AllowedDomains = []string{"amand.lettron.fr"}
	if _, err := p.Save(value); err != nil {
		t.Fatal(err)
	}
	proxy, _ := NewProxy("192.0.2.10,127.0.0.1")
	h := proxy.Middleware(p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })))
	for _, tc := range []struct {
		host, peer, destination, path string
		allowed                       bool
	}{
		{"amand.lettron.fr", "192.0.2.10:1", "192.0.2.20", "/", true},
		{"AMAND.LETTRON.FR.:8443", "192.0.2.10:1", "192.0.2.20", "/", true},
		{"amand.lettron.fr.evil", "192.0.2.10:1", "192.0.2.20", "/", false},
		{"evil.example", "127.0.0.1:1", "127.0.0.1", "/", false},
		{"rabbit.local", "192.0.2.30:1", "192.0.2.20", "/", false},
		{"192.0.2.20:8080", "192.0.2.30:1", "192.0.2.20", "/", true},
		{"192.0.2.21", "192.0.2.30:1", "192.0.2.20", "/", false},
		{"192.0.2.20", "192.0.2.10:1", "192.0.2.20", "/", false},
		{"192.0.2.20", "invalid", "192.0.2.20", "/", false},
		{"192.0.2.20", "192.0.2.30:1", "", "/", false},
		{"0.0.0.0", "192.0.2.30:1", "0.0.0.0", "/", false},
		{"[2001:db8::20]:80", "[2001:db8::30]:1", "2001:db8::20", "/", true},
		{"10.41.0.1", "10.41.0.2:1", "10.41.0.1", "/wifi", true},
		{"127.0.0.1", "127.0.0.1:1", "127.0.0.1", "/healthz", true},
		{"127.0.0.1", "192.0.2.30:1", "192.0.2.20", "/healthz", false},
	} {
		t.Run(tc.host+tc.peer+tc.path, func(t *testing.T) {
			r := domainRequest(tc.host, tc.peer, tc.destination, tc.path)
			r.Header.Set("X-Forwarded-For", "127.0.0.1")
			r.Header.Set("X-Forwarded-Host", "amand.lettron.fr")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if (w.Code == http.StatusNoContent) != tc.allowed {
				t.Fatalf("admission: %d", w.Code)
			}
		})
	}
	// A corrupt policy only admits direct recovery and the loopback health endpoint.
	p.mu.Lock()
	p.valid = false
	p.mu.Unlock()
	for _, host := range []string{"amand.lettron.fr", "evil.example", "192.0.2.20"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, domainRequest(host, "192.0.2.30:1", "192.0.2.20", "/"))
		if (w.Code == http.StatusNoContent) != (host == "192.0.2.20") {
			t.Fatalf("invalid policy admission for %s: %d", host, w.Code)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, domainRequest("127.0.0.1", "127.0.0.1:1", "127.0.0.1", "/healthz"))
	if w.Code != http.StatusNoContent {
		t.Fatal("invalid preferences broke boot health")
	}
}

func TestPreferencesUncertainCommit(t *testing.T) {
	for _, renamed := range []bool{false, true} {
		t.Run(fmt.Sprint(renamed), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "preferences.json")
			p := LoadPreferences(path)
			candidate := Preferences{Name: "Carotte", SessionHours: 1, AllowedDomains: []string{"rabbit.local"}}
			raw, _ := json.Marshal(candidate)
			uncertain := errors.New("directory fsync failed")
			p.mu.Lock()
			applied, err := p.commit(candidate, raw, func(path string, raw []byte) error {
				if renamed {
					if err := config.WriteFile(path, raw); err != nil {
						return err
					}
				}
				return uncertain
			})
			p.mu.Unlock()
			if applied != renamed || !errors.Is(err, uncertain) {
				t.Fatalf("uncertain commit: %v %v", applied, err)
			}
			got, warning := p.Current()
			if renamed && (!reflect.DeepEqual(got, candidate) || !p.valid || !errors.Is(warning, uncertain)) {
				t.Fatal("visible commit not applied")
			}
			if !renamed && (!reflect.DeepEqual(got, defaultPreferences()) || warning != nil) {
				t.Fatal("failed write applied")
			}
			if stored, _ := LoadPreferences(path).Current(); !reflect.DeepEqual(stored, got) {
				t.Fatal("disk and active policy disagree")
			}
		})
	}
}

func TestPreferencesConcurrentSave(t *testing.T) {
	p := LoadPreferences(filepath.Join(t.TempDir(), "preferences.json"))
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			for range 10 {
				candidate := Preferences{Name: fmt.Sprint(i), SessionHours: 1, AllowedDomains: []string{fmt.Sprintf("rabbit-%d.local", i)}}
				if _, err := p.Save(candidate); err != nil {
					t.Error(err)
				}
				current, _ := p.Current()
				if current.AllowedDomains[0] != "rabbit-"+current.Name+".local" {
					t.Error("mixed snapshot")
				}
			}
		})
	}
	wg.Wait()
	current, _ := p.Current()
	stored, err := LoadPreferences(p.path).Current()
	if err != nil || !reflect.DeepEqual(current, stored) {
		t.Fatal("last save not visible")
	}
}

func TestSessionDuration(t *testing.T) {
	for _, hours := range []int{1, 24, 168} {
		t.Run(fmt.Sprint(hours), func(t *testing.T) {
			a := newAuth(t)
			prefs := defaultPreferences()
			prefs.SessionHours = hours
			if _, err := a.prefs.Save(prefs); err != nil {
				t.Fatal(err)
			}
			now := time.Unix(1_800_000_000, 0)
			a.Now = func() time.Time { return now }
			tok := a.newSession()
			expires := now.Add(time.Duration(hours) * time.Hour)
			r := httptest.NewRequest(http.MethodGet, "https://rabbit.local/", nil)
			r.AddCookie(&http.Cookie{Name: CookieName, Value: tok})
			w := httptest.NewRecorder()
			a.SetCookie(w, r, tok)
			cookies := w.Result().Cookies()
			if len(cookies) != 1 || cookies[0].MaxAge != hours*3600 || !cookies[0].Expires.Equal(expires) || !cookies[0].Secure || !cookies[0].HttpOnly {
				t.Fatalf("cookie: %v", cookies)
			}
			prefs.SessionHours = 24
			a.prefs.Save(prefs)
			for range 5 {
				now = now.Add(time.Duration(hours) * time.Hour / 6)
				if !a.Valid(r) || !a.sessions[tok].Equal(expires) {
					t.Fatal("requests or settings changes moved existing expiry")
				}
			}
			now = expires
			if a.Valid(r) {
				t.Fatal("token valid at exact expiry")
			}
			newTok := a.newSession()
			if !a.sessions[newTok].Equal(now.Add(24 * time.Hour)) {
				t.Fatal("new session ignored latest preference")
			}
		})
	}
}
