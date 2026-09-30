package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/guilhem/nabos/services/internal/network"
)

const testProfile = "12345678-1234-1234-1234-123456789abc"
const coreLease = "only-the-core-and-go-know-this-lease"

type wifiCore struct {
	mu                    sync.Mutex
	status                network.Status
	reserved              bool
	physicalExpires       time.Time
	renewals              int
	connections           int
	ssid                  []byte
	uuid, token, password string
	cancel                uint64
	forget                string
	fail                  bool
}

func (c *wifiCore) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, _ := json.Marshal(c.status)
	return map[string]dbus.Variant{
		"Status":   dbus.MakeVariant(string(s)),
		"Networks": dbus.MakeVariant(`[{"ssid":[255,0,65],"strength":90,"security":"wpa-psk"},{"ssid":[87,105,70,105],"strength":80,"security":"open"}]`),
		"Profiles": dbus.MakeVariant(`[{"uuid":"` + testProfile + `","ssid":[255,0,65]}]`),
	}, nil
}
func (c *wifiCore) Get(iface, name string) (dbus.Variant, *dbus.Error) {
	properties, _ := c.GetAll(iface)
	return properties[name], nil
}
func (c *wifiCore) Reserve(token string) (string, *dbus.Error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status.Mode != "hotspot" || c.status.Phase == "connecting" {
		return "", dbus.MakeFailedError(context.Canceled)
	}
	if token == "" && !c.reserved {
		c.reserved = true
		return coreLease, nil
	}
	if token == coreLease && c.reserved {
		c.renewals++
		return token, nil
	}
	return "", dbus.MakeFailedError(context.Canceled)
}
func (c *wifiCore) Authorized(token string) (bool, *dbus.Error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reserved && token == coreLease && time.Now().Before(c.physicalExpires), nil
}
func (c *wifiCore) Release(token string) *dbus.Error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if token != coreLease {
		return dbus.MakeFailedError(context.Canceled)
	}
	c.reserved = false
	return nil
}
func (c *wifiCore) Connect(ssid []byte, security, password, uuid, token string) (uint64, *dbus.Error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if token != "" && (!c.reserved || token != coreLease || !time.Now().Before(c.physicalExpires)) {
		return 0, dbus.MakeFailedError(context.Canceled)
	}
	if c.fail {
		return 0, dbus.NewError("org.nabaztag.Core.Error.Refused", []any{password})
	}
	c.connections++
	c.ssid, c.uuid, c.token, c.password = ssid, uuid, token, password
	c.physicalExpires = time.Time{}
	c.status.AttemptID, c.status.Phase = uint64(c.connections), "connecting"
	return c.status.AttemptID, nil
}
func (c *wifiCore) Cancel(id uint64) *dbus.Error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancel = id
	return nil
}
func (c *wifiCore) Forget(uuid string) *dbus.Error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.forget = uuid
	return nil
}
func (c *wifiCore) Scan() *dbus.Error { return dbus.MakeFailedError(context.Canceled) }

func wifiTestBus(t *testing.T) *wifiCore {
	t.Helper()
	if _, err := exec.LookPath("dbus-daemon"); err != nil {
		t.Skip("dbus-daemon not installed")
	}
	daemon := exec.Command("dbus-daemon", "--session", "--nofork", "--print-address=1")
	out, err := daemon.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { daemon.Process.Kill(); daemon.Wait() })
	address, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DBUS_SYSTEM_BUS_ADDRESS", strings.TrimSpace(address))
	bus, err := dbus.ConnectSystemBus()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bus.Close() })
	if _, err = bus.RequestName(network.Destination, dbus.NameFlagDoNotQueue); err != nil {
		t.Fatal(err)
	}
	core := &wifiCore{status: network.Status{Mode: "hotspot", Address: hotspotAddress, Phase: "idle"}}
	if err = bus.Export(core, network.Path, network.Interface); err != nil {
		t.Fatal(err)
	}
	if err = bus.Export(core, network.Path, "org.freedesktop.DBus.Properties"); err != nil {
		t.Fatal(err)
	}
	return core
}

