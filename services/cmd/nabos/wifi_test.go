package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"github.com/guilhem/nabos/services/internal/devicetest"
	"golang.org/x/sys/unix"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/guilhem/nabos/services/internal/network"
	"github.com/guilhem/nabos/services/internal/web"
)

const testProfile = "12345678-1234-1234-1234-123456789abc"

var wifiFixtures = map[string]*wifiCore{}

const coreLease = "only-the-core-and-go-know-this-lease"

type wifiCore struct {
	system                *devicetest.Fixture
	guardPath             string
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
	unavailable           bool
	connectEntered        chan struct{}
	connectResume         chan struct{}
}

func (c *wifiCore) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.unavailable {
		return nil, dbus.MakeFailedError(context.Canceled)
	}

	return map[string]dbus.Variant{
		"Status":   dbus.MakeVariant(c.status),
		"Networks": dbus.MakeVariant([]network.Network{{SSID: network.SSID{255, 0, 65}, Strength: 90, Security: "wpa-psk"}, {SSID: network.SSID("WiFi"), Strength: 80, Security: "open"}}),
		"Profiles": dbus.MakeVariant([]network.Profile{{UUID: testProfile, SSID: network.SSID{255, 0, 65}}}),
	}, nil
}
func (c *wifiCore) Get(iface, name string) (dbus.Variant, *dbus.Error) {
	properties, err := c.GetAll(iface)
	return properties[name], err
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
	if c.connectEntered != nil {
		close(c.connectEntered)
		resume := c.connectResume
		c.mu.Unlock()
		<-resume
		c.mu.Lock()
	}
	if c.fail {
		return 0, dbus.NewError("io.github.guilhem.DeviceCore1.Error.Refused", []any{password})
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
	core := &wifiCore{system: devicetest.Attach(t, bus), guardPath: t.TempDir() + "/guard", status: network.Status{Mode: "hotspot", Generation: "network:1", Address: hotspotAddress, Phase: "idle"}}
	if err = bus.Export(core, network.Path, network.Interface); err != nil {
		t.Fatal(err)
	}
	if err = bus.Export(core, network.Path, "org.freedesktop.DBus.Properties"); err != nil {
		t.Fatal(err)
	}
	wifiFixtures[os.Getenv("DBUS_SYSTEM_BUS_ADDRESS")] = core
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

var wifiHardware = map[*App]*nativeFixture{}

// Use the authenticated hardware signal path, including exact uint64 edges.
func physicalButton(t *testing.T, a *App, button string, edge uint64) {
	t.Helper()
	f := wifiHardware[a]
	if f == nil {
		f = startNativeDispatch(t, a, a.onEvent)
		wifiHardware[a] = f
		t.Cleanup(func() { delete(wifiHardware, a) })
	}
	f.event(t, "Button", button, edge)
	for {
		select {
		case event := <-f.observed:
			if event.Kind == "button" && event.Button == button && (button != "down" || event.EdgeMonotonicNS == edge) {
				return
			}
		case <-time.After(time.Second):
			t.Fatal("hardware button was not dispatched")
		}
	}
}

func freshDown(t *testing.T, a *App) uint64 {
	t.Helper()
	edge := a.auth.MonoNow()
	physicalButton(t, a, "down", edge)
	return edge
}

func TestWifiBeforeAdminOnRealHotspot(t *testing.T) {
	core := wifiTestBus(t)
	a := testApp(t)
	h := a.routes()
	request := func(method, path string, f url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
		return wifiRequest(h, method, path, f, cookie, hotspotAddress, "http://10.41.0.1", false)
	}
	password := url.Values{"password": {"carotte-42"}, "confirm": {"carotte-42"}}
	freshDown(t, a)
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
	consumedEdge := freshDown(t, a)
	w = request("POST", "/wifi/connect", form, cookie)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "coupure") || strings.Contains(w.Body.String(), form.Get("password")) || strings.Contains(w.Body.String(), coreLease) {
		t.Fatal("transition or secrets", w.Code)
	}
	if a.auth.Present() {
		t.Fatal("Wi-Fi press remained usable for admin setup")
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
	core.status.Mode, core.status.Address, core.status.Phase = "client", "192.168.1.3", "connecting"
	core.mu.Unlock()
	if w := request("POST", "/setup", password, nil); w.Header().Get("Location") != "/wifi" || a.auth.Present() || a.auth.Configured() {
		t.Fatal("client address during connecting armed admin setup")
	}
	duringConnect := freshDown(t, a)
	core.mu.Lock()
	core.status.Phase, core.status.Ready = "succeeded", true
	core.mu.Unlock()
	if w := request("GET", "/wifi/status", nil, cookie); w.Code != http.StatusForbidden {
		t.Fatal("pre-admin operations admitted outside actual hotspot")
	}
	w = request("POST", "/setup", password, nil)
	if !strings.HasPrefix(w.Header().Get("Location"), "/setup?err=") || a.auth.Configured() {
		t.Fatal("Wi-Fi proof authorized admin creation after reconnect")
	}
	// These downs happened before the setup cutoff, even if the event queue delivers
	// them later and publication/receipt time both appear fresh.
	for _, edge := range []uint64{consumedEdge, duringConnect} {
		physicalButton(t, a, "down", edge)
		if a.auth.Present() {
			t.Fatal("delayed pre-setup down authorized admin creation")
		}
	}
	freshDown(t, a)
	if w := request("GET", "/setup", nil, nil); w.Code != 200 || !a.auth.Present() {
		t.Fatal("setup reload cleared fresh proof")
	}
	for _, invalid := range []url.Values{
		{"password": {"carotte-42"}, "confirm": {"different"}},
		{"password": {"short"}, "confirm": {"short"}},
	} {
		if w := request("POST", "/setup", invalid, nil); !strings.HasPrefix(w.Header().Get("Location"), "/setup?err=") || !a.auth.Present() {
			t.Fatal("form error cleared proof or moved cutoff")
		}
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

func TestSetupDisarmsOnNetworkAndServiceTransitions(t *testing.T) {
	core := wifiTestBus(t)
	a := testApp(t)
	h := a.routes()
	ready := network.Status{Mode: "client", Ready: true, Generation: "network:1", Address: "192.0.2.10", Phase: "succeeded"}
	setStatus := func(s network.Status, unavailable bool) {
		core.mu.Lock()
		core.status, core.unavailable = s, unavailable
		core.mu.Unlock()
	}
	for _, tc := range []struct {
		name        string
		status      network.Status
		unavailable bool
	}{
		{"hotspot", network.Status{Mode: "hotspot", Address: hotspotAddress}, false},
		{"reconnecting", network.Status{Mode: "reconnecting"}, false},
		{"no-address", network.Status{Mode: "client", Generation: "network:1", Phase: "succeeded"}, false},
		{"connecting-with-address", network.Status{Mode: "client", Ready: true, Generation: "network:1", Address: ready.Address, Phase: "connecting"}, false},
		{"unavailable", network.Status{Mode: "unavailable"}, false},
		{"dbus-error", ready, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setStatus(ready, false)
			if w := serviceRequest(h, "GET", "/setup", nil, nil); w.Code != 200 {
				t.Fatal("open setup", w.Code)
			}
			freshDown(t, a)
			if !a.auth.Present() {
				t.Fatal("ready setup refused fresh press")
			}
			setStatus(tc.status, tc.unavailable)
			w := serviceRequest(h, "GET", "/setup", nil, nil)
			if (tc.unavailable && w.Code != 503) || (!tc.unavailable && w.Header().Get("Location") != "/wifi") || a.auth.Present() {
				t.Fatal("network transition kept setup armed", w.Code, w.Header())
			}
			oldEdge := freshDown(t, a)
			if a.auth.Present() {
				t.Fatal("down during unavailable setup granted proof")
			}
			setStatus(ready, false)
			if w := serviceRequest(h, "GET", "/setup", nil, nil); w.Code != 200 {
				t.Fatal("reopen setup", w.Code)
			}
			physicalButton(t, a, "down", oldEdge)
			if a.auth.Present() {
				t.Fatal("delayed down survived setup rearm")
			}
		})
	}
	// A service restart has no proof or armed gate, even on a ready client.
	oldEdge := freshDown(t, a)
	stopCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	a.rabbit.Stop(stopCtx)
	stop()
	delete(wifiHardware, a)
	// Finish the old hardware callbacks before replacing the service auth state.
	a.auth = web.NewAuth(a.store)
	physicalButton(t, a, "down", oldEdge)
	if a.auth.Present() {
		t.Fatal("restart admitted a queued down")
	}
	if w := serviceRequest(h, "GET", "/setup", nil, nil); w.Code != 200 {
		t.Fatal("open setup after restart", w.Code)
	}
	physicalButton(t, a, "down", oldEdge)
	if a.auth.Present() {
		t.Fatal("restart admitted pre-arm proof after opening setup")
	}
}

func TestButtonPresenceRequiresFreshDownMetadata(t *testing.T) {
	core := wifiTestBus(t)
	core.mu.Lock()
	core.status = network.Status{Mode: "client", Ready: true, Generation: "network:1", Address: "192.0.2.10", Phase: "idle"}
	core.mu.Unlock()
	a := testApp(t)
	var now atomic.Uint64
	now.Store(uint64(1<<53) + 1000) // Above JSON's exact floating-point integer range.
	a.auth.MonoNow = now.Load
	h := a.routes()
	if w := serviceRequest(h, "GET", "/setup", nil, nil); w.Code != 200 {
		t.Fatal("open setup", w.Code)
	}
	cutoff := now.Load()
	now.Add(1)
	for _, event := range []string{"up", "click", "hold", "double_click", "click_and_hold"} {
		physicalButton(t, a, event, now.Load())
		if a.auth.Present() {
			t.Fatal("non-down granted presence", event)
		}
	}
	// D-Bus transports the timestamp as uint64; malformed JSON no longer exists.
	for _, edge := range []uint64{0, cutoff, now.Load() + 1} {
		physicalButton(t, a, "down", edge)
		if a.auth.Present() {
			t.Fatalf("invalid edge granted presence: %v", edge)
		}
	}
	edge := freshDown(t, a)
	if !a.auth.Present() {
		t.Fatal("exact uint64 down refused")
	}
	now.Store(edge + uint64(5*time.Minute))
	physicalButton(t, a, "down", edge)
	if a.auth.Present() {
		t.Fatal("duplicate renewed expired proof")
	}
	freshDown(t, a)
	if !a.auth.Present() {
		t.Fatal("new down refused after expiry")
	}
}

func TestWifiConnectDisarmsBeforeDBusAndStaysDisarmedOnError(t *testing.T) {
	core := wifiTestBus(t)
	a := testApp(t)
	h := a.routes()
	core.mu.Lock()
	core.status = network.Status{Mode: "client", Ready: true, Generation: "network:1", Address: "192.0.2.10", Phase: "idle"}
	core.mu.Unlock()
	if w := serviceRequest(h, "GET", "/setup", nil, nil); w.Code != 200 {
		t.Fatal("open setup", w.Code)
	}
	freshDown(t, a)
	core.mu.Lock()
	core.status = network.Status{Mode: "hotspot", Generation: "network:1", Address: hotspotAddress, Phase: "idle"}
	core.connectEntered, core.connectResume = make(chan struct{}), make(chan struct{})
	core.fail = true
	core.mu.Unlock()
	request := func(method, path string, f url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
		return wifiRequest(h, method, path, f, cookie, hotspotAddress, "http://10.41.0.1", false)
	}
	w := request("POST", "/wifi/reserve", nil, nil)
	if len(w.Result().Cookies()) != 1 {
		t.Fatal("reserve", w.Code, w.Header())
	}
	cookie := w.Result().Cookies()[0]
	core.mu.Lock()
	core.physicalExpires = time.Now().Add(time.Minute)
	core.mu.Unlock()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- request("POST", "/wifi/connect", url.Values{"ssid": {"LAN"}, "security": {"open"}}, cookie)
	}()
	<-core.connectEntered
	if a.auth.Present() {
		close(core.connectResume)
		<-done
		t.Fatal("Connect started with admin proof still armed")
	}
	freshDown(t, a)
	if a.auth.Present() {
		close(core.connectResume)
		<-done
		t.Fatal("down in pending Connect granted admin proof")
	}
	setup := make(chan *httptest.ResponseRecorder, 1)
	go func() { setup <- request("GET", "/setup", nil, nil) }()
	select {
	case <-setup:
		close(core.connectResume)
		<-done
		t.Fatal("setup readiness was not serialized with Connect")
	case <-time.After(50 * time.Millisecond):
	}
	close(core.connectResume)
	if w := <-done; !strings.Contains(w.Header().Get("Location"), "err=") || a.auth.Present() {
		t.Fatal("D-Bus error restored admin proof", w.Code, w.Header())
	}
	if w := <-setup; w.Header().Get("Location") != "/wifi" {
		t.Fatal("setup after failed Connect", w.Code, w.Header())
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

func (c *wifiCore) AcquireGuard(expected string) (dbus.UnixFD, *dbus.Error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if expected == "" || expected != c.status.Generation || !c.status.ClientReady() || c.status.Phase == "connecting" {
		return 0, dbus.MakeFailedError(context.Canceled)
	}
	fd, err := unix.Open(c.guardPath, unix.O_CREAT|unix.O_RDONLY|unix.O_CLOEXEC, 0600)
	if err != nil {
		return 0, dbus.MakeFailedError(err)
	}
	if err = unix.Flock(fd, unix.LOCK_SH); err != nil {
		unix.Close(fd)
		return 0, dbus.MakeFailedError(err)
	}
	// The wire implementation duplicates SCM_RIGHTS descriptors. Close our
	// descriptor after the send, retaining the client's duplicated shared lock.
	time.AfterFunc(time.Second, func() { unix.Close(fd) })
	return dbus.UnixFD(fd), nil
}

func TestSetupNetworkGenerationRequiresNewProof(t *testing.T) {
	core := wifiTestBus(t)
	core.mu.Lock()
	core.status = network.Status{Mode: "client", Generation: "network:1", Ready: true, Address: "192.0.2.10", Phase: "idle"}
	core.mu.Unlock()
	a := testApp(t)
	h := a.routes()
	if w := serviceRequest(h, "GET", "/setup", nil, nil); w.Code != 200 {
		t.Fatal(w.Code)
	}
	freshDown(t, a)
	core.mu.Lock()
	core.status.Generation = "network:2"
	core.mu.Unlock()
	password := url.Values{"password": {"carotte-42"}, "confirm": {"carotte-42"}}
	if w := serviceRequest(h, "POST", "/setup", password, nil); !strings.Contains(w.Header().Get("Location"), "err=") || a.auth.Configured() {
		t.Fatal("old network proof committed administrator", w.Header())
	}
	freshDown(t, a)
	if w := serviceRequest(h, "POST", "/setup", password, nil); w.Header().Get("Location") != "/settings" {
		t.Fatal("fresh proof rejected", w.Header())
	}
}
