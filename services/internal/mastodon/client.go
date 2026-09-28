// Package mastodon implements the pynab NabPairing direct-message protocol.
package mastodon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// State is persisted by the caller. PairingState is "", "proposed",
// "waiting_approval", or "married". Ear positions are nil until received.
type State struct {
	Instance       string    `json:"instance"`
	ClientID       string    `json:"client_id"`
	ClientSecret   string    `json:"client_secret"`
	RedirectURI    string    `json:"redirect_uri"`
	AccessToken    string    `json:"access_token"`
	AccountID      string    `json:"account_id"`
	Username       string    `json:"username"`
	DisplayName    string    `json:"display_name"`
	Avatar         string    `json:"avatar"`
	SpouseHandle   string    `json:"spouse_handle"`
	PairingState   string    `json:"spouse_pairing_state"`
	PairingDate    time.Time `json:"spouse_pairing_date"`
	LeftEar        *int      `json:"spouse_left_ear_position"`
	RightEar       *int      `json:"spouse_right_ear_position"`
	LastStatusID   string    `json:"last_processed_status_id"`
	LastStatusDate time.Time `json:"last_processed_status_date"`
}

type Account struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	Acct        string `json:"acct"`
	DisplayName string `json:"display_name"`
	Avatar      string `json:"avatar"`
}

type Client struct {
	base string
	http *http.Client
}

var ErrUnauthorized = errors.New("Mastodon authorization expired")

// NewClient accepts an instance hostname or base URL. Plain HTTP is permitted
// only for loopback test servers. Redirects are rejected to keep credentials on
// the chosen instance.
func NewClient(instance string, h *http.Client) (*Client, error) {
	if !strings.Contains(instance, "://") {
		instance = "https://" + instance
	}
	u, err := url.Parse(instance)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid Mastodon instance")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || net.ParseIP(u.Hostname()) != nil && net.ParseIP(u.Hostname()).IsLoopback())) {
		return nil, errors.New("Mastodon instance requires HTTPS")
	}
	if h == nil {
		h = &http.Client{Timeout: 30 * time.Second}
	}
	copy := *h
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{base: u.String(), http: &copy}, nil
}

func (c *Client) Instance() string { u, _ := url.Parse(c.base); return u.Hostname() }

func (c *Client) request(ctx context.Context, method, path, token string, form url.Values, result any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("%w: %s", ErrUnauthorized, path)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Mastodon %s: HTTP %d", path, resp.StatusCode)
	}
	if result != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(result); err != nil {
			return fmt.Errorf("Mastodon %s: %w", path, err)
		}
	}
	return nil
}

func (c *Client) RegisterApp(ctx context.Context, redirectURI string) (string, string, error) {
	if err := validRedirect(redirectURI); err != nil {
		return "", "", err
	}
	var app struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	err := c.request(ctx, http.MethodPost, "/api/v1/apps", "", url.Values{
		"client_name": {"nabmastodond"}, "redirect_uris": {redirectURI},
		"scopes": {"read write"},
	}, &app)
	if err == nil && (app.ClientID == "" || app.ClientSecret == "") {
		err = errors.New("Mastodon app credentials missing")
	}
	return app.ClientID, app.ClientSecret, err
}

// AuthorizeURL requires a caller-generated, unpredictable state. The UI must
// compare it with the callback's state before calling ExchangeCode.
func (c *Client) AuthorizeURL(clientID, redirectURI, state string) (string, error) {
	if clientID == "" || state == "" {
		return "", errors.New("OAuth client ID and state required")
	}
	if err := validRedirect(redirectURI); err != nil {
		return "", err
	}
	q := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirectURI}, "scope": {"read write"}, "state": {state}}
	return c.base + "/oauth/authorize?" + q.Encode(), nil
}

func (c *Client) ExchangeCode(ctx context.Context, clientID, clientSecret, redirectURI, code string) (string, error) {
	if clientID == "" || clientSecret == "" || code == "" {
		return "", errors.New("OAuth credentials and code required")
	}
	if err := validRedirect(redirectURI); err != nil {
		return "", err
	}
	var v struct {
		AccessToken string `json:"access_token"`
	}
	err := c.request(ctx, http.MethodPost, "/oauth/token", "", url.Values{
		"grant_type": {"authorization_code"}, "client_id": {clientID}, "client_secret": {clientSecret},
		"redirect_uri": {redirectURI}, "code": {code}, "scope": {"read write"},
	}, &v)
	if err == nil && v.AccessToken == "" {
		err = errors.New("Mastodon access token missing")
	}
	return v.AccessToken, err
}

func (c *Client) VerifyAccount(ctx context.Context, token string) (Account, error) {
	if token == "" {
		return Account{}, errors.New("access token required")
	}
	var a Account
	err := c.request(ctx, http.MethodGet, "/api/v1/accounts/verify_credentials", token, nil, &a)
	if err == nil && (a.ID == "" || a.Username == "") {
		err = errors.New("Mastodon account missing ID or username")
	}
	return a, err
}

// SetAccount records verified credentials and starts a fresh cursor. On token
// replacement it clears pairing state, matching pynab's reset_access_token.
func (s *State) SetAccount(token string, a Account) error {
	if token == "" || a.ID == "" || a.Username == "" {
		return errors.New("invalid account")
	}
	if s.AccessToken != token {
		s.clearPairing()
		s.LastStatusID = ""
		s.LastStatusDate = time.Now().UTC()
	}
	s.AccessToken, s.AccountID, s.Username, s.DisplayName, s.Avatar = token, a.ID, a.Username, a.DisplayName, a.Avatar
	return nil
}

