package main

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFrenchVoiceSettingsInstall(t *testing.T) {
	a := testApp(t)
	a.ctx = t.Context()
	cookie := serviceSession(t, a)
	h := a.routes()
	const path = "/settings/voice/install"
	if w := serviceRequest(h, http.MethodPost, path, url.Values{}, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated install: HTTP %d", w.Code)
	}
	if w := serviceRequest(h, http.MethodPost, path, nil, cookie); w.Code != http.StatusForbidden {
		t.Fatalf("install without request origin: HTTP %d", w.Code)
	}
	settings := serviceRequest(h, http.MethodGet, "/settings", nil, cookie)
	if !strings.Contains(settings.Body.String(), path) || !strings.Contains(settings.Body.String(), "Installer la voix") {
		t.Fatal("voice installation is missing from Settings")
	}
	if err := os.WriteFile(filepath.Join(a.env.DataDir, ".volatile"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if w := serviceRequest(h, http.MethodPost, path, url.Values{}, cookie); w.Code != http.StatusSeeOther {
		t.Fatalf("install did not return to Settings: HTTP %d", w.Code)
	}
	for deadline := time.Now().Add(time.Second); a.frenchVoiceStatus().Fetching; {
		if time.Now().After(deadline) {
			t.Fatal("voice installation did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	settings = serviceRequest(h, http.MethodGet, "/settings", nil, cookie)
	if !strings.Contains(settings.Body.String(), "le stockage de données est volatile") {
		t.Fatal("Settings did not report volatile storage")
	}
}
