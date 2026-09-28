package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/guilhem/nabos/services/internal/clock"
	"github.com/guilhem/nabos/services/internal/system"
)

func testApp(t *testing.T) *App {
	dir := t.TempDir()
	a, err := NewApp(Env{MQTTHost: "127.0.0.1", MQTTPort: 1, DataDir: dir,
		TimesyncFile: filepath.Join(dir, "synchronized"), TimesyncClock: filepath.Join(dir, "clock")})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestClockQualityWithoutInternet(t *testing.T) {
	a := testApp(t)
	if q, _ := a.clockQuality(); q != clock.Unknown {
		t.Fatal("no time source must be unknown")
	}
	// Saved clock in the future: the system time was not restored yet.
	os.WriteFile(a.env.TimesyncClock, nil, 0o644)
	os.Chtimes(a.env.TimesyncClock, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	if q, _ := a.clockQuality(); q != clock.Unknown {
		t.Fatal("time behind the saved clock is not trustworthy")
	}
	os.Chtimes(a.env.TimesyncClock, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	if q, s := a.clockQuality(); q != clock.Coarse || s != "restored" {
		t.Fatalf("offline cold boot: %v %s", q, s)
	}
	if err := a.sayTime(); err == nil {
		t.Fatal("the time must not be announced from a coarse clock")
	}
	if id := system.BootID(); id != "" {
		os.WriteFile(a.manualClockFile(), []byte(id+"\n"), 0o640)
		if _, s := a.clockQuality(); s != "manual" {
			t.Fatal("manual time of this boot")
		}
		os.WriteFile(a.manualClockFile(), []byte("previous-boot\n"), 0o640)
		if _, s := a.clockQuality(); s != "restored" {
			t.Fatal("manual time of a previous boot must be ignored")
		}
	}
	os.WriteFile(a.env.TimesyncFile, nil, 0o644)
	if _, s := a.clockQuality(); s != "ntp" {
		t.Fatal("NTP")
	}
}

func TestEveryPageRenders(t *testing.T) {
	a := testApp(t)
	a.auth.MarkPresence()
	tok, err := a.auth.Setup("carotte-42")
	if err != nil {
		t.Fatal(err)
	}
	h := a.routes()
	for _, p := range []string{"/", "/settings", "/updates", "/tags", "/sounds"} {
		r := httptest.NewRequest("GET", p, nil)
		r.AddCookie(&http.Cookie{Name: "nab_session", Value: tok})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "</html>") {
			t.Fatalf("%s: %d %s", p, w.Code, w.Body.String())
		}
	}
	for _, p := range []string{"/login", "/setup"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != 200 && w.Code != http.StatusSeeOther {
			t.Fatalf("%s: %d", p, w.Code)
		}
	}
}
