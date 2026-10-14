package web

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

type Proxy struct {
	trusted []*net.IPNet
}

// NewProxy trusts only explicitly configured socket peers, never forwarded IPs.
func NewProxy(addresses string) (*Proxy, error) {
	p := &Proxy{}
	if addresses == "" {
		return p, nil
	}
	for _, address := range strings.Split(addresses, ",") {
		address = strings.TrimSpace(address)
		ip := net.ParseIP(address)
		var network *net.IPNet
		if ip != nil {
			bits := 128
			if v4 := ip.To4(); v4 != nil {
				ip, bits = v4, 32
			}
			network = &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}
		} else {
			var err error
			_, network, err = net.ParseCIDR(address)
			if err != nil {
				return nil, fmt.Errorf("adresse de proxy invalide %q : indiquez une IP ou un CIDR", address)
			}
		}
		p.trusted = append(p.trusted, network)
	}
	return p, nil
}

type proxyContextKey struct{}
type proxyRequest struct {
	scheme, prefix string
}

var (
	prefixRe = regexp.MustCompile(`^(/[A-Za-z0-9_-]+)+$`)
	hostRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)
)

func validAuthority(host string) bool {
	u, err := url.Parse("http://" + host)
	if err != nil || u.Host != host || u.User != nil || u.Hostname() == "" || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return false
	}
	if strings.HasPrefix(host, "[") {
		if !strings.Contains(u.Hostname(), ":") || net.ParseIP(u.Hostname()) == nil {
			return false
		}
	} else if !hostRe.MatchString(u.Hostname()) {
		return false
	}
	if strings.Contains(host, ":") {
		if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
			return net.ParseIP(u.Hostname()) != nil
		}
		if _, _, err := net.SplitHostPort(host); err != nil {
			return false
		}
		port, err := strconv.Atoi(u.Port())
		return err == nil && port > 0 && port <= 65535
	}
	return true
}

// Middleware preserves transport and routing information for local policies.
func (p *Proxy) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validAuthority(r.Host) {
			http.Error(w, "invalid Host", http.StatusBadRequest)
			return
		}
		external := proxyRequest{scheme: Scheme(r)}
		peer, err := netip.ParseAddrPort(r.RemoteAddr)
		if err == nil {
			for _, network := range p.trusted {
				if !network.Contains(peer.Addr().AsSlice()) {
					continue
				}
				if values := r.Header.Values("X-Forwarded-Proto"); len(values) != 0 {
					if len(values) != 1 || (values[0] != "http" && values[0] != "https") {
						http.Error(w, "invalid X-Forwarded-Proto", http.StatusBadRequest)
						return
					}
					external.scheme = values[0]
				}
				for _, header := range []string{"X-Forwarded-Prefix", "X-Ingress-Path"} {
					if values := r.Header.Values(header); len(values) != 0 {
						if len(values) != 1 || !prefixRe.MatchString(values[0]) {
							http.Error(w, "invalid "+header, http.StatusBadRequest)
							return
						}
						if external.prefix != "" && external.prefix != values[0] {
							http.Error(w, "conflicting proxy prefixes", http.StatusBadRequest)
							return
						}
						external.prefix = values[0]
					}
				}
				break
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), proxyContextKey{}, external)))
	})
}

func Prefix(r *http.Request) string {
	external, _ := r.Context().Value(proxyContextKey{}).(proxyRequest)
	return external.prefix
}

func Scheme(r *http.Request) string {
	if external, ok := r.Context().Value(proxyContextKey{}).(proxyRequest); ok {
		return external.scheme
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// Path receives an internal absolute path, including any query string.
func Path(r *http.Request, path string) string { return Prefix(r) + path }
