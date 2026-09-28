// Package triggers sends RFID-triggered requests: IFTTT Maker events and
// generic webhooks (GET). Errors never contain the URL, key or query.
package triggers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	IFTTTURL = "https://maker.ifttt.com"
	maxURL   = 2048
	maxBody  = 64 << 10
)

var errURL = errors.New("invalid URL")

// ValidateURL accepts absolute http/https URLs with a host and no
// credentials. LAN and loopback hosts are allowed.
func ValidateURL(raw string) (*url.URL, error) {
	if raw == "" || len(raw) > maxURL || strings.ContainsFunc(raw, unicode.IsSpace) {
		return nil, errURL
	}
	u, err := url.Parse(raw) // its error would echo the URL
	if err != nil || u.Opaque != "" || u.Hostname() == "" {
		return nil, errURL
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("URL must use http or https")
	}
	if u.User != nil {
		return nil, errors.New("URL must not contain credentials")
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return nil, errURL
		}
	}
	return u, nil
}

type Client struct {
	HTTP     *http.Client
	IFTTTURL string
}

func NewClient() *Client {
	return &Client{HTTP: &http.Client{Timeout: 10 * time.Second}, IFTTTURL: IFTTTURL}
}

var iftttKey = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// IFTTT fires event with value1=uid, value2=❤️, value3=🐇 (as PyNab did).
func (c *Client) IFTTT(ctx context.Context, key, event, uid string) error {
	if !iftttKey.MatchString(key) {
		return errors.New("ifttt: invalid key")
	}
	if event == "" || event == "." || event == ".." || len(event) > 256 || strings.ContainsFunc(event, unicode.IsControl) {
		return errors.New("ifttt: invalid event name")
	}
	q := url.Values{"value1": {uid}, "value2": {"❤️"}, "value3": {"🐇"}}
	raw := strings.TrimSuffix(c.IFTTTURL, "/") + "/trigger/" + url.PathEscape(event) + "/with/key/" + key + "?" + q.Encode()
	return c.get(ctx, raw, "ifttt")
}

// Webhook GETs rawURL after ValidateURL.
func (c *Client) Webhook(ctx context.Context, rawURL string) error {
	u, err := ValidateURL(rawURL)
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	return c.get(ctx, u.String(), "webhook")
}

func (c *Client) get(ctx context.Context, raw, what string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", what, errURL)
	}
	resp, err := guarded(c.HTTP).Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", what, StripURL(err))
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s: HTTP %d", what, resp.StatusCode)
	}
	return nil
}

// StripURL drops the request URL (which may carry secrets) from client errors.
func StripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// CheckRedirect applies ValidateURL to every redirect hop, so a redirect
// cannot add credentials or leave http/https.
func CheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	_, err := ValidateURL(req.URL.String())
	return err
}

// guarded copies an injected client with CheckRedirect enforced.
func guarded(c *http.Client) *http.Client {
	g := *c
	g.CheckRedirect = CheckRedirect
	return &g
}
