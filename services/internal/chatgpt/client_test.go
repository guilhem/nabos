package chatgpt

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"golang.org/x/oauth2"
)

const testClientID = "oaiapp_test_registration"

// The transport serves an in-memory OpenID provider. A request to any other
// host (including a pasted loopback URL) fails the test without opening a socket.
type localProvider struct {
	t                  *testing.T
	key                *rsa.PrivateKey
	signer             jose.Signer
	authorization      url.Values
	identity           map[string]any
	scopes             string
	accessOnly         bool
	tokenError         string
	tokenRedirect      bool
	badEndpoint        string
	revocationStatus   int
	exchanges, revokes int
}

func newFixture(t *testing.T) (*Client, *localProvider) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithHeader("kid", "local"))
	if err != nil {
		t.Fatal(err)
	}
	p := &localProvider{
		t: t, key: key, signer: signer, identity: map[string]any{},
		scopes:           "openid profile email offline_access resource.invoke " + planScope,
		revocationStatus: http.StatusOK,
	}
	c := New(t.TempDir())
	c.http.Transport = p
	return c, p
}

func (p *localProvider) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Host != "auth.openai.com" || r.URL.Scheme != "https" || r.URL.RawQuery != "" {
		p.t.Error("request escaped the configured issuer")
		return nil, errors.New("unknown test host")
	}
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		metadata := map[string]any{
			"issuer": issuer, "authorization_endpoint": issuer + "/authorize",
			"token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys",
			"revocation_endpoint": issuer + "/revoke", "id_token_signing_alg_values_supported": []string{"RS256"},
		}
		if p.badEndpoint != "" {
			metadata[p.badEndpoint] = "https://other.invalid/example"
		}
		json.NewEncoder(w).Encode(metadata)
	case "/keys":
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{
			Keys: []jose.JSONWebKey{{Key: &p.key.PublicKey, KeyID: "local", Use: "sig", Algorithm: "RS256"}},
		})
	case "/token":
		p.exchanges++
		if err := r.ParseForm(); err != nil {
			p.t.Fatal(err)
		}
		if r.Method != http.MethodPost || r.Form.Get("grant_type") != "authorization_code" ||
			r.Form.Get("client_id") != testClientID || r.Form.Get("resource") != resource ||
			r.Form.Get("redirect_uri") != p.authorization.Get("redirect_uri") ||
			oauth2.S256ChallengeFromVerifier(r.Form.Get("code_verifier")) != p.authorization.Get("code_challenge") ||
			r.Form.Get("code") != "local-private-code" || r.Form.Has("client_secret") || r.Header.Get("Authorization") != "" {
			p.t.Fatal("invalid public-client code exchange")
		}
		if p.tokenRedirect {
			w.Header().Set("Location", "https://other.invalid/token")
			w.WriteHeader(http.StatusTemporaryRedirect)
			break
		}
		if p.tokenError != "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": p.tokenError, "error_description": "local-private-code"})
			break
		}
		claims := map[string]any{
			"iss": issuer, "aud": testClientID, "sub": "test-subject", "email": "owner@example.test",
			"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": p.authorization.Get("nonce"),
		}
		for k, v := range p.identity {
			claims[k] = v
		}
		id, err := jwt.Signed(p.signer).Claims(claims).Serialize()
		if err != nil {
			p.t.Fatal(err)
		}
		tokens := map[string]any{
			"access_token": "local-private-access", "refresh_token": "local-private-refresh",
			"id_token": id, "scope": p.scopes, "token_type": "Bearer", "expires_in": 3600,
		}
		if p.accessOnly {
			delete(tokens, "refresh_token")
		}
		json.NewEncoder(w).Encode(tokens)
	case "/revoke":
		p.revokes++
		if err := r.ParseForm(); err != nil {
			p.t.Fatal(err)
		}
		wantToken, wantHint := "local-private-refresh", "refresh_token"
		if p.accessOnly {
			wantToken, wantHint = "local-private-access", "access_token"
		}
		if r.Method != http.MethodPost || r.Form.Get("token_type_hint") != wantHint ||
			r.Form.Get("token") != wantToken || r.Form.Get("client_id") != testClientID {
			p.t.Fatal("invalid revocation request")
		}
		w.WriteHeader(p.revocationStatus)
	default:
		p.t.Fatal("unexpected network path")
	}
	return w.Result(), nil
}

