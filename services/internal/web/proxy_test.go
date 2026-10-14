package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTrustedProxyConfiguration(t *testing.T) {
	for _, value := range []string{"", "192.0.2.10", "192.0.2.0/24, 2001:db8::/64", "127.0.0.1,::1"} {
		if _, err := NewProxy(value); err != nil {
			t.Errorf("valid %q: %v", value, err)
		}
	}
	for _, value := range []string{" ", "localhost", "192.0.2.10:80", "192.0.2.0/33", "192.0.2.10,", "*", "https://192.0.2.10", "[::1]"} {
		if _, err := NewProxy(value); err == nil {
			t.Errorf("invalid proxy configuration accepted: %q", value)
		}
	}
}

func TestProxyTrustUsesOnlyRemoteAddr(t *testing.T) {
	for _, tc := range []struct {
		name, config, peer string
		trusted            bool
	}{
		{"disabled", "", "192.0.2.10:4321", false},
		{"exact peer", "192.0.2.10", "192.0.2.10:4321", true},
		{"other LAN peer", "192.0.2.10", "192.0.2.11:4321", false},
		{"no implicit loopback", "192.0.2.10", "127.0.0.1:4321", false},
		{"IPv4 CIDR", "192.0.2.0/28", "192.0.2.15:4321", true},
		{"CIDR boundary", "192.0.2.0/28", "192.0.2.16:4321", false},
		{"IPv6 peer", "2001:db8::10", "[2001:db8::10]:4321", true},
		{"IPv6 CIDR", "2001:db8::/64", "[2001:db8::20]:4321", true},
		{"mapped IPv4", "192.0.2.10", "[::ffff:192.0.2.10]:4321", true},
		{"invalid remote", "192.0.2.10", "192.0.2.10", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewProxy(tc.config)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodGet, "http://public.example/wifi?ok=ready", nil)
			r.RemoteAddr = tc.peer
			r.Header.Set("X-Forwarded-Proto", "https")
			r.Header.Set("X-Ingress-Path", "/api/ingress/rabbit")
			r.Header.Set("X-Forwarded-For", "127.0.0.1, 192.0.2.10")
			r.Header.Set("X-Forwarded-Host", "attacker.example")
			r.Header.Set("Forwarded", "proto=https;host=attacker.example;for=127.0.0.1")
			p.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, got *http.Request) {
				prefix, scheme := "", "http"
				if tc.trusted {
					prefix, scheme = "/api/ingress/rabbit", "https"
				}
				if Prefix(got) != prefix || Scheme(got) != scheme || Path(got, "/login?ok=ready") != prefix+"/login?ok=ready" {
					t.Errorf("external URL: %s %s", Scheme(got), Path(got, "/login?ok=ready"))
				}
				if got.RemoteAddr != r.RemoteAddr || got.Host != r.Host || got.TLS != r.TLS || got.URL != r.URL || Loopback(got) != Loopback(r) {
					t.Error("proxy changed transport, routing or local admission")
				}
			})).ServeHTTP(httptest.NewRecorder(), r)
		})
	}
}

func TestProxyRejectsInvalidTrustedHeaders(t *testing.T) {
	p, _ := NewProxy("192.0.2.10")
	for header, values := range map[string][][]string{
		"X-Forwarded-Proto": {{""}, {"HTTPS"}, {"ftp"}, {"https,http"}, {"https, https"}, {" https"}, {"https "}, {"https", "https"}},
		"X-Ingress-Path":    {{""}, {"/"}, {"https://evil.example"}, {"//evil.example"}, {"/a//b"}, {"/a/"}, {"/a/.."}, {"/a/."}, {"/a%2fb"}, {"/a\\b"}, {"/a?x=b"}, {"/a#b"}, {"/a b"}, {"/a\tb"}, {"/a\nb"}, {"/a,b"}, {"/a", "/b"}},
	} {
		for _, values := range values {
			t.Run(header+"="+values[0], func(t *testing.T) {
				for _, trusted := range []bool{true, false} {
					r := httptest.NewRequest(http.MethodGet, "http://public.example/", nil)
					r.RemoteAddr = "192.0.2.10:4321"
					if !trusted {
						r.RemoteAddr = "192.0.2.11:4321"
					}
					r.Header[header] = values
					w := httptest.NewRecorder()
					p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })).ServeHTTP(w, r)
					want := http.StatusNoContent
					if trusted {
						want = http.StatusBadRequest
					}
					if w.Code != want {
						t.Errorf("trusted=%v: %d, want %d", trusted, w.Code, want)
					}
				}
			})
		}
	}
}

