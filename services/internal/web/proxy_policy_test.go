package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/guilhem/nabos/services/internal/config"
)

func policyRequest() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://rabbit.local/", nil)
	r.RemoteAddr = "192.0.2.10:4321"
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Ingress-Path", "/api/ingress/rabbit")
	return r
}

func policyScheme(p *ProxyPolicy) string {
	var scheme string
	p.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		scheme = Scheme(r)
	})).ServeHTTP(httptest.NewRecorder(), policyRequest())
	return scheme
}

func TestProxyPolicyLoadSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trusted-proxies")
	p := LoadProxy(path)
	if value, err := p.Current(); value != "" || err != nil || policyScheme(p) != "http" {
		t.Fatalf("missing file must disable trust: %q %v", value, err)
	}
	for _, value := range []string{"192.0.2.10,2001:db8::/64", ""} {
		if applied, err := p.Save(value); !applied || err != nil {
			t.Fatalf("save %q: applied=%v: %v", value, applied, err)
		}
		loaded := LoadProxy(path)
		if got, err := loaded.Current(); got != value || err != nil {
			t.Fatalf("reload: %q %v", got, err)
		}
		wantScheme := "http"
		if value != "" {
			wantScheme = "https"
		}
		if policyScheme(loaded) != wantScheme || policyScheme(p) != wantScheme {
			t.Fatal("persisted and active policy disagree")
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("private file: %v %v", info, err)
		}
	}
	for _, value := range []string{"192.0.2.10,bad", strings.Repeat("a", MaxProxyAddresses+1)} {
		if applied, err := p.Save(value); applied || err == nil {
			t.Fatal("invalid candidate applied")
		}
	}
	if raw, err := os.ReadFile(path); err != nil || len(raw) != 0 || policyScheme(p) != "http" {
		t.Fatal("invalid candidate changed saved policy")
	}
}

func TestProxyPolicyInvalidLoadCanBeRepaired(t *testing.T) {
	for _, raw := range []string{"192.0.2.10,bad", strings.Repeat("a", MaxProxyAddresses+1)} {
		path := filepath.Join(t.TempDir(), "trusted-proxies")
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		p := LoadProxy(path)
		if _, err := p.Current(); err == nil || policyScheme(p) != "http" {
			t.Fatal("invalid file enabled trust or hid error")
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != raw {
			t.Fatal("load modified invalid file")
		}
		if applied, err := p.Save("192.0.2.10"); !applied || err != nil || policyScheme(p) != "https" {
			t.Fatalf("repair: %v %v", applied, err)
		}
		if _, err := p.Current(); err != nil {
			t.Fatal("repair did not clear load error")
		}
	}
	p := LoadProxy(t.TempDir()) // A directory is an unreadable policy file.
	if _, err := p.Current(); err == nil || policyScheme(p) != "http" {
		t.Fatal("unreadable file enabled trust or hid error")
	}
}

func TestProxyPolicyFailedPersistenceKeepsActiveSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trusted-proxies")
	p := LoadProxy(path)
	if _, err := p.Save("192.0.2.10"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if applied, err := p.Save(""); applied || err == nil || policyScheme(p) != "https" {
		t.Fatalf("failed replacement changed active policy: %v %v", applied, err)
	}
}

func TestProxyPolicyUncertainCommit(t *testing.T) {
	for _, renamed := range []bool{false, true} {
		t.Run(fmt.Sprintf("renamed=%v", renamed), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "trusted-proxies")
			p := LoadProxy(path)
			if _, err := p.Save("192.0.2.10"); err != nil {
				t.Fatal(err)
			}
			candidate, _ := NewProxy("")
			uncertain := errors.New("directory fsync failed after rename")
			p.mu.Lock()
			applied, err := p.commit("", candidate, func(path string, raw []byte) error {
				if renamed {
					if err := config.WriteFile(path, raw); err != nil {
						return err
					}
				}
				return uncertain
			})
			p.mu.Unlock()
			if applied != renamed || !errors.Is(err, uncertain) {
				t.Fatalf("uncertain result: %v %v", applied, err)
			}
			value, warning := p.Current()
			if renamed && (value != "" || !errors.Is(warning, uncertain) || !strings.Contains(warning.Error(), "enregistrée et appliquée")) {
				t.Fatalf("visible commit reported as failure: %q %v", value, warning)
			}
			if !renamed && (value != "192.0.2.10" || warning != nil) {
				t.Fatalf("failed write replaced active state: %q %v", value, warning)
			}
			if policyScheme(p) != policyScheme(LoadProxy(path)) {
				t.Fatal("active snapshot disagrees with disk after uncertain commit")
			}
			if _, err := p.Save("192.0.2.11"); err != nil {
				t.Fatal(err)
			}
			if value, err := p.Current(); value != "192.0.2.11" || err != nil {
				t.Fatal("later save reapplied stale state or retained warning")
			}
		})
	}
}

func TestProxyPolicySnapshotSurvivesSave(t *testing.T) {
	p := LoadProxy(filepath.Join(t.TempDir(), "trusted-proxies"))
	for _, value := range []string{"192.0.2.10", ""} {
		before := policyScheme(p)
		h := p.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			if _, err := p.Save(value); err != nil {
				t.Fatal(err)
			}
			if Scheme(r) != before || Path(r, "/settings") != Prefix(r)+"/settings" {
				t.Fatal("save changed an in-flight request's snapshot")
			}
		}))
		h.ServeHTTP(httptest.NewRecorder(), policyRequest())
		if policyScheme(p) == before {
			t.Fatal("subsequent request did not see saved policy")
		}
	}
}

func TestProxyPolicyConcurrentReadSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trusted-proxies")
	p := LoadProxy(path)
	h := p.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if (Scheme(r) == "http" && Prefix(r) != "") || (Scheme(r) == "https" && Prefix(r) != "/api/ingress/rabbit") {
			t.Error("request mixed policies")
		}
	}))
	var wg sync.WaitGroup
	for worker := range 4 {
		wg.Go(func() {
			for range 15 {
				if worker%2 == 0 {
					value := ""
					if worker == 0 {
						value = "192.0.2.10"
					}
					if applied, err := p.Save(value); !applied || err != nil {
						t.Errorf("concurrent save: %v %v", applied, err)
					}
				} else {
					h.ServeHTTP(httptest.NewRecorder(), policyRequest())
					p.Current()
				}
			}
		})
	}
	wg.Wait()
	value, err := p.Current()
	raw, readErr := os.ReadFile(path)
	if err != nil || readErr != nil || string(raw) != value || policyScheme(p) != policyScheme(LoadProxy(path)) {
		t.Fatalf("last save not published: %q %q %v %v", value, raw, err, readErr)
	}
}