func (p *localProvider) start(c *Client, owner string) url.Values {
	p.t.Helper()
	link, err := c.Start(context.Background(), owner)
	if err != nil {
		p.t.Fatal(err)
	}
	u, err := url.Parse(link)
	if err != nil || u.Scheme+"://"+u.Host+u.Path != issuer+"/authorize" {
		p.t.Fatal("unexpected authorization endpoint")
	}
	q := u.Query()
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || redirect.Scheme != "http" || redirect.Hostname() != "127.0.0.1" ||
		redirect.Port() == "" || redirect.Path != "/auth/callback" || redirect.RawQuery != "" {
		p.t.Fatal("unexpected callback address")
	}
	if q.Get("nonce") == "" || q.Get("state") == "" || q.Get("nonce") == q.Get("state") ||
		q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" ||
		q.Get("response_type") != "code" || q.Get("resource") != resource ||
		q.Get("scope") != "openid profile email offline_access resource.invoke "+planScope ||
		!strings.HasPrefix(q.Get("ext_agent_host_id"), "urn:uuid:") {
		p.t.Fatal("missing authorization proof or plan parameters")
	}
	p.authorization = q
	return q
}

func callback(q url.Values) string {
	values := url.Values{"state": {q.Get("state")}, "code": {"local-private-code"}}
	if q.Get("client_id") == dynamicClientID {
		values.Set("client_id", testClientID)
	}
	return q.Get("redirect_uri") + "?" + values.Encode()
}

func TestLinkRestartAndDisconnect(t *testing.T) {
	c, p := newFixture(t)
	const owner = "private-admin-cookie"
	initial := p.start(c, owner)
	if initial.Get("client_id") != dynamicClientID || initial.Get("agent_name_hint") != "NabOS" {
		t.Fatal("missing dynamic registration")
	}
	checkStatus(t, c, owner, true, "")
	checkStatus(t, c, "second-admin", false, "")
	cb := callback(initial) + "&session_state=provider-extension&another=ignored"
	if err := c.Complete(t.Context(), "second-admin", cb); !errors.Is(err, errAttempt) {
		t.Fatal("another browser consumed an attempt")
	}
	if err := c.Complete(t.Context(), owner, cb); err != nil {
		t.Fatal(err)
	}
	if err := c.Complete(t.Context(), owner, cb); !errors.Is(err, errAttempt) || p.exchanges != 1 {
		t.Fatal("callback could be replayed")
	}
	restarted := New(c.dataDir)
	restarted.http.Transport = p
	checkStatus(t, restarted, "second-admin", false, "owner@example.test")
	checkStatus(t, restarted, "", false, "")
	path := filepath.Join(c.directory(), "account.json")
	for name, mode := range map[string]os.FileMode{path: 0o600, c.directory(): 0o700} {
		info, err := os.Stat(name)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatal("incorrect account storage permissions")
		}
	}
	readRecord := func() string {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	record := readRecord()
	if strings.Contains(record, owner) || strings.Contains(record, initial.Get("state")) ||
		strings.Contains(record, "local-private-code") || !strings.Contains(record, "local-private-refresh") {
		t.Fatal("incorrect persisted account or transaction information")
	}
	if err := restarted.Disconnect(t.Context()); err != nil || p.revokes != 1 {
		t.Fatal("renewable session was not revoked", err)
	}
	if strings.Contains(readRecord(), "local-private-") || strings.Contains(readRecord(), "id_token") {
		t.Fatal("sign-out retained credentials")
	}
	checkStatus(t, restarted, owner, false, "")
	returning := p.start(restarted, owner)
	if returning.Get("client_id") != testClientID || returning.Has("agent_name_hint") ||
		returning.Has("id_token_hint") || returning.Get("ext_agent_host_id") != initial.Get("ext_agent_host_id") {
		t.Fatal("returning account did not retain its registration and host identity")
	}
	if err := restarted.Complete(t.Context(), owner, callback(returning)); err != nil {
		t.Fatal(err)
	}
	checkStatus(t, restarted, owner, false, "owner@example.test")
}

