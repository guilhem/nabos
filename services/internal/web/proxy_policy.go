package web

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"

	"github.com/guilhem/nabos/services/internal/config"
)

const MaxProxyAddresses = 4096

// ProxyPolicy owns only HTTP trust, separate from rollback-sensitive app settings.
type ProxyPolicy struct {
	mu        sync.Mutex
	path      string
	proxy     *Proxy
	addresses string
	err       error
}

// LoadProxy fails closed without moving or resetting any configuration file.
func LoadProxy(path string) *ProxyPolicy {
	p := &ProxyPolicy{path: path, proxy: &Proxy{}}
	raw, err := readProxyFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return p
	}
	if err == nil {
		p.addresses = string(raw)
		var candidate *Proxy
		candidate, err = NewProxy(p.addresses)
		if err == nil {
			p.proxy = candidate
		}
	}
	if err != nil {
		p.err = fmt.Errorf("accès via proxy désactivé : impossible de charger la configuration (%w)", err)
	}
	return p
}

func readProxyFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, MaxProxyAddresses+1))
	if err == nil && len(raw) > MaxProxyAddresses {
		err = errors.New("liste de proxies trop longue")
	}
	return raw, err
}

func (p *ProxyPolicy) Current() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.addresses, p.err
}

// Save returns whether the candidate is visible and applied, even if durability is uncertain.
func (p *ProxyPolicy) Save(addresses string) (bool, error) {
	if len(addresses) > MaxProxyAddresses {
		return false, errors.New("liste de proxies trop longue")
	}
	candidate, err := NewProxy(addresses)
	if err != nil {
		return false, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.commit(addresses, candidate, config.WriteFile)
}

// Called with mu held: an uncertain commit must never republish a stale policy.
func (p *ProxyPolicy) commit(addresses string, candidate *Proxy, write func(string, []byte) error) (bool, error) {
	raw := []byte(addresses)
	err := write(p.path, raw)
	visible := err == nil
	if !visible {
		stored, readErr := readProxyFile(p.path)
		visible = readErr == nil && bytes.Equal(stored, raw)
	}
	if visible {
		p.proxy, p.addresses, p.err = candidate, addresses, nil
		if err != nil {
			p.err = fmt.Errorf("configuration enregistrée et appliquée, mais sa conservation après une coupure de courant est incertaine (%w)", err)
			err = p.err
		}
	}
	return visible, err
}

func (p *ProxyPolicy) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		snapshot := p.proxy
		p.mu.Unlock()
		snapshot.Middleware(next).ServeHTTP(w, r)
	})
}
