package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/guilhem/nabos/services/internal/device"
)

func TestUpdateUIUsesRemoteOperationsAndConfig(t *testing.T) {
	a := testApp(t)
	f := appFixture(t, a)
	f.Mu.Lock()
	f.Configured = true
	f.Catalog = []device.Release{{Tag: "v1.1.0", Ready: true, Published: "2026-09-30T10:00:00Z"}}
	f.Mu.Unlock()
	cookie := serviceSession(t, a)
	h := a.routes()
	for _, path := range []string{"/updates/check", "/updates/install", "/updates/settings"} {
		if w := serviceRequest(h, "POST", path, url.Values{}, nil); w.Code != http.StatusUnauthorized {
			t.Fatal(path, w.Code)
		}
	}
	revision, _, _ := a.device.ReadConfig(a.ctx)
	form := url.Values{"revision": {revision}, "mode": {"auto"}, "channel": {"test"}, "start": {"23:00"}, "end": {"02:00"}}
	if w := serviceRequest(h, "POST", "/updates/settings", form, cookie); strings.Contains(w.Header().Get("Location"), "err=") {
		t.Fatal(w.Header())
	}
	if w := serviceRequest(h, "POST", "/updates/check", url.Values{}, cookie); strings.Contains(w.Header().Get("Location"), "err=") {
		t.Fatal(w.Header())
	}
	if w := serviceRequest(h, "POST", "/updates/install", url.Values{"tag": {"v1.1.0"}, "retry": {"true"}}, cookie); strings.Contains(w.Header().Get("Location"), "err=") {
		t.Fatal(w.Header())
	}
	f.Mu.Lock()
	if !f.Settings.Updates.Automatic || f.Checked != 1 || f.InstallTag != "v1.1.0" || f.InstallChannel != "test" || f.InstallAutomatic || !f.InstallRetry {
		t.Fatal("wrong remote arguments", f.Settings, f.InstallTag)
	}
	f.Mu.Unlock()
	page := serviceRequest(h, "GET", "/updates", nil, cookie)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "v1.1.0") || !strings.Contains(page.Body.String(), "30/09/2026") {
		t.Fatal(page.Code, page.Body.String())
	}
}