func checkStatus(t *testing.T, c *Client, owner string, pending bool, account string) {
	t.Helper()
	s, err := c.Status(owner)
	if err != nil || (s.AuthorizationURL != "") != pending || s.Account != account {
		t.Fatal("incorrect public status", err)
	}
}

func TestRestartAttemptAndExpiry(t *testing.T) {
	c, p := newFixture(t)
	if _, err := c.Start(t.Context(), ""); err == nil {
		t.Fatal("started without a session")
	}
	first := p.start(c, "one")
	second := p.start(c, "one")
	if first.Get("nonce") == second.Get("nonce") || first.Get("state") == second.Get("state") ||
		first.Get("code_challenge") == second.Get("code_challenge") {
		t.Fatal("restart reused a proof")
	}
	if err := c.Complete(t.Context(), "one", callback(first)); err == nil {
		t.Fatal("old callback accepted after restart")
	}
	third := p.start(c, "two")
	checkStatus(t, c, "one", false, "")
	if err := c.Complete(t.Context(), "one", callback(second)); err == nil {
		t.Fatal("old owner retained its attempt")
	}
	if err := c.Complete(t.Context(), "one", callback(third)); err == nil {
		t.Fatal("wrong owner accepted")
	}
	c.now = func() time.Time { return time.Now().Add(attemptLifetime) }
	checkStatus(t, c, "two", false, "")
	if err := c.Complete(t.Context(), "two", callback(third)); err == nil || p.exchanges != 0 {
		t.Fatal("expired callback reached token endpoint")
	}
}

func TestConcurrentCallbacksExchangeTheCodeOnce(t *testing.T) {
	c, p := newFixture(t)
	q := p.start(c, "admin")
	cb := callback(q)
	const browsers = 6
	start := make(chan struct{})
	done := make(chan error, browsers)
	ctx := t.Context()
	for range browsers {
		go func() {
			<-start
			done <- c.Complete(ctx, "admin", cb)
		}()
	}
	close(start)
	accepted := 0
	for range browsers {
		switch err := <-done; {
		case err == nil:
			accepted++
		case !errors.Is(err, errAttempt):
			t.Fatal(err)
		}
	}
	if accepted != 1 || p.exchanges != 1 {
		t.Fatal("one callback produced multiple exchanges")
	}
}

func TestTokenRedirectDoesNotForwardTheCode(t *testing.T) {
	c, p := newFixture(t)
	q := p.start(c, "admin")
	p.tokenRedirect = true
	if err := c.Complete(t.Context(), "admin", callback(q)); !errors.Is(err, errGrant) {
		t.Fatal("unexpected result for a redirected token exchange")
	}
	checkStatus(t, c, "admin", false, "")
}

func TestOnlyExactUnambiguousCallbackAccepted(t *testing.T) {
	c, p := newFixture(t)
	q := p.start(c, "admin")
	cb := callback(q)
	for _, bad := range []string{
		strings.Replace(cb, "127.0.0.1", "localhost", 1),
		strings.Replace(cb, "127.0.0.1", "127.0.0.1@other.invalid", 1),
		strings.Replace(cb, "http://", "https://", 1),
		strings.Replace(cb, "/auth/callback", "/auth/%63allback", 1),
		strings.Replace(cb, "/auth/callback", ":1/auth/callback", 1),
		cb + "#", cb + "#private", cb + "&code=other", cb + "&invalid=%xy", cb + "&ext=x&ext=y",
		cb + "&error=access_denied", cb + "&error_description=local-private-code",
		cb + "&iss=https%3A%2F%2Fother.invalid", cb + "&%73tate=" + q.Get("state"),
		strings.Replace(cb, q.Get("state"), "other-state", 1),
		strings.Replace(cb, "client_id="+testClientID+"&", "", 1),
		strings.Replace(cb, testClientID, dynamicClientID, 1),
	} {
		err := c.Complete(t.Context(), "admin", bad)
		if err == nil || strings.Contains(err.Error(), "local-private") {
			t.Fatal("invalid callback was accepted or appeared in an error")
		}
	}
	denied := q.Get("redirect_uri") + "?" + url.Values{
		"error": {"access_denied"}, "state": {q.Get("state")}, "error_description": {"local-private-code"},
	}.Encode()
	if err := c.Complete(t.Context(), "admin", denied); !errors.Is(err, errGrant) {
		t.Fatal("denial did not end the transaction cleanly")
	}
	if err := c.Complete(t.Context(), "admin", cb); err == nil || p.exchanges != 0 {
		t.Fatal("denied or invalid callback reached token endpoint")
	}
}

