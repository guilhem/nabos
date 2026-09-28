package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/guilhem/nabos/services/internal/bus"
	"github.com/guilhem/nabos/services/internal/config"
	"github.com/guilhem/nabos/services/internal/update"
	"github.com/guilhem/nabos/services/internal/voice"
)

func TestUpdateWindows(t *testing.T) {
	u := config.Defaults().Updates
	paris, _ := time.LoadLocation("Europe/Paris")
	for _, tc := range []struct {
		at, want   string
		start, end config.HM
	}{
		{"2026-09-29T02:59:00+02:00", "", u.Start, u.End},
		{"2026-09-29T03:00:00+02:00", "2026-09-29", u.Start, u.End},
		{"2026-09-29T05:00:00+02:00", "", u.Start, u.End},
		{"2026-09-30T01:00:00+02:00", "2026-09-29", config.HM{Hour: 23}, config.HM{Hour: 2}},
		{"2026-09-30T02:00:00+02:00", "", config.HM{Hour: 23}, config.HM{Hour: 2}},
		{"2026-10-25T02:30:00+02:00", "2026-10-25", config.HM{Hour: 2}, config.HM{Hour: 4}},
		{"2026-10-25T02:30:00+01:00", "2026-10-25", config.HM{Hour: 2}, config.HM{Hour: 4}},
		{"2026-03-29T03:00:00+02:00", "2026-03-29", config.HM{Hour: 2}, config.HM{Hour: 4}},
	} {
		at, err := time.Parse(time.RFC3339, tc.at)
		if err != nil {
			t.Fatal(err)
		}
		u.Start, u.End = tc.start, tc.end
		if got := updateWindow(u, at.In(paris)); got != tc.want {
			t.Errorf("%s: window %q, want %q", tc.at, got, tc.want)
		}
	}
}

