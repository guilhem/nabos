// Package chatgpt links one ChatGPT account to the rabbit using OpenAI's
// public local-client protocol. Share one Client across the settings handlers.
package chatgpt

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	issuer          = "https://auth.openai.com"
	resource        = "https://api.openai.com/v1"
	dynamicClientID = "dynamic_agent_client"
	planScope       = "chatgpt.tokens.use.direct"
	attemptLifetime = 10 * time.Minute
)

var (
	errOwner    = errors.New("an authenticated administrator session is required")
	errAttempt  = errors.New("no matching ChatGPT sign-in attempt")
	errCallback = errors.New("invalid ChatGPT callback")
	errProvider = errors.New("the ChatGPT authorization service is unavailable")
	errGrant    = errors.New("ChatGPT did not grant the requested account access")
	errIdentity = errors.New("could not verify the ChatGPT account")
)

// ErrRevocationUnconfirmed means that local sign-out succeeded. The owner
// can check or revoke the remote authorization in ChatGPT settings.
var ErrRevocationUnconfirmed = errors.New("signed out locally, remote revocation was not confirmed")

// ErrConsentRequired means that the owner can explicitly request plan access
// again for this registration. A valid identity alone does not enable it.
var ErrConsentRequired = errors.New("permission to use the ChatGPT plan is required")

// Status contains only the information an authenticated settings page needs.
type Status struct {
	AuthorizationURL string
	Account          string
	NeedsConsent     bool
}

type Client struct {
	dataDir string
	http    *http.Client
	now     func() time.Time

	// Serialize all account changes. Status can read the atomically replaced
	// account file and immutable attempt while a provider request is in flight.
	mu      sync.Mutex
	pending atomic.Pointer[attempt]
}

