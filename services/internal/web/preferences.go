package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/guilhem/nabos/services/internal/config"
)

const maxPreferences = 16 << 10

type Preferences struct {
	Name           string   `json:"name"`
	AllowedDomains []string `json:"allowed_domains"`
	SessionHours   int      `json:"session_hours"`
}

func defaultPreferences() Preferences {
	return Preferences{Name: "Nabaztag", SessionHours: 168}
}

func (p *Preferences) validate() error {
	p.Name = strings.TrimSpace(p.Name)
	if !utf8.ValidString(p.Name) || utf8.RuneCountInString(p.Name) < 1 || utf8.RuneCountInString(p.Name) > 64 || strings.ContainsFunc(p.Name, unicode.IsControl) {
		return errors.New("le nom doit contenir de 1 à 64 caractères sans caractères de contrôle")
	}
	if p.SessionHours != 1 && p.SessionHours != 24 && p.SessionHours != 168 {
		return errors.New("choisissez une durée de connexion de 1 heure, 1 jour ou 7 jours")
	}
	if len(p.AllowedDomains) > 32 {
		return errors.New("indiquez au maximum 32 domaines")
	}
	for i, domain := range p.AllowedDomains {
		domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
		if len(domain) == 0 || len(domain) > 253 || net.ParseIP(domain) != nil {
			return fmt.Errorf("domaine invalide %q : indiquez un nom sans protocole, port ni chemin", domain)
		}
		for _, label := range strings.Split(domain, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' || strings.ContainsFunc(label, func(c rune) bool {
				return !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-')
			}) {
				return fmt.Errorf("domaine invalide %q : indiquez un nom exact sans joker", domain)
			}
		}
		p.AllowedDomains[i] = domain
	}
	return nil
}

// PreferencesPolicy leaves rollback-sensitive application settings untouched.
type PreferencesPolicy struct {
	mu    sync.Mutex
	path  string
	value Preferences
	valid bool
	err   error
}

func readPreferences(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxPreferences+1))
	if err == nil && len(raw) > maxPreferences {
		err = errors.New("préférences trop volumineuses")
	}
	return raw, err
}

func LoadPreferences(path string) *PreferencesPolicy {
	p := &PreferencesPolicy{path: path, value: defaultPreferences()}
	raw, err := readPreferences(path)
	if errors.Is(err, os.ErrNotExist) {
		p.valid = true
		return p
	}
	var candidate Preferences
	if err == nil {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		err = dec.Decode(&candidate)
		if err == nil {
			var trailing any
			if dec.Decode(&trailing) != io.EOF {
				err = errors.New("contenu supplémentaire après les préférences")
			}
		}
		if err == nil {
			err = candidate.validate()
		}
	}
	if err != nil {
		p.err = fmt.Errorf("préférences illisibles : accès limité à l’IP directe du lapin jusqu’à leur réenregistrement (%w)", err)
	} else {
		p.value, p.valid = candidate, true
	}
	return p
}

func (p *PreferencesPolicy) Current() (Preferences, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	value := p.value
	value.AllowedDomains = append([]string(nil), value.AllowedDomains...)
	return value, p.err
}

func (p *PreferencesPolicy) Save(value Preferences) (bool, error) {
	value.AllowedDomains = append([]string(nil), value.AllowedDomains...)
	if err := value.validate(); err != nil {
		return false, err
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return false, err
	}
	raw = append(raw, '\n')
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.commit(value, raw, config.WriteFile)
}

// Called with mu held; a visible rename applies even if directory sync fails.
func (p *PreferencesPolicy) commit(value Preferences, raw []byte, write func(string, []byte) error) (bool, error) {
	err := write(p.path, raw)
	visible := err == nil
	if !visible {
		stored, readErr := readPreferences(p.path)
		visible = readErr == nil && bytes.Equal(stored, raw)
	}
	if visible {
		p.value, p.valid, p.err = value, true, nil
		if err != nil {
			p.err = fmt.Errorf("préférences enregistrées et appliquées, mais leur conservation après une coupure de courant est incertaine (%w)", err)
			err = p.err
		}
	}
	return visible, err
}

func directAddress(r *http.Request, host string) bool {
	external, ok := r.Context().Value(proxyContextKey{}).(proxyRequest)
	if !ok || external.trusted {
		return false
	}
	if _, err := netip.ParseAddrPort(r.RemoteAddr); err != nil {
		return false
	}
	local, ok := r.Context().Value(http.LocalAddrContextKey).(*net.TCPAddr)
	ip := net.ParseIP(host)
	return ok && local != nil && local.IP != nil && !local.IP.IsUnspecified() && ip != nil && ip.Equal(local.IP)
}

func (p *PreferencesPolicy) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Boot qualification must remain reachable even with invalid preferences.
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.URL.Path == "/healthz" && Loopback(r) {
			next.ServeHTTP(w, r)
			return
		}
		p.mu.Lock()
		value, valid := p.value, p.valid
		p.mu.Unlock()
		u, _ := url.Parse("http://" + r.Host) // The outer Proxy middleware validated the authority.
		host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
		allowed := valid && len(value.AllowedDomains) == 0
		if valid {
			for _, domain := range value.AllowedDomains {
				allowed = allowed || host == domain
			}
		}
		if !allowed && !directAddress(r, host) {
			http.Error(w, "domaine non autorisé : utilisez directement l’IP locale du lapin", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