func updateApp(t *testing.T) (*App, <-chan string) {
	t.Helper()
	a := testApp(t)
	const asset = "nabos-zero2-arm64.raucb"
	var srv *httptest.Server
	tags := []string{"v1.1.0", "v1.1.1", "v1.2.0-rc.2", "v1.3.0-beta.1"}
	release := func(tag string) map[string]any {
		base := srv.URL + "/guilhem/nabos/releases/download/" + tag + "/"
		assets := []map[string]any{}
		if tag != "v1.3.0-beta.1" {
			assets = append(assets,
				map[string]any{"name": asset, "size": len("bundle-" + tag), "state": "uploaded", "browser_download_url": base + asset},
				map[string]any{"name": "SHA256SUMS", "size": 100, "state": "uploaded", "browser_download_url": base + "SHA256SUMS"})
		}
		return map[string]any{"tag_name": tag, "body": "<script>alert('notes')</script>", "prerelease": strings.Contains(tag, "-"), "published_at": "2026-09-29T00:00:00Z", "assets": assets}
	}
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/guilhem/nabos/releases":
			catalog := []map[string]any{}
			if r.URL.Query().Get("page") == "1" || r.URL.Query().Get("page") == "" {
				for _, tag := range tags {
					catalog = append(catalog, release(tag))
				}
			}
			json.NewEncoder(w).Encode(catalog)
		case strings.HasPrefix(r.URL.Path, "/repos/guilhem/nabos/releases/tags/"):
			tag := strings.TrimPrefix(r.URL.Path, "/repos/guilhem/nabos/releases/tags/")
			for _, known := range tags {
				if known == tag {
					json.NewEncoder(w).Encode(release(tag))
					return
				}
			}
			http.NotFound(w, r)
		case strings.HasPrefix(r.URL.Path, "/guilhem/nabos/releases/download/"):
			parts := strings.Split(r.URL.Path, "/")
			body := []byte("bundle-" + parts[len(parts)-2])
			if filepath.Base(r.URL.Path) == "SHA256SUMS" {
				fmt.Fprintf(w, "%x  %s\n", sha256.Sum256(body), asset)
			} else {
				w.Write(body)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	a.upd = update.New("guilhem/nabos", asset, "v1.0.0", filepath.Join(a.env.DataDir, "updates"))
	a.upd.APIBase, a.upd.DownloadBase, a.upd.HTTP = srv.URL, srv.URL, srv.Client()
	a.upd.Probe = func() (update.BootState, error) {
		return update.BootState{BootID: "boot-a", Slot: "A", Health: "good", Operation: "idle"}, nil
	}
	installed := make(chan string, 4)
	a.upd.Install = func(_ context.Context, path string, progress func(int)) error {
		body, err := os.ReadFile(path)
		if err == nil {
			installed <- string(body)
			progress(100)
		}
		return err
	}
	return a, installed
}

func TestUpdateUIAndSelectedInstallation(t *testing.T) {
	a, installed := updateApp(t)
	cookie := serviceSession(t, a)
	h := a.routes()
	form := url.Values{"mode": {"auto"}, "channel": {"test"}, "start": {"23:00"}, "end": {"02:00"}}
	for _, path := range []string{"/updates/settings", "/updates/check", "/updates/install"} {
		if w := serviceRequest(h, "POST", path, form, nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s: %d", path, w.Code)
		}
		r := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
		r.AddCookie(cookie)
		r.Header.Set("Origin", "http://foreign.example")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("cross-site %s: %d", path, w.Code)
		}
	}
	if _, err := a.upd.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	page := serviceRequest(h, "GET", "/updates", nil, cookie).Body.String()
	if !strings.Contains(page, "v1.1.1") || strings.Contains(page, "v1.2.0-rc.2") || strings.Contains(page, "<script>") || !strings.Contains(page, "&lt;script&gt;") {
		t.Fatal("stable catalogue or escaping", page)
	}
	if page := serviceRequest(h, "GET", "/", nil, cookie).Body.String(); !strings.Contains(page, "Nouvelle version v1.1.1") {
		t.Fatal("missing home notice")
	}
	if w := serviceRequest(h, "POST", "/updates/settings", form, cookie); strings.Contains(w.Header().Get("Location"), "err=") {
		t.Fatal(w.Header())
	}
	stored, err := config.Open(filepath.Join(a.env.DataDir, "config.json"))
	if err != nil || !stored.Get().Updates.Automatic || stored.Get().Updates.Channel != "test" {
		t.Fatal("preferences not persisted", err)
	}
	page = serviceRequest(h, "GET", "/updates", nil, cookie).Body.String()
	if !strings.Contains(page, "v1.2.0-rc.2") || !strings.Contains(page, "En préparation") || strings.Contains(page, "Installer v1.3.0-beta.1") {
		t.Fatal("preview catalogue", page)
	}
	form.Set("end", "23:00")
	if w := serviceRequest(h, "POST", "/updates/settings", form, cookie); !strings.Contains(w.Header().Get("Location"), "err=") {
		t.Fatal("empty window accepted")
	}
	if a.store.Get().Updates.End != (config.HM{Hour: 2}) {
		t.Fatal("invalid settings partially saved")
	}
	if w := serviceRequest(h, "POST", "/updates/install", url.Values{"tag": {"v1.1.0"}}, cookie); strings.Contains(w.Header().Get("Location"), "err=") {
		t.Fatal(w.Header())
	}
	select {
	case got := <-installed:
		if got != "bundle-v1.1.0" {
			t.Fatal("installed a different release", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("selected installation never started")
	}
	pynabWait(t, "installation completion", 5*time.Second, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return !a.updateBusy })
	if st := a.upd.Status(); st.State != "reboot" || st.PendingAuto {
		t.Fatalf("manual update unexpectedly became automatic: %+v", st)
	}
	page = serviceRequest(h, "GET", "/updates", nil, cookie).Body.String()
	if !strings.Contains(page, "Redémarrer sur la nouvelle version") {
		t.Fatal("missing reboot action")
	}
}

func TestAutomaticUpdateWithMQTT(t *testing.T) {
	if os.Getenv("NABOS_INTEGRATION") != "1" {
		t.Skip("set NABOS_INTEGRATION=1 for Mosquitto")
	}
	a, installed := updateApp(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	h := &pynabMQTT{t: t, port: port, pub: pynabTool(t, "MOSQUITTO_PUB", "mosquitto_pub"), env: os.Environ()}
	conf := filepath.Join(t.TempDir(), "mosquitto.conf")
	if err := os.WriteFile(conf, []byte(fmt.Sprintf("listener %d 127.0.0.1\nallow_anonymous true\npersistence false\n", port)), 0o600); err != nil {
		t.Fatal(err)
	}
	h.start(t, "broker", []string{pynabTool(t, "MOSQUITTO", "mosquitto", "/usr/sbin/mosquitto"), "-c", conf})
	pynabWait(t, "broker", 5*time.Second, func() bool {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err == nil {
			c.Close()
		}
		return err == nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.ctx = ctx
	a.bus = bus.New("127.0.0.1", port, "update-test", bus.Handlers{})
	if err := a.bus.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.bus.Stop(context.Background()) })
	publish := func(topic, value string) {
		t.Helper()
		cmd := exec.Command(h.pub, "-h", "127.0.0.1", "-p", strconv.Itoa(port), "-q", "1", "-r", "-t", topic, "-m", value)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("publish: %v %s", err, out)
		}
	}
	publish(bus.TopicCoreAvail, "online")
	publish(bus.TopicState, `{"v":1,"state":"idle","playing":null}`)
	pynabWait(t, "idle core", 5*time.Second, func() bool { s, online := a.bus.State(); return online && s.State == "idle" })
	now := time.Now().UTC()
	_, err = a.store.Update(func(s *config.Settings) error {
		s.Timezone, s.Updates.Automatic = "UTC", true
		s.Updates.Start, s.Updates.End = config.HM{Hour: now.Hour()}, config.HM{Hour: (now.Hour() + 2) % 24}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.upd.Check(ctx); err != nil {
		t.Fatal(err)
	}
	assertDeferred := func() {
		t.Helper()
		a.updateTick(now)
		if a.upd.Status().LastWindow != "" {
			t.Fatal("claimed a window before safe conditions")
		}
	}
	assertDeferred() // unknown clock
	a.env.TimesyncFile = "none"
	a.mediaGate <- struct{}{}
	assertDeferred() // ongoing book/radio holds the gate
	<-a.mediaGate
	a.voice = &voice.Client{}
	assertDeferred() // voice state unknown
	a.voice = nil
	publish(bus.TopicState, `{"v":1,"state":"playing","playing":"radio"}`)
	pynabWait(t, "busy core", 5*time.Second, func() bool { s, _ := a.bus.State(); return s.State == "playing" })
	assertDeferred()
	publish(bus.TopicState, `{"v":1,"state":"asleep","playing":null}`)
	pynabWait(t, "sleeping core", 5*time.Second, func() bool { s, _ := a.bus.State(); return s.State == "asleep" })
	var reboots atomic.Int32
	a.rebootSystem = func() error { reboots.Add(1); return nil }
	a.updateTick(now)
	select {
	case <-installed:
	case <-time.After(5 * time.Second):
		t.Fatal("automatic installation did not start")
	}
	pynabWait(t, "installation completion", 5*time.Second, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return !a.updateBusy })
	a.updateTick(now.Add(6 * time.Hour))
	if reboots.Load() != 0 {
		t.Fatal("reboot outside window")
	}
	a.store.Update(func(s *config.Settings) error { s.Updates.Automatic = false; return nil })
	a.updateTick(now)
	if reboots.Load() != 0 {
		t.Fatal("reboot after disabling automatic mode")
	}
	a.store.Update(func(s *config.Settings) error { s.Updates.Automatic = true; s.Updates.Channel = "test"; return nil })
	a.updateTick(now)
	if reboots.Load() != 0 {
		t.Fatal("reboot after changing channel")
	}
	a.store.Update(func(s *config.Settings) error { s.Updates.Channel = "stable"; return nil })
	a.updateTick(now)
	a.updateTick(now)
	if reboots.Load() != 1 {
		t.Fatal("automatic reboot not performed exactly once", reboots.Load())
	}
}