func TestProxyAuthority(t *testing.T) {
	for _, host := range []string{"rabbit.local", "192.0.2.10:80", "public.example:443", "[2001:db8::1]", "[::1]:8080"} {
		if !validAuthority(host) {
			t.Errorf("valid authority refused: %q", host)
		}
	}
	p, _ := NewProxy("")
	for _, host := range []string{"", "evil.example/rabbit", "evil.example?foo", "evil.example#foo", "user@evil.example", "evil\\example", "public.example:", "public.example:99999", "public.example:443:444", "[not-an-ip]:80", "[192.0.2.10]", "public.example\t", "public.example,other.example"} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Host = host
		w := httptest.NewRecorder()
		p.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("invalid host reached handler") })).ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("invalid host %q: %d", host, w.Code)
		}
	}
}

func TestProxyCSRF(t *testing.T) {
	a := newAuth(t)
	p, _ := NewProxy("192.0.2.10")
	h := p.Middleware(a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }), "/login"))
	for _, tc := range []struct {
		name, origin, referer string
		trusted, allowed      bool
	}{
		{"HTTPS origin", "https://public.example", "", true, true},
		{"HTTPS referer", "", "https://public.example/api/ingress/rabbit/login", true, true},
		{"HTTP mismatch", "http://public.example", "", true, false},
		{"different host", "https://evil.example", "", true, false},
		{"different port", "https://public.example:444", "", true, false},
		{"missing proof", "", "", true, false},
		{"null origin", "null", "https://public.example/login", true, false},
		{"invalid origin beats referer", "https://evil.example", "https://public.example/login", true, false},
		{"origin user info", "https://user@public.example", "", true, false},
		{"origin path", "https://public.example/login", "", true, false},
		{"origin query", "https://public.example?foo=bar", "", true, false},
		{"origin fragment", "https://public.example#foo", "", true, false},
		{"untrusted HTTPS spoof", "https://public.example", "", false, false},
		{"untrusted direct HTTP", "http://public.example", "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://public.example/login", nil)
			r.RemoteAddr = "192.0.2.10:4321"
			if !tc.trusted {
				r.RemoteAddr = "192.0.2.11:4321"
			}
			r.Header.Set("X-Forwarded-Proto", "https")
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			r.Header.Set("Referer", tc.referer)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			want := http.StatusForbidden
			if tc.allowed {
				want = http.StatusNoContent
			}
			if w.Code != want {
				t.Errorf("CSRF status: %d, want %d", w.Code, want)
			}
		})
	}
	r := httptest.NewRequest(http.MethodPost, "http://public.example/login", nil)
	r.RemoteAddr = "192.0.2.10:4321"
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("Origin", "")
	r.Header.Set("Referer", "https://public.example/login")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("present empty Origin fell back to Referer: %d", w.Code)
	}
	for _, header := range []string{"Origin", "Referer"} {
		r := httptest.NewRequest(http.MethodPost, "http://public.example/login", nil)
		r.RemoteAddr = "192.0.2.10:4321"
		r.Header.Set("X-Forwarded-Proto", "https")
		r.Header[header] = []string{"https://public.example", "https://evil.example"}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("duplicate %s accepted: %d", header, w.Code)
		}
	}
}

func TestProxySchemeDefaults(t *testing.T) {
	for _, tc := range []struct {
		target, forwarded, config, want string
	}{
		{"http://public.example/", "", "192.0.2.10", "http"},
		{"https://public.example/", "", "192.0.2.10", "https"},
		{"https://public.example/", "http", "", "https"},
		{"http://public.example/", "http", "192.0.2.10", "http"},
		{"https://public.example/", "http", "192.0.2.10", "http"},
	} {
		p, _ := NewProxy(tc.config)
		r := httptest.NewRequest(http.MethodGet, tc.target, nil)
		r.RemoteAddr = "192.0.2.10:4321"
		if tc.forwarded != "" {
			r.Header.Set("X-Forwarded-Proto", tc.forwarded)
		}
		p.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, got *http.Request) {
			if Scheme(got) != tc.want || Prefix(got) != "" || got.TLS != r.TLS {
				t.Errorf("scheme for %s via %q: %s", tc.target, tc.forwarded, Scheme(got))
			}
		})).ServeHTTP(httptest.NewRecorder(), r)
	}
}