func wifiRequest(h http.Handler, method, path string, form url.Values, cookie *http.Cookie, local, origin string, cancel bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://10.41.0.1"+path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	ctx := context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP(local), Port: 80})
	if cancel {
		cancelled, stop := context.WithCancel(ctx)
		stop()
		ctx = cancelled
	}
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestWifiBeforeAdminOnRealHotspot(t *testing.T) {
	core := wifiTestBus(t)
	a := testApp(t)
	h := a.routes()
	request := func(method, path string, f url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
		return wifiRequest(h, method, path, f, cookie, hotspotAddress, "http://10.41.0.1", false)
	}
	password := url.Values{"password": {"carotte-42"}, "confirm": {"carotte-42"}}
	a.auth.MarkPresence()
	if w := request("POST", "/setup", password, nil); w.Header().Get("Location") != "/wifi" || a.auth.Configured() {
		t.Fatal("admin creation bypassed Wi-Fi")
	}
	w := request("GET", "/wifi", nil, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "0xff0041") || !strings.Contains(w.Body.String(), "même radio") {
		t.Fatal("Wi-Fi page", w.Code)
	}
	for _, local := range []string{"127.0.0.1", "192.168.1.2", ""} {
		w := wifiRequest(h, "POST", "/wifi/reserve", nil, nil, local, "http://10.41.0.1", false)
		if w.Code != http.StatusForbidden {
			t.Fatal("Host bypassed actual socket address", local, w.Code)
		}
	}
	if w := wifiRequest(h, "POST", "/wifi/reserve", nil, nil, hotspotAddress, "http://attacker.test", false); w.Code != http.StatusForbidden {
		t.Fatal("cross-site reservation", w.Code)
	}
	w = request("POST", "/wifi/reserve", url.Values{"token": {"forged"}}, nil)
	if w.Code != http.StatusSeeOther {
		t.Fatal("reserve", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != wifiCookie || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || len(cookies[0].Value) != 64 || cookies[0].Value == coreLease {
		t.Fatal("reservation cookie not securely bound")
	}
	cookie := cookies[0]
	form := url.Values{"ssid_bytes": {base64.StdEncoding.EncodeToString([]byte{255, 0, 65})}, "security": {"wpa-psk"}, "password": {"wifi-secret-42"}, "token": {coreLease}}
	for _, forged := range []*http.Cookie{nil, {Name: wifiCookie, Value: coreLease}, {Name: wifiCookie, Value: "forged"}} {
		if w := request("POST", "/wifi/connect", form, forged); w.Code != http.StatusForbidden {
			t.Fatal("forged cookie/form authorized connect", w.Code)
		}
	}
	if w := request("POST", "/wifi/connect", form, cookie); w.Code != http.StatusForbidden {
		t.Fatal("no physical press required", w.Code)
	}
	core.mu.Lock()
	expiry := time.Now().Add(time.Minute)
	core.physicalExpires = expiry
	core.mu.Unlock()
	w = request("POST", "/wifi/reserve", nil, cookie)
	if w.Code != http.StatusSeeOther || w.Result().Cookies()[0].Value != cookie.Value {
		t.Fatal("renewal changed browser nonce")
	}
	core.mu.Lock()
	if core.renewals != 1 || !core.physicalExpires.Equal(expiry) {
		t.Fatal("renewal extended physical authorization")
	}
	core.physicalExpires = time.Now().Add(-time.Second)
	core.mu.Unlock()
	if w := request("POST", "/wifi/reserve", nil, cookie); w.Code != http.StatusSeeOther {
		t.Fatal("expired physical lease renewal failed")
	}
	if w := request("POST", "/wifi/connect", form, cookie); w.Code != http.StatusForbidden {
		t.Fatal("renewal restored expired physical authorization")
	}
	core.mu.Lock()
	core.physicalExpires = time.Now().Add(time.Minute)
	core.mu.Unlock()
	w = request("GET", "/wifi/status", nil, cookie)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"authorized":true`) || strings.Contains(w.Body.String(), coreLease) {
		t.Fatal("authorization status leaks lease or is wrong")
	}
	w = request("POST", "/wifi/connect", form, cookie)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "coupure") || strings.Contains(w.Body.String(), form.Get("password")) || strings.Contains(w.Body.String(), coreLease) {
		t.Fatal("transition or secrets", w.Code)
	}
	core.mu.Lock()
	if core.connections != 1 || len(core.ssid) != 3 || core.ssid[0] != 255 || core.token != coreLease {
		t.Fatal("candidate bytes/reservation changed")
	}
	core.mu.Unlock()
	if w := request("POST", "/wifi/connect", form, cookie); w.Code != http.StatusForbidden {
		t.Fatal("physical authorization reused")
	}
	if w := request("POST", "/wifi/cancel", url.Values{"attempt_id": {"99"}}, cookie); w.Code != http.StatusForbidden {
		t.Fatal("cancellation not bound to this session")
	}
	if w := request("POST", "/wifi/reserve", nil, cookie); w.Code != http.StatusConflict {
		t.Fatal("reserve during active attempt was accepted")
	}
	if w := request("POST", "/wifi/cancel", url.Values{"attempt_id": {"1"}}, cookie); w.Code != http.StatusSeeOther {
		t.Fatal("matching cancellation", w.Code)
	}
	if w := request("POST", "/wifi/forget", url.Values{"uuid": {testProfile}}, cookie); w.Code != http.StatusUnauthorized {
		t.Fatal("pre-admin profile deletion", w.Code)
	}
	core.mu.Lock()
	core.status.Mode, core.status.Address, core.status.Phase = "client", "192.168.1.3", "succeeded"
	core.mu.Unlock()
	if w := request("GET", "/wifi/status", nil, cookie); w.Code != http.StatusForbidden {
		t.Fatal("pre-admin operations admitted outside actual hotspot")
	}
	w = request("POST", "/setup", password, nil)
	if w.Header().Get("Location") != "/settings" || !a.auth.Configured() {
		t.Fatal("admin unavailable after client connection", w.Code, w.Header().Get("Location"))
	}
}

func TestWifiAdminAuthAndDetachedConnection(t *testing.T) {
	core := wifiTestBus(t)
	a := testApp(t)
	cookie := serviceSession(t, a)
	h := a.routes()
	form := url.Values{"ssid": {"LAN"}, "security": {"wpa-psk"}, "password": {"wifi-secret-42"}, "token": {"forged"}}
	for _, path := range []string{"/wifi/reserve", "/wifi/release", "/wifi/scan", "/wifi/connect", "/wifi/cancel", "/wifi/forget"} {
		if w := wifiRequest(h, "POST", path, form, nil, hotspotAddress, "http://10.41.0.1", false); w.Code != http.StatusUnauthorized {
			t.Fatal("admin path unauthenticated", path, w.Code)
		}
		if w := wifiRequest(h, "POST", path, form, cookie, hotspotAddress, "http://evil.test", false); w.Code != http.StatusForbidden {
			t.Fatal("admin CSRF", path, w.Code)
		}
	}
	if w := wifiRequest(h, "GET", "/wifi", nil, nil, hotspotAddress, "", false); w.Header().Get("Location") != "/login" {
		t.Fatal("hotspot bypassed existing admin")
	}
	w := wifiRequest(h, "POST", "/wifi/connect", form, cookie, "192.168.1.3", "http://10.41.0.1", true)
	if w.Code != 200 {
		t.Fatal("HTTP cancellation killed accepted attempt", w.Code, w.Body.String())
	}
	core.mu.Lock()
	if core.connections != 1 || core.token != "" {
		t.Fatal("admin submitted arbitrary lease")
	}
	core.fail = true
	core.mu.Unlock()
	w = wifiRequest(h, "POST", "/wifi/connect", form, cookie, hotspotAddress, "http://10.41.0.1", false)
	if strings.Contains(w.Body.String()+w.Header().Get("Location"), form.Get("password")) || !strings.Contains(w.Header().Get("Location"), "err=") {
		t.Fatal("D-Bus error leaked secret")
	}
	core.mu.Lock()
	core.fail = false
	core.mu.Unlock()
	profile := url.Values{"uuid": {testProfile}}
	w = wifiRequest(h, "POST", "/wifi/connect", profile, cookie, "192.168.1.3", "http://10.41.0.1", false)
	if w.Code != 200 {
		t.Fatal("existing profile", w.Code)
	}
	core.mu.Lock()
	if core.uuid != testProfile || len(core.ssid) != 0 || core.password != "" {
		t.Fatal("saved profile was reconstructed or its secret requested")
	}
	core.mu.Unlock()
	if w := wifiRequest(h, "POST", "/wifi/forget", profile, cookie, "192.168.1.3", "http://10.41.0.1", false); w.Code != http.StatusSeeOther {
		t.Fatal("profile deletion", w.Code)
	}
	if w := wifiRequest(h, "POST", "/wifi/scan", nil, cookie, "192.168.1.3", "http://10.41.0.1", false); w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "err=") {
		t.Fatal("scan rejection lost manual fallback")
	}
}

func TestWifiHealthAndInputBoundary(t *testing.T) {
	a := testApp(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/healthz", nil)
	r.RemoteAddr = "192.168.1.4:4321"
	r.Header.Set("X-Forwarded-For", "127.0.0.1")
	a.routes().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatal("remote health endpoint", w.Code)
	}
	for _, f := range []url.Values{
		{"ssid_bytes": {"invalid!"}, "security": {"open"}},
		{"ssid": {strings.Repeat("é", 17)}, "security": {"open"}},
		{"ssid": {"net"}, "security": {"wep"}},
		{"ssid": {"net"}, "security": {"wpa-psk"}, "password": {"short"}},
		{"uuid": {"/org/freedesktop/NetworkManager/Settings/1"}},
	} {
		r := httptest.NewRequest("POST", "/wifi/connect", strings.NewReader(f.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if err := wifiForm(httptest.NewRecorder(), r); err != nil {
			t.Fatal(err)
		}
		if _, _, _, _, err := wifiCandidate(r); err == nil {
			t.Fatal("invalid Wi-Fi candidate accepted")
		}
	}
}