func (s *State) ClearAccount() {
	s.AccessToken, s.AccountID, s.Username, s.DisplayName, s.Avatar = "", "", "", "", ""
	s.clearPairing()
	s.LastStatusID, s.LastStatusDate = "", time.Time{}
}

func (s *State) clearPairing() {
	s.SpouseHandle, s.PairingState = "", ""
	s.PairingDate = time.Time{}
	s.LeftEar, s.RightEar = nil, nil
}

var handleRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*@[A-Za-z0-9][A-Za-z0-9.-]*[A-Za-z0-9]$`)

func (c *Client) handle(v string) (string, error) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "@")
	if !strings.Contains(v, "@") {
		v += "@" + c.Instance()
	}
	if !handleRE.MatchString(v) || strings.Contains(v, "..") {
		return "", errors.New("invalid Mastodon handle")
	}
	return strings.ToLower(v), nil
}

func validRedirect(raw string) error {
	u, err := url.Parse(raw)
	host := ""
	if err == nil {
		host = u.Hostname()
	}
	ip := net.ParseIP(host)
	local := host == "localhost" || strings.HasSuffix(host, ".local") || ip != nil && (ip.IsLoopback() || ip.IsPrivate())
	if err != nil || host == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && local)) {
		return errors.New("invalid OAuth redirect URI")
	}
	return nil
}

const protocolURL = "https://github.com/nabaztag2018/pynab"

var messages = map[string]string{
	"proposal":    "Would you accept to be my spouse? (NabPairing Proposal - " + protocolURL + ")",
	"acceptation": "Oh yes, I do accept to be your spouse (NabPairing Acceptation - " + protocolURL + ")",
	"rejection":   "Sorry, I cannot be your spouse right now (NabPairing Rejection - " + protocolURL + ")",
	"divorce":     "I think we should split. Can we skip the lawyers? (NabPairing Divorce - " + protocolURL + ")",
}

type Status struct {
	ID         string    `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	Visibility string    `json:"visibility"`
	Content    string    `json:"content"`
	Account    Account   `json:"account"`
}

func (c *Client) send(ctx context.Context, token, target, kind string, left, right int) (Status, error) {
	if token == "" {
		return Status{}, errors.New("access token required")
	}
	target, err := c.handle(target)
	if err != nil {
		return Status{}, err
	}
	message := messages[kind]
	if kind == "ears" {
		if err := validEars(left, right); err != nil {
			return Status{}, err
		}
		message = fmt.Sprintf("Let's dance (NabPairing Ears %d %d - %s)", left, right, protocolURL)
	}
	if message == "" {
		return Status{}, errors.New("unknown NabPairing message")
	}
	var st Status
	err = c.request(ctx, http.MethodPost, "/api/v1/statuses", token, url.Values{"status": {"@" + target + " " + message}, "visibility": {"direct"}}, &st)
	if err == nil && (st.ID == "" || st.CreatedAt.IsZero()) {
		err = errors.New("Mastodon status missing ID or date")
	}
	return st, err
}

func validEars(left, right int) error {
	if left < 0 || left > 16 || right < 0 || right > 16 {
		return errors.New("ear positions must be 0..16")
	}
	return nil
}

func (c *Client) Propose(ctx context.Context, s *State, spouse string) error {
	if s.PairingState != "" {
		return errors.New("already pairing")
	}
	h, err := c.handle(spouse)
	if err != nil {
		return err
	}
	st, err := c.send(ctx, s.AccessToken, h, "proposal", 0, 0)
	if err != nil {
		return err
	}
	s.SpouseHandle, s.PairingState, s.PairingDate = h, "proposed", st.CreatedAt
	return nil
}

func (c *Client) Accept(ctx context.Context, s *State) error {
	if s.PairingState != "waiting_approval" {
		return errors.New("no proposal to accept")
	}
	st, err := c.send(ctx, s.AccessToken, s.SpouseHandle, "acceptation", 0, 0)
	if err != nil {
		return err
	}
	s.PairingState, s.PairingDate = "married", st.CreatedAt
	return nil
}

func (c *Client) Reject(ctx context.Context, s *State) error {
	if s.PairingState != "waiting_approval" {
		return errors.New("no proposal to reject")
	}
	if _, err := c.send(ctx, s.AccessToken, s.SpouseHandle, "rejection", 0, 0); err != nil {
		return err
	}
	s.clearPairing()
	return nil
}

func (c *Client) Divorce(ctx context.Context, s *State) error {
	if s.PairingState != "married" && s.PairingState != "proposed" {
		return errors.New("no pairing to end")
	}
	if _, err := c.send(ctx, s.AccessToken, s.SpouseHandle, "divorce", 0, 0); err != nil {
		return err
	}
	s.clearPairing()
	return nil
}

func (c *Client) SendEars(ctx context.Context, s *State, left, right int) error {
	if s.PairingState != "married" {
		return errors.New("not married")
	}
	if err := validEars(left, right); err != nil {
		return err
	}
	if _, err := c.send(ctx, s.AccessToken, s.SpouseHandle, "ears", left, right); err != nil {
		return err
	}
	s.LeftEar, s.RightEar = &left, &right
	return nil
}

func statusNumber(id string) (uint64, error) { return strconv.ParseUint(id, 10, 64) }