func New(dataDir string) *Client {
	return &Client{
		dataDir: dataDir,
		now:     time.Now,
		http: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

type attempt struct {
	owner, authorizationURL string
	hostID                  string
	state, nonce, verifier  string
	expires                 time.Time
	provider                *provider
	config                  oauth2.Config
}

func (c *Client) activeAttempt() *attempt {
	a := c.pending.Load()
	if a != nil && !c.now().Before(a.expires) {
		c.pending.CompareAndSwap(a, nil)
		return nil
	}
	return a
}

// Start returns a URL for the owner's browser. An outstanding attempt belongs
// exclusively to the administrator session that initiated it.
func (c *Client) Start(ctx context.Context, owner string) (string, error) {
	if owner == "" {
		return "", errOwner
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	c.pending.Store(nil)
	s, err := c.prepare()
	if err != nil {
		return "", err
	}
	p, err := c.discover(ctx)
	if err != nil {
		return "", err
	}
	a := &attempt{
		owner: owner, hostID: s.HostID, provider: p, expires: c.now().Add(attemptLifetime),
		state: rand.Text(), nonce: rand.Text(), verifier: oauth2.GenerateVerifier(),
		config: oauth2.Config{
			ClientID:    s.ClientID,
			Endpoint:    p.Endpoint(),
			RedirectURL: callbackURI(),
			Scopes:      []string{oidc.ScopeOpenID, "profile", "email", "offline_access", "resource.invoke", planScope},
		},
	}
	a.config.Endpoint.AuthStyle = oauth2.AuthStyleInParams // public client, no client secret
	options := []oauth2.AuthCodeOption{
		oidc.Nonce(a.nonce), oauth2.S256ChallengeOption(a.verifier),
		oauth2.SetAuthURLParam("resource", resource),
		oauth2.SetAuthURLParam("ext_agent_host_id", s.HostID),
	}
	if a.config.ClientID == "" {
		a.config.ClientID = dynamicClientID
		options = append(options, oauth2.SetAuthURLParam("agent_name_hint", "NabOS"))
	} else if s.NeedsConsent {
		options = append(options, oauth2.SetAuthURLParam("prompt", "consent"))
	}
	// Optional ID-token hints would expose a previous credential in the settings URL.
	a.authorizationURL = a.config.AuthCodeURL(a.state, options...)
	c.pending.Store(a)
	return a.authorizationURL, nil
}

func callbackURI() string {
	// The browser on the owner's device returns to its own loopback. The
	// administrator pastes that address into NabOS, which never visits it.
	var random [2]byte
	rand.Read(random[:])
	port := 49152 + int(binary.BigEndian.Uint16(random[:])%16384)
	return fmt.Sprintf("http://127.0.0.1:%d/auth/callback", port)
}

// Complete consumes one callback and verifies the returned account before
// replacing any previously working registration or credentials.
func (c *Client) Complete(ctx context.Context, owner, callback string) error {
	if owner == "" {
		return errOwner
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	a := c.activeAttempt()
	if a == nil || a.owner != owner {
		return errAttempt
	}
	q, err := a.parseCallback(callback)
	if err != nil {
		return err
	}
	c.pending.Store(nil)
	conf := a.config
	if issued := q.Get("client_id"); issued != "" {
		conf.ClientID = issued
	}
	s, err := c.read()
	if err != nil {
		return err
	}
	if s.HostID != a.hostID || (s.ClientID != "" && s.ClientID != a.config.ClientID) {
		return errAttempt
	}
	if q.Has("error") {
		if q.Get("error") != "access_denied" || conf.ClientID == dynamicClientID {
			return errGrant
		}
		s.ClientID, s.NeedsConsent = conf.ClientID, true
		if err := c.write(s); err != nil {
			return err
		}
		return ErrConsentRequired
	}
	// OpenAI issues the registration in the first callback. Keep it for a
	// fresh PKCE exchange after an interrupted/expired code. It is not an
	// account identity or a connected session until the ID token verifies.
	if s.ClientID == "" {
		s.ClientID = conf.ClientID
		if err := c.write(s); err != nil {
			return err
		}
	}
	ctx = oidc.ClientContext(ctx, c.http)
	token, err := conf.Exchange(ctx, q.Get("code"),
		oauth2.VerifierOption(a.verifier), oauth2.SetAuthURLParam("resource", resource))
	if err != nil {
		// oauth2.RetrieveError may contain provider text and the submitted code.
		var failure *oauth2.RetrieveError
		if s.Subject == "" && errors.As(err, &failure) && failure.ErrorCode == "invalid_client" {
			s.ClientID, s.NeedsConsent = "", false
			if err := c.write(s); err != nil {
				return err
			}
		}
		return errGrant
	}
	s, err = a.verifiedAccount(ctx, s, token, c.now)
	if err != nil {
		return err
	}
	if err := c.write(s); err != nil {
		return err
	}
	if s.NeedsConsent {
		return ErrConsentRequired
	}
	return nil
}

// Status expects the caller to have authenticated the nonempty session. All
// administrators can see the linked account. Only the initiating session can
// see its pending authorization URL.
func (c *Client) Status(owner string) (Status, error) {
	if owner == "" {
		return Status{}, nil
	}
	s, err := c.read()
	if err != nil {
		return Status{}, err
	}
	out := Status{NeedsConsent: s.NeedsConsent}
	if s.Tokens != nil {
		out.Account = s.Email
		if out.Account == "" {
			out.Account = "ChatGPT"
		}
	}
	if a := c.activeAttempt(); a != nil && a.owner == owner {
		out.AuthorizationURL = a.authorizationURL
	}
	return out, nil
}

// Disconnect removes tokens locally, retains the registration for the next
// sign-in, and attempts remote revocation even if the access token has expired.
func (c *Client) Disconnect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending.Store(nil)
	s, err := c.read()
	if err != nil || (s.Tokens == nil && !s.NeedsConsent) {
		return err
	}
	token, hint := "", "refresh_token"
	hadTokens := s.Tokens != nil
	if hadTokens {
		token = s.Tokens.Refresh
		if token == "" {
			token, hint = s.Tokens.Access, "access_token"
		}
	}
	s.Tokens, s.NeedsConsent = nil, false
	if err := c.write(s); err != nil {
		return err
	}
	if !hadTokens {
		return nil
	}
	if token == "" {
		return ErrRevocationUnconfirmed
	}
	p, err := c.discover(ctx)
	if err != nil || p.revocation == "" {
		return ErrRevocationUnconfirmed
	}
	values := url.Values{
		"client_id": {s.ClientID}, "token": {token}, "token_type_hint": {hint},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.revocation, strings.NewReader(values.Encode()))
	if err != nil {
		return ErrRevocationUnconfirmed
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := c.http.Do(request)
	if err != nil {
		return ErrRevocationUnconfirmed
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ErrRevocationUnconfirmed
	}
	return nil
}
