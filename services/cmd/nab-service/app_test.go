package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/guilhem/nabos/services/internal/clock"
	"github.com/guilhem/nabos/services/internal/config"
	"github.com/guilhem/nabos/services/internal/system"
)

func TestStartupDoesNotReplaySoftwareMute(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(fmt.Sprintf("settings_write_blocked=%v", blocked), func(t *testing.T) {
			a := testApp(t)
			if _, err := a.store.Update(func(s *config.Settings) error { s.Volume = 0; return nil }); err != nil {
				t.Fatal(err)
			}
			if blocked {
				path := filepath.Join(a.env.DataDir, "config.json")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			dir := t.TempDir()
			log := filepath.Join(dir, "volume")
			t.Setenv("NABOS_TEST_VOLUME", log)
			t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
			if err := os.WriteFile(filepath.Join(dir, "wpctl"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$NABOS_TEST_VOLUME\"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			// Startup applies the audio settings before opening the HTTP listener.
			a.env.HTTPAddr = "invalid::address"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := a.Run(ctx); err == nil || !strings.Contains(err.Error(), "invalid::address") {
				t.Fatalf("startup must reach HTTP even if settings cannot be saved: %v", err)
			}
			want := 100
			if blocked {
				want = 0
			}
			got, err := os.ReadFile(log)
			if err != nil || string(got) != "set-volume @DEFAULT_AUDIO_SINK@ 1.00\n" || a.store.Get().Volume != want {
				t.Fatalf("stored mute replayed: command=%q, volume=%d, error=%v", got, a.store.Get().Volume, err)
			}
		})
	}
}

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
	cookie := serviceSession(t, a)
	h := a.routes()
	for _, p := range []string{"/", "/settings", "/updates", "/tags", "/sounds"} {
		r := httptest.NewRequest("GET", p, nil)
		r.AddCookie(cookie)
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
