package chatgpt

import (
	"context"
	"crypto/subtle"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type provider struct {
	*oidc.Provider
	revocation string
}

func (c *Client) discover(ctx context.Context) (*provider, error) {
	p, err := oidc.NewProvider(oidc.ClientContext(ctx, c.http), issuer)
	if err != nil {
		return nil, errProvider
	}
	var metadata struct {
		Keys       string `json:"jwks_uri"`
		Revocation string `json:"revocation_endpoint"`
	}
	endpoints := p.Endpoint()
	if p.Claims(&metadata) != nil || !issuerEndpoint(endpoints.AuthURL) ||
		!issuerEndpoint(endpoints.TokenURL) || !issuerEndpoint(metadata.Keys) ||
		(metadata.Revocation != "" && !issuerEndpoint(metadata.Revocation)) {
		return nil, errProvider
	}
	return &provider{Provider: p, revocation: metadata.Revocation}, nil
}

func issuerEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host == "auth.openai.com" && u.User == nil &&
		strings.HasPrefix(u.Path, "/") && u.RawPath == "" && !strings.ContainsAny(raw, "?#\\")
}

// parseCallback is a pure check of the address copied from the browser. Only
// the known token endpoint ever receives the authorization code and verifier.
func (a *attempt) parseCallback(raw string) (url.Values, error) {
	address, query, ok := strings.Cut(raw, "?")
	if !ok || len(raw) > 16<<10 || address != a.config.RedirectURL || strings.ContainsAny(raw, "#\r\n\t") {
		return nil, errCallback
	}
	q, err := url.ParseQuery(query)
	if err != nil || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(a.state)) != 1 {
		return nil, errCallback
	}
	// OAuth responses can carry extensions. Ignore their contents and reject
	// repeated parameters so every field we act on has one interpretation.
	for _, values := range q {
		if len(values) != 1 {
			return nil, errCallback
		}
	}
	if q.Has("iss") && q.Get("iss") != issuer {
		return nil, errCallback
	}
	issued := q.Get("client_id")
	if q.Has("client_id") && (!opaqueValue(issued) || issued == dynamicClientID ||
		(a.config.ClientID != dynamicClientID && issued != a.config.ClientID)) {
		return nil, errCallback
	}
	if q.Has("error") {
		if !opaqueValue(q.Get("error")) || q.Has("code") {
			return nil, errCallback
		}
		return q, nil
	}
	if !opaqueValue(q.Get("code")) || q.Has("error_description") || q.Has("error_uri") {
		return nil, errCallback
	}
	if a.config.ClientID == dynamicClientID && issued == "" {
		return nil, errCallback
	}
	return q, nil
}

func opaqueValue(value string) bool {
	return value != "" && strings.IndexFunc(value, func(r rune) bool { return r < '!' || r > '~' }) == -1
}

// Only an ID token verified for the issued client can establish an account.
// Only the token response's actual grant can enable use of its ChatGPT plan.
func (a *attempt) verifiedAccount(ctx context.Context, previous localAccount, token *oauth2.Token, now func() time.Time) (localAccount, error) {
	if !strings.EqualFold(token.TokenType, "Bearer") || !token.Expiry.After(now()) {
		return previous, errGrant
	}
	rawID, _ := token.Extra("id_token").(string)
	verifier := a.provider.Verifier(&oidc.Config{ClientID: previous.ClientID, Now: now})
	id, err := verifier.Verify(ctx, rawID)
	if err != nil || id.Subject == "" || id.Nonce != a.nonce ||
		(previous.Subject != "" && previous.Subject != id.Subject) ||
		(id.AccessTokenHash != "" && id.VerifyAccessToken(token.AccessToken) != nil) {
		return previous, errIdentity
	}
	var claims struct {
		Email string `json:"email"`
		AZP   string `json:"azp"`
	}
	if id.Claims(&claims) != nil || (claims.AZP != "" && claims.AZP != previous.ClientID) ||
		(len(id.Audience) > 1 && claims.AZP == "") {
		return previous, errIdentity
	}
	granted, _ := token.Extra("scope").(string)
	scopes := strings.Fields(granted)
	needsConsent := !slices.Contains(scopes, planScope)
	if !needsConsent && token.RefreshToken == "" {
		return previous, errGrant
	}
	return localAccount{
		HostID: previous.HostID, ClientID: previous.ClientID, Issuer: id.Issuer,
		Subject: id.Subject, Email: claims.Email, NeedsConsent: needsConsent,
		Tokens: &credentials{
			Access: token.AccessToken, Refresh: token.RefreshToken, ID: rawID,
			Type: token.TokenType, Expiry: token.Expiry, Scopes: scopes,
		},
	}, nil
}
