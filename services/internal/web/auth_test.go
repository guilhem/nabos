package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/guilhem/nabos/services/internal/config"
)

func newAuth(t *testing.T) *Auth {
	st, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	return NewAuth(st)
}

func do(h http.Handler, method, path, cookie, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(""))
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: CookieName, Value: cookie})
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAuthFlow(t *testing.T) {
	a := newAuth(t)
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("secret")) })
	h := a.Middleware(ok, "/login", "/setup", "/healthz")
	if w := do(h, "GET", "/", "", ""); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/setup" {
		t.Fatalf("unconfigured must redirect to setup: %d", w.Code)
	}
	if _, err := a.Setup("short"); err == nil {
		t.Fatal("short password accepted")
	}
	if _, err := a.Setup("carotte-42"); err != ErrNoPresence {
		t.Fatalf("setup without pressing the button: %v", err)
	}
	a.MarkPresence()
	if _, err := a.Setup(strings.Repeat("x", maxPassword+1)); err == nil {
		t.Fatal("oversized password accepted")
	}
	tok, err := a.Setup("carotte-42")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Setup("another-pass"); err == nil {
		t.Fatal("setup must only work once")
	}
	if w := do(h, "GET", "/", "", ""); w.Header().Get("Location") != "/login" {
		t.Fatal("anonymous access")
	}
	if w := do(h, "GET", "/", tok, ""); w.Body.String() != "secret" {
		t.Fatal("session refused")
	}
	if w := do(h, "POST", "/", tok, "http://evil.example"); w.Code != http.StatusForbidden {
		t.Fatalf("cross-site post accepted: %d", w.Code)
	}
	if w := do(h, "POST", "/", tok, ""); w.Code != http.StatusForbidden {
		t.Fatal("post without origin accepted")
	}
	if w := do(h, "POST", "/", tok, "http://example.com"); w.Code != http.StatusOK {
		t.Fatalf("same-origin post refused: %d", w.Code)
	}
	if w := do(h, "POST", "/", "forged", "http://example.com"); w.Code != http.StatusUnauthorized {
		t.Fatal("forged session accepted")
	}
}

func TestLoginLockout(t *testing.T) {
	a := newAuth(t)
	now := time.Unix(1_800_000_000, 0)
	a.Now = func() time.Time { return now }
	a.MarkPresence()
	a.Setup("carotte-42")
	// Concurrent wrong attempts cannot slip past the lockout.
	var wg sync.WaitGroup
	var accepted atomic.Int32
	for i := 0; i < 3*maxFailures; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.Login("wrong"); err == nil {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 0 {
		t.Fatal("wrong password accepted")
	}
	if _, err := a.Login("carotte-42"); err != ErrLocked {
		t.Fatalf("expected lockout, got %v", err)
	}
	now = now.Add(lockout + time.Second)
	if _, err := a.Login("carotte-42"); err != nil {
		t.Fatal(err)
	}
	if a.ChangePassword("carotte-42", "lapin-1234") != nil {
		t.Fatal("change password")
	}
	if _, err := a.Login("carotte-42"); err == nil {
		t.Fatal("old password still valid")
	}
}

func TestSessionsAreBoundedAndResetWorks(t *testing.T) {
	a := newAuth(t)
	now := time.Unix(1_800_000_000, 0)
	a.Now = func() time.Time { return now }
	a.MarkPresence()
	first, _ := a.Setup("carotte-42")
	for i := 0; i < 3*maxSessions; i++ {
		now = now.Add(time.Second)
		a.newSession()
	}
	if len(a.sessions) != maxSessions {
		t.Fatalf("%d sessions kept", len(a.sessions))
	}
	if _, ok := a.sessions[first]; ok {
		t.Fatal("oldest session not evicted")
	}
	now = now.Add(sessionTTL + time.Hour)
	a.newSession()
	if len(a.sessions) != 1 {
		t.Fatal("expired sessions not reaped")
	}
	if a.Reset() != nil || a.Configured() || len(a.sessions) != 0 {
		t.Fatal("reset")
	}
}

func TestLoopback(t *testing.T) {
	r := httptest.NewRequest("GET", "/healthz", nil)
	r.RemoteAddr = "192.168.1.20:5555"
	if Loopback(r) {
		t.Fatal("LAN address treated as loopback")
	}
	r.RemoteAddr = "127.0.0.1:5555"
	if !Loopback(r) {
		t.Fatal("loopback refused")
	}
}

func TestSameOriginIncludesScheme(t *testing.T) {
	for _, tc := range []struct {
		target, origin string
		want           bool
	}{
		{"http://rabbit.local/wifi/connect", "http://rabbit.local", true},
		{"https://rabbit.local/wifi/connect", "https://rabbit.local", true},
		{"http://rabbit.local/wifi/connect", "https://rabbit.local", false},
		{"https://rabbit.local/wifi/connect", "http://rabbit.local", false},
		{"http://rabbit.local/wifi/connect", "//rabbit.local", false},
		{"http://rabbit.local/wifi/connect", "http://other.local", false},
		{"http://rabbit.local/wifi/connect", "null", false},
	} {
		r := httptest.NewRequest("POST", tc.target, nil)
		r.Header.Set("Origin", tc.origin)
		if sameOrigin(r) != tc.want {
			t.Errorf("sameOrigin(%q, %q)", tc.target, tc.origin)
		}
	}
}