func TestFailedExchangeRetainsRegistrationWithoutGrant(t *testing.T) {
	c, p := newFixture(t)
	initial := p.start(c, "admin")
	p.tokenError = "invalid_grant"
	if err := c.Complete(t.Context(), "admin", callback(initial)); !errors.Is(err, errGrant) {
		t.Fatal("unexpected result of expired authorization code")
	}
	next := New(c.dataDir)
	next.http.Transport = p
	checkStatus(t, next, "admin", false, "")
	returning := p.start(next, "admin")
	if returning.Get("client_id") != testClientID || returning.Has("agent_name_hint") {
		t.Fatal("interrupted registration was repeated")
	}
	if err := next.Complete(t.Context(), "admin", callback(returning)+"&client_id=other"); err == nil {
		t.Fatal("callback replaced an existing registration")
	}
	p.tokenError = ""
	if err := next.Complete(t.Context(), "admin", callback(returning)); err != nil {
		t.Fatal(err)
	}
}

func TestGrantedScopeAndVerifiedIdentityRequired(t *testing.T) {
	c, p := newFixture(t)
	q := p.start(c, "admin")
	p.scopes = "openid profile email"
	if err := c.Complete(t.Context(), "admin", callback(q)+"&scope="+planScope); !errors.Is(err, ErrConsentRequired) {
		t.Fatal("callback scope overrode actual token grant")
	}
	s, err := c.Status("admin")
	if err != nil || !s.NeedsConsent || s.Account != "owner@example.test" || s.AuthorizationURL != "" {
		t.Fatal("verified identity without plan access was not retained")
	}
	// A second browser session can explicitly request plan access after restart.
	c = New(c.dataDir)
	c.http.Transport = p
	q = p.start(c, "admin")
	if q.Get("prompt") != "consent" || q.Get("client_id") != testClientID {
		t.Fatal("plan consent was not explicitly requested for the saved registration")
	}
	p.identity["sub"] = "wrong-account"
	if err := c.Complete(t.Context(), "admin", callback(q)); !errors.Is(err, errIdentity) {
		t.Fatal("identity-only reauthorization accepted a different account")
	}
	delete(p.identity, "sub")
	p.scopes += " " + planScope
	q = p.start(c, "admin")
	if err := c.Complete(t.Context(), "admin", callback(q)); err != nil {
		t.Fatal(err)
	}
	s, err = c.Status("admin")
	if err != nil || s.NeedsConsent || s.Account != "owner@example.test" {
		t.Fatal("verified plan grant did not re-enable the existing account")
	}
	working, err := os.ReadFile(filepath.Join(c.directory(), "account.json"))
	if err != nil {
		t.Fatal(err)
	}
	for claim, value := range map[string]any{
		"nonce": "other", "aud": "other", "iss": "other", "sub": "other", "azp": "other",
		"exp": time.Now().Add(-time.Minute).Unix(), "at_hash": "different",
	} {
		t.Run(claim, func(t *testing.T) {
			p.identity[claim] = value
			defer delete(p.identity, claim)
			q := p.start(c, "admin")
			if q.Has("prompt") {
				t.Fatal("ordinary reauthorization forced consent")
			}
			if err := c.Complete(t.Context(), "admin", callback(q)); !errors.Is(err, errIdentity) {
				t.Fatal("invalid identity replaced the working account", err)
			}
		})
	}
	// Keep the trusted signing key unchanged. Change only the published JWKS.
	p.key, err = rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	q = p.start(c, "admin")
	if err := c.Complete(t.Context(), "admin", callback(q)); !errors.Is(err, errIdentity) {
		t.Fatal("invalid signature accepted")
	}
	after, err := os.ReadFile(filepath.Join(c.directory(), "account.json"))
	if err != nil || string(after) != string(working) {
		t.Fatal("failed reauthorization changed stored registration or credentials")
	}
}

