package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/guilhem/nabos/services/internal/chatgpt"
)

const signInURL = "https://auth.openai.com/authorize?state=test-attempt"
const testCallback = "http://127.0.0.1:1455/auth/callback?code=test-code"

type testChatGPT struct {
	owner, account string
	needsConsent   bool
}

func (c *testChatGPT) Start(_ context.Context, owner string) (string, error) {
	c.owner = owner
	return signInURL, nil
}

func (c *testChatGPT) Status(owner string) (chatgpt.Status, error) {
	s := chatgpt.Status{Account: c.account, NeedsConsent: c.needsConsent}
	if c.owner != "" && owner == c.owner {
		s.AuthorizationURL = signInURL
	}
	return s, nil
}

func (c *testChatGPT) Complete(_ context.Context, owner, callback string) error {
	if c.owner == "" || c.owner != owner || callback != testCallback {
		return errors.New("test-code must not reach the browser")
	}
	c.owner, c.account = "", "lapin@example.test"
	return nil
}

func (c *testChatGPT) Disconnect(context.Context) error {
	c.owner, c.account = "", ""
	return nil
}

func TestChatGPTSettings(t *testing.T) {
	a := testApp(t)
	account := &testChatGPT{}
	a.chatGPT = account
	cookie := serviceSession(t, a)
	h := a.routes()
	const path = "/settings/chatgpt/"
	// Forwarded HTTPS only counts when the operator trusts the proxy.
	w := ingressRequest(h, "GET", "/settings", nil, cookie)
	if !strings.Contains(w.Body.String(), "HTTPS via un proxy") || strings.Contains(w.Body.String(), path+"start") {
		t.Fatal("HTTP settings should explain the HTTPS prerequisite")
	}
	if w := serviceRequest(h, "POST", path+"start", url.Values{}, cookie); w.Code != http.StatusForbidden {
		t.Fatal("HTTP account setup accepted")
	}
	trustIngress(t, a)
	if w := ingressRequest(h, "POST", path+"start", url.Values{}); w.Code != http.StatusUnauthorized {
		t.Fatal("unauthenticated account setup accepted")
	}
	r := ingressHTTPRequest("POST", path+"start", url.Values{}, cookie)
	r.Header.Set("Origin", "https://other.example")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || account.owner != "" {
		t.Fatal("cross-origin account setup accepted")
	}
	w = ingressRequest(h, "POST", path+"start", url.Values{}, cookie)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != ingressPath+"/settings#chatgpt" || account.owner != cookie.Value {
		t.Fatal("sign-in was not started for this session")
	}
	w = ingressRequest(h, "GET", "/settings", nil, cookie)
	doc := uiDocument(t, w)
	checkUISaveForm(t, doc, ingressPath+path+"complete", false, "callback")
	if w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), signInURL) || !strings.Contains(w.Body.String(), `rel="noopener noreferrer"`) {
		t.Fatal("pending authorization link is missing or cacheable")
	}
	form := url.Values{"callback": {testCallback}}
	w = ingressRequest(h, "POST", path+"complete?callback=test-code", url.Values{}, cookie)
	if w.Code != http.StatusBadRequest || account.account != "" || strings.Contains(w.Body.String(), "test-code") {
		t.Fatal("callback query accepted or reflected")
	}
	w = ingressRequest(h, "POST", path+"complete", url.Values{"callback": {"invalid-test-code"}}, cookie)
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "err=") || strings.Contains(w.Header().Get("Location")+w.Body.String(), "test-code") {
		t.Fatal("failed callback exposed input or provider error")
	}
	w = ingressRequest(h, "POST", path+"complete", form, cookie)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != ingressPath+"/settings#chatgpt" {
		t.Fatal("failed to complete sign-in")
	}
	w = ingressRequest(h, "GET", "/settings", nil, cookie)
	if !strings.Contains(w.Body.String(), "lapin@example.test") || strings.Contains(w.Body.String(), signInURL) {
		t.Fatal("connected account did not replace the pending authorization")
	}
	account.needsConsent = true
	w = ingressRequest(h, "GET", "/settings", nil, cookie)
	if !strings.Contains(w.Body.String(), "Autoriser l’utilisation de mon forfait ChatGPT") {
		t.Fatal("the administrator cannot grant missing plan access")
	}
	if w := ingressRequest(h, "POST", path+"disconnect", url.Values{}, cookie); w.Code != http.StatusSeeOther || account.account != "" {
		t.Fatal("failed to disconnect")
	}
	ingressRequest(h, "POST", path+"start", url.Values{}, cookie)
	ingressRequest(h, "POST", "/logout", url.Values{}, cookie)
	if w := ingressRequest(h, "POST", path+"complete", form, cookie); w.Code != http.StatusUnauthorized || account.account != "" {
		t.Fatal("a logged-out administrator completed account setup")
	}
}
