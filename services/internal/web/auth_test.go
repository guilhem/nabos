package web

import (
	"errors"
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

func freshPresence(t *testing.T, a *Auth) {
	t.Helper()
	if !a.ArmPresence() {
		t.Fatal("setup did not arm")
	}
	a.MarkPresence(a.MonoNow())
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
	freshPresence(t, a)
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
	freshPresence(t, a)
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
	freshPresence(t, a)
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

func TestPresenceUsesArmedEdgeTime(t *testing.T) {
	a := newAuth(t)
	now := uint64(10 * time.Minute)
	a.MonoNow = func() uint64 { return now }
	a.MarkPresence(now)
	if a.Present() {
		t.Fatal("startup presence must be disarmed")
	}
	if !a.ArmPresence() {
		t.Fatal("arm")
	}
	cutoff := now
	for _, edge := range []uint64{0, cutoff - 1, cutoff, cutoff + 1, ^uint64(0)} {
		a.MarkPresence(edge)
		if a.Present() {
			t.Fatalf("accepted pre-arm or future edge %d", edge)
		}
	}
	now += uint64(time.Second)
	edge := now
	a.MarkPresence(edge)
	if !a.Present() {
		t.Fatal("fresh edge refused")
	}
	now += uint64(time.Minute)
	if !a.ArmPresence() || a.cutoff != cutoff || a.presence != edge || !a.Present() {
		t.Fatal("reload cleared proof or moved cutoff")
	}
	a.MarkPresence(edge - 1)
	if a.presence != edge {
		t.Fatal("out-of-order edge replaced proof")
	}
	now = edge + uint64(presenceTTL)
	a.MarkPresence(edge)
	if a.Present() || a.presence != edge {
		t.Fatal("duplicate extended proof past the edge's expiry")
	}
	a.MarkPresence(now)
	if !a.Present() {
		t.Fatal("new press refused")
	}
	a.DisarmPresence()
	a.MarkPresence(now)
	if a.Present() || a.presence != 0 {
		t.Fatal("disarm did not clear and reject proof")
	}
	a.ArmPresence()
	a.MarkPresence(edge)
	if a.Present() {
		t.Fatal("rearm admitted old proof")
	}
	a.DisarmPresence()
	now = 0
	if a.ArmPresence() || a.Present() {
		t.Fatal("clock failure armed presence")
	}
}

func TestSetupRechecksPresenceAtCommit(t *testing.T) {
	for _, invalidate := range []string{"disarm", "expire"} {
		t.Run(invalidate, func(t *testing.T) {
			a := newAuth(t)
			var now atomic.Uint64
			now.Store(uint64(10 * time.Minute))
			a.MonoNow = now.Load
			a.ArmPresence()
			now.Add(uint64(time.Second))
			a.MarkPresence(now.Load())

			// Hold the real store lock: hashing and an in-flight config wait must
			// not prevent invalidation, or let its old proof commit afterwards.
			entered, release, stored := make(chan struct{}), make(chan struct{}), make(chan struct{})
			go func() {
				a.store.Update(func(*config.Settings) error {
					close(entered)
					<-release
					return errors.New("no change")
				})
				close(stored)
			}()
			<-entered
			checked := make(chan struct{})
			var checks atomic.Int32
			a.MonoNow = func() uint64 {
				n := now.Load()
				if checks.Add(1) == 1 {
					close(checked)
				}
				return n
			}
			done := make(chan error, 1)
			go func() { _, err := a.Setup("carotte-42"); done <- err }()
			<-checked
			invalidated := make(chan struct{})
			go func() {
				if invalidate == "disarm" {
					a.DisarmPresence()
				} else {
					now.Add(uint64(presenceTTL))
				}
				close(invalidated)
			}()
			select {
			case <-invalidated:
			case <-time.After(time.Second):
				close(release)
				<-stored
				<-done
				t.Fatal("pending config commit blocked presence invalidation")
			}
			close(release)
			<-stored
			if err := <-done; err != ErrNoPresence || a.Configured() || len(a.sessions) != 0 {
				t.Fatalf("stale presence committed: error=%v configured=%v", err, a.Configured())
			}
		})
	}
}
