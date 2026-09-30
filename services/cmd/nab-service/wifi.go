package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/guilhem/nabos/services/internal/network"
)

const (
	wifiCookie     = "nab_wifi"
	hotspotAddress = "10.41.0.1"
	wifiLeaseTTL   = 5 * time.Minute
)

var profileUUID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type wifiSession struct {
	browser, token string
	expires        time.Time
	attempt        uint64
}

func (a *App) wifiRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /wifi", a.wifiRoute(a.wifiPage))
	m.HandleFunc("GET /wifi/status", a.wifiRoute(a.wifiStatus))
	m.HandleFunc("POST /wifi/reserve", a.wifiRoute(a.wifiReserve))
	m.HandleFunc("POST /wifi/release", a.wifiRoute(a.wifiRelease))
	m.HandleFunc("POST /wifi/scan", a.wifiRoute(a.wifiScan))
	m.HandleFunc("POST /wifi/connect", a.wifiRoute(a.wifiConnect))
	m.HandleFunc("POST /wifi/cancel", a.wifiRoute(a.wifiCancel))
	m.HandleFunc("POST /wifi/forget", a.wifiRoute(a.wifiForget))
	m.HandleFunc("GET /wifi.js", func(w http.ResponseWriter, r *http.Request) {
		b, _ := uiFS.ReadFile("wifi.js")
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Write(b)
	})
}

// Check the socket address set by net/http, never Host, forwarded headers or
// the remote IP. A client on another interface cannot use initial setup.
func hotspotSocket(r *http.Request, s network.Status) bool {
	addr, ok := r.Context().Value(http.LocalAddrContextKey).(*net.TCPAddr)
	return ok && addr.IP.Equal(net.ParseIP(hotspotAddress)) && s.Mode == "hotspot" && s.Address == hotspotAddress
}

func (a *App) wifiRoute(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "same-origin")
		if a.auth.Configured() {
			if !a.auth.Valid(r) {
				if r.Method == http.MethodGet {
					http.Redirect(w, r, "/login", http.StatusSeeOther)
				} else {
					http.Error(w, "authentication required", http.StatusUnauthorized)
				}
				return
			}
		} else {
			s, err := network.ReadStatus(r.Context())
			if err != nil {
				http.Error(w, network.ErrUnavailable.Error(), http.StatusServiceUnavailable)
				return
			}
			if !hotspotSocket(r, s) {
				if r.URL.Path == "/wifi" && s.ClientReady() {
					http.Redirect(w, r, "/setup", http.StatusSeeOther)
				} else {
					http.Error(w, "connectez-vous au hotspot du lapin", http.StatusForbidden)
				}
				return
			}
		}
		next(w, r)
	}
}

func (a *App) wifiPage(w http.ResponseWriter, r *http.Request) {
	s, err := network.Read(r.Context())
	if err != nil {
		a.render(w, r, "wifi", "Wi-Fi", map[string]any{"Unavailable": true})
		return
	}
	a.wifiMu.Lock()
	reserved := a.wifiBrowser(r)
	a.wifiMu.Unlock()
	authorized := a.auth.Configured()
	if !authorized && reserved {
		token := a.wifiToken(r)
		authorized, _ = network.Authorized(r.Context(), token)
	}
	a.render(w, r, "wifi", "Wi-Fi", map[string]any{"Snapshot": s, "Reserved": reserved, "Authorized": authorized, "Setup": !a.auth.Configured()})
}

// wifiBrowser is called with wifiMu held. The server owns the lease; a
// browser-supplied value is only compared to a freshly generated random nonce.
func (a *App) wifiBrowser(r *http.Request) bool {
	c, err := r.Cookie(wifiCookie)
	return err == nil && a.wifi != nil && time.Now().Before(a.wifi.expires) && subtle.ConstantTimeCompare([]byte(c.Value), []byte(a.wifi.browser)) == 1
}

func (a *App) wifiToken(r *http.Request) string {
	a.wifiMu.Lock()
	defer a.wifiMu.Unlock()
	if a.wifiBrowser(r) {
		return a.wifi.token
	}
	return ""
}

func wifiJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func (a *App) wifiStatus(w http.ResponseWriter, r *http.Request) {
	s, err := network.ReadStatus(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	authorized := a.auth.Configured()
	reserved := false
	if token := a.wifiToken(r); token != "" {
		reserved = true
		if !authorized {
			authorized, err = network.Authorized(r.Context(), token)
			if err != nil {
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
		}
	}
	wifiJSON(w, map[string]any{"status": s, "reserved": reserved, "authorized": authorized})
}

func (a *App) wifiReserve(w http.ResponseWriter, r *http.Request) {
	a.wifiMu.Lock()
	defer a.wifiMu.Unlock()
	if a.wifi != nil && time.Now().Before(a.wifi.expires) && !a.wifiBrowser(r) {
		http.Error(w, "un autre navigateur configure le Wi-Fi, réessayez dans cinq minutes", http.StatusConflict)
		return
	}
	browser, token := "", ""
	if a.wifiBrowser(r) {
		browser, token = a.wifi.browser, a.wifi.token
	}
	if browser == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		browser = hex.EncodeToString(b)
	}
	lease, err := network.Reserve(r.Context(), token)
	if err == nil && token != "" && lease != token {
		err = network.ErrUnavailable
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	attempt := uint64(0)
	if a.wifi != nil && a.wifi.browser == browser {
		attempt = a.wifi.attempt
	}
	a.wifi = &wifiSession{browser: browser, token: lease, expires: time.Now().Add(wifiLeaseTTL), attempt: attempt}
	http.SetCookie(w, &http.Cookie{Name: wifiCookie, Value: browser, Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: int(wifiLeaseTTL.Seconds())})
	if r.Header.Get("Accept") == "application/json" {
		wifiJSON(w, map[string]bool{"reserved": true})
	} else {
		http.Redirect(w, r, "/wifi", http.StatusSeeOther)
	}
}

func (a *App) wifiRelease(w http.ResponseWriter, r *http.Request) {
	a.wifiMu.Lock()
	defer a.wifiMu.Unlock()
	if !a.wifiBrowser(r) {
		http.Error(w, "réservation absente ou expirée", http.StatusForbidden)
		return
	}
	if err := network.Release(r.Context(), a.wifi.token); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	a.wifi = nil
	http.SetCookie(w, &http.Cookie{Name: wifiCookie, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	a.render(w, r, "wifi-released", "Hotspot libéré", nil)
}

func (a *App) wifiScan(w http.ResponseWriter, r *http.Request) {
	err := network.Scan(r.Context())
	if err != nil {
		err = errors.New("scan temporairement indisponible ; utilisez les derniers résultats ou saisissez le nom du réseau")
	}
	back(w, r, "/wifi", err, "Scan demandé ; rechargez la page dans quelques secondes")
}

func wifiForm(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if r.ParseForm() != nil {
		return errors.New("formulaire Wi-Fi invalide")
	}
	return nil
}

func wifiCandidate(r *http.Request) (network.SSID, string, string, string, error) {
	f := r.PostForm.Get
	bad := errors.New("nom de réseau, sécurité ou mot de passe Wi-Fi invalide")
	uuid := f("uuid")
	if uuid != "" {
		if !profileUUID.MatchString(uuid) {
			return nil, "", "", "", bad
		}
		return nil, "", "", uuid, nil
	}
	ssid := []byte(f("ssid"))
	if raw := f("ssid_bytes"); raw != "" {
		var err error
		ssid, err = base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return nil, "", "", "", bad
		}
	}
	security, password := f("security"), f("password")
	if len(ssid) < 1 || len(ssid) > 32 || strings.ContainsRune(password, 0) {
		return nil, "", "", "", bad
	}
	switch security {
	case "open":
		if password != "" {
			return nil, "", "", "", bad
		}
	case "wpa-psk":
		if len(password) == 64 {
			if _, err := hex.DecodeString(password); err != nil {
				return nil, "", "", "", bad
			}
		} else if len(password) < 8 || len(password) > 63 {
			return nil, "", "", "", bad
		}
	case "sae":
		if len(password) < 1 || len(password) > 63 {
			return nil, "", "", "", bad
		}
	default:
		return nil, "", "", "", bad
	}
	return ssid, security, password, "", nil
}

func (a *App) wifiConnect(w http.ResponseWriter, r *http.Request) {
	if err := wifiForm(w, r); err != nil {
		back(w, r, "/wifi", err, "")
		return
	}
	ssid, security, password, uuid, err := wifiCandidate(r)
	if err != nil {
		back(w, r, "/wifi", err, "")
		return
	}
	a.wifiMu.Lock()
	defer a.wifiMu.Unlock()
	token := ""
	if !a.auth.Configured() {
		if !a.wifiBrowser(r) {
			http.Error(w, "préparez d'abord la connexion", http.StatusForbidden)
			return
		}
		token = a.wifi.token
		ok, err := network.Authorized(r.Context(), token)
		if err != nil || !ok {
			http.Error(w, "appuyez sur le bouton du lapin pour autoriser cette tentative pendant cinq minutes", http.StatusForbidden)
			return
		}
		// This press belongs to Wi-Fi, even if Connect's reply is lost.
		a.auth.DisarmPresence()
	} else if !a.auth.Valid(r) {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	// Submit independently of the browser disconnect. Rust returns before the
	// radio switch and owns the attempt, timeout, checkpoint and recovery.
	id, err := network.Connect(context.Background(), ssid, security, password, uuid, token)
	if err != nil {
		back(w, r, "/wifi", err, "")
		return
	}
	if a.wifiBrowser(r) {
		a.wifi.attempt = id
	}
	a.render(w, r, "wifi-transition", "Connexion Wi-Fi en cours", map[string]any{"Attempt": id, "Setup": !a.auth.Configured()})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func (a *App) wifiCancel(w http.ResponseWriter, r *http.Request) {
	if err := wifiForm(w, r); err != nil {
		back(w, r, "/wifi", err, "")
		return
	}
	id, err := strconv.ParseUint(r.PostForm.Get("attempt_id"), 10, 64)
	if err != nil || id == 0 {
		http.Error(w, "tentative invalide", http.StatusBadRequest)
		return
	}
	if !a.auth.Configured() {
		a.wifiMu.Lock()
		ok := a.wifiBrowser(r) && a.wifi.attempt == id
		a.wifiMu.Unlock()
		if !ok {
			http.Error(w, "tentative non autorisée", http.StatusForbidden)
			return
		}
	}
	back(w, r, "/wifi", network.Cancel(context.Background(), id), "Annulation demandée")
}

func (a *App) wifiForget(w http.ResponseWriter, r *http.Request) {
	if !a.auth.Configured() || !a.auth.Valid(r) {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if err := wifiForm(w, r); err != nil {
		back(w, r, "/wifi", err, "")
		return
	}
	uuid := r.PostForm.Get("uuid")
	if !profileUUID.MatchString(uuid) {
		http.Error(w, "profil invalide", http.StatusBadRequest)
		return
	}
	back(w, r, "/wifi", network.Forget(r.Context(), uuid), "Profil oublié")
}

func (a *App) setupNetwork(w http.ResponseWriter, r *http.Request) bool {
	a.wifiMu.Lock()
	defer a.wifiMu.Unlock()
	if a.auth.Configured() {
		return true
	}
	s, err := network.ReadStatus(r.Context())
	if err != nil {
		a.auth.DisarmPresence()
		http.Error(w, network.ErrUnavailable.Error(), http.StatusServiceUnavailable)
		return false
	}
	if !s.ClientReady() || s.Phase == "connecting" {
		a.auth.DisarmPresence()
		http.Redirect(w, r, "/wifi", http.StatusSeeOther)
		return false
	}
	if !a.auth.ArmPresence() {
		http.Error(w, network.ErrUnavailable.Error(), http.StatusServiceUnavailable)
		return false
	}
	return true
}