func TestDeniedRegistrationCanAskForConsentAgain(t *testing.T) {
	c, p := newFixture(t)
	q := p.start(c, "admin")
	denied := func(q url.Values, clientID string) string {
		values := url.Values{"state": {q.Get("state")}, "error": {"access_denied"}}
		if clientID != "" {
			values.Set("client_id", clientID)
		}
		return q.Get("redirect_uri") + "?" + values.Encode()
	}
	if err := c.Complete(t.Context(), "admin", denied(q, testClientID)); !errors.Is(err, ErrConsentRequired) {
		t.Fatal("issued registration was lost on access_denied")
	}
	s, err := c.Status("admin")
	if err != nil || !s.NeedsConsent || s.Account != "" {
		t.Fatal("an unverified account was exposed")
	}
	q = p.start(c, "admin")
	if q.Get("client_id") != testClientID || q.Get("prompt") != "consent" {
		t.Fatal("denied registration could not request consent")
	}
	if err := c.Complete(t.Context(), "admin", denied(q, "different")); !errors.Is(err, errCallback) {
		t.Fatal("denied callback replaced the registered client")
	}
	if err := c.Complete(t.Context(), "admin", denied(q, "")); !errors.Is(err, ErrConsentRequired) {
		t.Fatal("denied returning account did not retain its consent state")
	}
	if err := c.Disconnect(t.Context()); err != nil || p.revokes != 0 || p.exchanges != 0 {
		t.Fatal("a grant was used without successful sign-in")
	}
}

func TestRevocationFailureStillRemovesLocalCredentials(t *testing.T) {
	c, p := newFixture(t)
	q := p.start(c, "admin")
	if err := c.Complete(t.Context(), "admin", callback(q)); err != nil {
		t.Fatal(err)
	}
	p.revocationStatus = http.StatusServiceUnavailable
	if err := c.Disconnect(t.Context()); !errors.Is(err, ErrRevocationUnconfirmed) || p.revokes != 1 {
		t.Fatal("missing remote revocation warning")
	}
	checkStatus(t, c, "admin", false, "")
	if err := c.Disconnect(t.Context()); err != nil || p.revokes != 1 {
		t.Fatal("local sign-out was not idempotent")
	}
}

func TestIdentityOnlySessionRevokesAccessToken(t *testing.T) {
	c, p := newFixture(t)
	p.accessOnly = true
	p.scopes = "openid profile email"
	q := p.start(c, "admin")
	if err := c.Complete(t.Context(), "admin", callback(q)); !errors.Is(err, ErrConsentRequired) {
		t.Fatal("identity-only sign-in failed")
	}
	if err := c.Disconnect(t.Context()); err != nil || p.revokes != 1 {
		t.Fatal("remaining access token was not revoked", err)
	}
	checkStatus(t, c, "admin", false, "")
}

func TestUntrustedDiscoveryAndStorage(t *testing.T) {
	c, p := newFixture(t)
	for _, endpoint := range []string{"issuer", "authorization_endpoint", "token_endpoint", "jwks_uri", "revocation_endpoint"} {
		p.badEndpoint = endpoint
		if _, err := c.Start(t.Context(), "admin"); !errors.Is(err, errProvider) {
			t.Fatal("accepted a discovery endpoint on another origin")
		}
	}
	data := t.TempDir()
	c = New(data)
	c.http.Transport = p
	if err := os.WriteFile(filepath.Join(data, ".volatile"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Start(t.Context(), "admin"); !errors.Is(err, errStorage) {
		t.Fatal("attempt allowed on temporary data partition")
	}
	if _, err := os.Stat(c.directory()); !os.IsNotExist(err) {
		t.Fatal("account storage was created during volatile boot")
	}
	c = New(t.TempDir())
	c.http.Transport = p
	if err := os.Symlink(t.TempDir(), c.directory()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Start(t.Context(), "admin"); !errors.Is(err, errStorage) {
		t.Fatal("followed a symlink in place of the private directory")
	}
}
