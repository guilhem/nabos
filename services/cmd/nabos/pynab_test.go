package main

import (
	"context"
	"github.com/guilhem/nabos/services/internal/hardware"
	"path/filepath"
	"testing"
	"time"

	"github.com/guilhem/nabos/services/internal/config"
	"github.com/guilhem/nabos/services/internal/pynab"
)

func TestRandomSchedulesPersistAndSkipExpiredJobs(t *testing.T) {
	a := testApp(t)
	now := time.Now().UTC()
	f := appFixture(t, a)
	f.Mu.Lock()
	f.ClockQuality = "unknown"
	f.Mu.Unlock()
	a.servicesTick(now)
	if !a.store.Get().Services.NextSurprise.IsZero() {
		t.Fatal("scheduled with unknown clock")
	}
	f.Mu.Lock()
	f.ClockQuality = "ntp"
	f.Mu.Unlock()
	a.servicesTick(now)
	s := a.store.Get().Services
	for _, job := range []struct {
		name string
		next time.Time
		freq int
	}{{"taichi", s.NextTaichi, s.TaichiFrequency}, {"surprise", s.NextSurprise, s.SurpriseFrequency}} {
		if job.next.Before(now.Add(pynab.Delay(job.name, job.freq, 0))) || job.next.After(now.Add(pynab.Delay(job.name, job.freq, 1))) {
			t.Fatalf("%s deadline: %v", job.name, job.next)
		}
	}
	reopened, err := config.Open(filepath.Join(a.env.DataDir, "application.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.Get().Services.NextSurprise.Equal(s.NextSurprise) {
		t.Fatal("deadline lost on restart")
	}
	if _, err = a.store.Update(func(s *config.Settings) error {
		s.Services.TaichiFrequency = 0
		s.Services.NextTaichi = time.Time{}
		s.Services.NextSurprise = now.Add(-24 * time.Hour)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a.servicesTick(now)
	s = a.store.Get().Services
	if !s.NextTaichi.IsZero() || !s.NextSurprise.After(now) {
		t.Fatal("disabled or expired schedule mishandled")
	}
}

func TestDisabledServicesRejectEveryEntryPoint(t *testing.T) {
	a := testApp(t)
	_, err := a.store.Update(func(s *config.Settings) error {
		s.Services.Eightball = false
		s.Services.Books = false
		s.Services.Radio = false
		s.Services.IFTTT = false
		s.Services.Webhooks = false
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"eightball", "airquality"} {
		if err := a.performService(context.Background(), name, "default", ""); err == nil {
			t.Fatal(name)
		}
	}
	for _, name := range []string{"radio", "ifttt", "webhook"} {
		if err := a.performTagAction(context.Background(), name, "01:02:03:04", "https://example.com"); err == nil {
			t.Fatal(name)
		}
	}
	for _, name := range []string{"book", "eightball"} {
		if err := a.startInteraction(name, "default/9782070548064"); err == nil {
			t.Fatal(name)
		}
	}
	if len(a.mediaGate) != 0 || a.interaction != nil || a.radioCancel != nil {
		t.Fatal("disabled service reserved playback")
	}
}

func TestHardwareTagsPreserveProductPayload(t *testing.T) {
	raw := hardware.Tag{Tech: "st25tb", UID: []byte{0xd0, 2, 0x18, 1, 2, 3, 4, 5}, Support: "formatted", Formatted: true, Picture: 7, App: 4, Data: append([]byte("default/9782092512593"), 0xff, 0xff)}
	tag := tagFromHardware(raw)
	if tag.UID != "d0:02:18:01:02:03:04:05" || tag.App != "book" || tag.Data != "default/9782092512593" {
		t.Fatal(tag)
	}
	if _, _, err := pynab.ParseBook(tag.Data); err != nil {
		t.Fatal("RFID padding broke book", err)
	}
	payload := tag.payload()
	for _, key := range []string{"v", "time", "event", "tech", "uid", "support", "locked", "picture", "app", "data"} {
		if _, ok := payload[key]; !ok {
			t.Fatal("HA tag field lost", key)
		}
	}
	raw.Removed = true
	payload = tagFromHardware(raw).payload()
	if len(payload) != 5 || payload["event"] != "removed" {
		t.Fatal("removed tag format changed", payload)
	}
	raw.Removed = false
	raw.Formatted = false
	payload = tagFromHardware(raw).payload()
	if _, ok := payload["app"]; ok {
		t.Fatal("unformatted app invented", payload)
	}
	raw.Formatted = true
	raw.App = 255
	payload = tagFromHardware(raw).payload()
	if _, ok := payload["app"]; ok {
		t.Fatal("empty tag app invented", payload)
	}
}
