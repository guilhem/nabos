package pynab

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestLegacyTagsAndContent(t *testing.T) {
	for _, app := range []string{"eightball", "surprise"} {
		for _, language := range Languages {
			for _, kind := range SurpriseKinds {
				data, err := Encode(app, language, kind)
				if err != nil {
					t.Fatal(err)
				}
				l, k := Decode(app, data)
				if l != language || (app == "surprise" && k != kind) {
					t.Fatalf("%s %q: %s %s", app, data, l, k)
				}
			}
		}
	}
	if l, k := Decode("surprise", "\xff\xff"); l != "default" || k != "surprise" {
		t.Fatal(l, k)
	}
	if v, err := BookTag("default", "978-2070548064"); err != nil || v != "default/9782070548064" {
		t.Fatal(v, err)
	}
	for _, bad := range []string{"../9782070548064", "default/../../etc", "default/9782070548064/extra", "/9782070548064", strings.Repeat("x", 30) + "/9782070548064"} {
		if _, _, err := ParseBook(bad); err == nil {
			t.Fatal("unsafe book", bad)
		}
	}
	b, _ := json.Marshal(Surprise(time.Date(2026, 2, 14, 12, 0, 0, 0, time.UTC), "fr_FR", ""))
	if !strings.Contains(string(b), "fr_FR/surprise/02-14/*.mp3;fr_FR/surprise/*.mp3") {
		t.Fatal(string(b))
	}
	for _, c := range []struct {
		f      int
		lo, hi time.Duration
	}{{250, 0, 1200 * time.Second}, {125, 1200 * time.Second, 3600 * time.Second}, {50, 3600 * time.Second, 7200 * time.Second}, {30, 7200 * time.Second, 10800 * time.Second}} {
		if Delay("surprise", c.f, 0) != c.lo || Delay("surprise", c.f, 1) != c.hi {
			t.Fatal("frequency", c.f)
		}
	}
	if Delay("taichi", 0, 0.5) != 0 || Delay("taichi", 255, 0.5) >= Delay("taichi", 30, 0.5) {
		t.Fatal("tai-chi frequency")
	}
}
