// Package web holds the admin authentication of the local interface.
package web

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/guilhem/nabos/services/internal/config"
)

const (
	CookieName  = "nab_session"
	sessionTTL  = 7 * 24 * time.Hour
	maxSessions = 32
	maxFailures = 5
	lockout     = time.Minute
	iterations  = 60000 // about a second on a Pi Zero
	presenceTTL = 5 * time.Minute
	minPassword = 8
	maxPassword = 128
)

var (
	ErrLocked      = errors.New("trop d'essais, réessayez dans une minute")
	ErrNoPresence  = errors.New("appuyez d'abord sur le bouton de la tête du lapin")
	ErrConfigured  = errors.New("un mot de passe existe déjà")
	errBadPassword = errors.New("mot de passe incorrect")
)

func hashPassword(pw string, salt []byte, iter int) string {
	k, _ := pbkdf2.Key(sha256.New, pw, salt, iter, 32)
	return hex.EncodeToString(k)
}

func NewAdmin(pw string) (config.Admin, error) {
	if len(pw) < minPassword || len(pw) > maxPassword {
		return config.Admin{}, errors.New("le mot de passe doit faire de 8 à 128 caractères")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return config.Admin{}, err
	}
	return config.Admin{Salt: hex.EncodeToString(salt), Hash: hashPassword(pw, salt, iterations), Iterations: iterations}, nil
}

func checkPassword(a config.Admin, pw string) bool {
	salt, err := hex.DecodeString(a.Salt)
	if err != nil || a.Hash == "" || a.Iterations <= 0 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hashPassword(pw, salt, a.Iterations)), []byte(a.Hash)) == 1
}

type Auth struct {
	store *config.Store
	Now   func() time.Time

	// login serialises password checks: the global lockout cannot be
	// bypassed by concurrent attempts, and hashing is rate limited.
	login sync.Mutex

	mu       sync.Mutex
	sessions map[string]time.Time
	failures int
	locked   time.Time
	presence time.Time
}

func NewAuth(store *config.Store) *Auth {
	return &Auth{store: store, Now: time.Now, sessions: map[string]time.Time{}}
}

func (a *Auth) Configured() bool { return a.store.Get().Admin.Hash != "" }

// MarkPresence records a physical button press on the rabbit.
func (a *Auth) MarkPresence() {
	a.mu.Lock()
	a.presence = a.Now()
	a.mu.Unlock()
}

func (a *Auth) Present() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return !a.presence.IsZero() && a.Now().Sub(a.presence) < presenceTTL
}

// Reset forgets the password (physical recovery: click then hold the button).
func (a *Auth) Reset() error {
	_, err := a.store.Update(func(s *config.Settings) error { s.Admin = config.Admin{}; return nil })
	a.mu.Lock()
	a.sessions = map[string]time.Time{}
	a.mu.Unlock()
	return err
}

func (a *Auth) newSession() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.Now()
	oldest, oldestExp := "", time.Time{}
	for k, exp := range a.sessions {
		if now.After(exp) {
			delete(a.sessions, k)
		} else if oldest == "" || exp.Before(oldestExp) {
			oldest, oldestExp = k, exp
		}
	}
	if len(a.sessions) >= maxSessions {
		delete(a.sessions, oldest)
	}
	a.sessions[tok] = now.Add(sessionTTL)
	return tok
}

// Setup sets the first password; it needs a recent button press and is
// refused once a password exists.
func (a *Auth) Setup(pw string) (string, error) {
	if !a.Present() {
		return "", ErrNoPresence
	}
	admin, err := NewAdmin(pw)
	if err != nil {
		return "", err
	}
	_, err = a.store.Update(func(s *config.Settings) error {
		if s.Admin.Hash != "" {
			return ErrConfigured
		}
		s.Admin = admin
		return nil
	})
	if err != nil {
		return "", err
	}
	return a.newSession(), nil
}

func (a *Auth) ChangePassword(old, pw string) error {
	if err := a.check(old); err != nil {
		return err
	}
	admin, err := NewAdmin(pw)
	if err != nil {
		return err
	}
	if _, err := a.store.Update(func(s *config.Settings) error { s.Admin = admin; return nil }); err != nil {
		return err
	}
	a.mu.Lock()
	a.sessions = map[string]time.Time{}
	a.mu.Unlock()
	return nil
}

// check verifies pw under the login lock with the global failure counter.
func (a *Auth) check(pw string) error {
	a.login.Lock()
	defer a.login.Unlock()
	a.mu.Lock()
	locked := a.Now().Before(a.locked)
	a.mu.Unlock()
	if locked {
		return ErrLocked
	}
	ok := len(pw) <= maxPassword && checkPassword(a.store.Get().Admin, pw)
	a.mu.Lock()
	defer a.mu.Unlock()
	if !ok {
		a.failures++
		if a.failures >= maxFailures {
			a.failures = 0
			a.locked = a.Now().Add(lockout)
		}
		return errBadPassword
	}
	a.failures = 0
	return nil
}

func (a *Auth) Login(pw string) (string, error) {
	if err := a.check(pw); err != nil {
		return "", err
	}
	return a.newSession(), nil
}

func (a *Auth) Logout(r *http.Request) {
	if c, err := r.Cookie(CookieName); err == nil {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
	}
}

func (a *Auth) Valid(r *http.Request) bool {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[c.Value]
	if !ok || a.Now().After(exp) {
		delete(a.sessions, c.Value)
		return false
	}
	a.sessions[c.Value] = a.Now().Add(sessionTTL)
	return true
}

func SetCookie(w http.ResponseWriter, tok string) {
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds())})
}

// sameOrigin rejects cross-site form posts (CSRF) in addition to SameSite cookies.
func sameOrigin(r *http.Request) bool {
	src := r.Header.Get("Origin")
	if src == "" {
		src = r.Header.Get("Referer")
	}
	if src == "" {
		return false
	}
	u, err := url.Parse(src)
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return err == nil && u.Scheme == scheme && u.Host == r.Host
}

// Middleware protects everything except the public paths.
func (a *Auth) Middleware(next http.Handler, public ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		for _, p := range public {
			if r.URL.Path == p || (strings.HasSuffix(p, "/") && strings.HasPrefix(r.URL.Path, p)) {
				next.ServeHTTP(w, r)
				return
			}
		}
		switch {
		case !a.Configured():
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
		case !a.Valid(r):
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
			} else {
				http.Error(w, "authentication required", http.StatusUnauthorized)
			}
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// Loopback reports whether the request comes from the rabbit itself.
func Loopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	return err == nil && ip != nil && ip.IsLoopback()
}
