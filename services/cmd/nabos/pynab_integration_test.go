package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/guilhem/nabos/services/internal/config"
	"github.com/guilhem/nabos/services/internal/device"
	"github.com/guilhem/nabos/services/internal/hardware"
	"github.com/guilhem/nabos/services/internal/rabbit"
)

func pynabWait(t *testing.T, what string, timeout time.Duration, ready func() bool) {
	t.Helper()
	for end := time.Now().Add(timeout); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if ready() {
			return
		}
	}
	t.Fatalf("timeout waiting for %s", what)
}

type nativeAudioPlay struct {
	ID, Kind, Source string
	done             chan struct{}
	Outcome          string
}
type nativeFixture struct {
	mu       sync.Mutex
	conn     *dbus.Conn
	status   hardware.Status
	plays    []*nativeAudioPlay
	stopped  []string
	hold     func(string) bool
	leds     int
	frames   []hardware.Color
	writes   []rabbit.TagWrite
	observed chan appEvent
}

func (f *nativeFixture) Claim() *dbus.Error   { return nil }
func (f *nativeFixture) Release() *dbus.Error { return nil }
func (f *nativeFixture) SetLeds(v []hardware.Color) *dbus.Error {
	f.mu.Lock()
	f.leds++
	f.frames = append(f.frames, v...)
	f.mu.Unlock()
	return nil
}
func (f *nativeFixture) PulseLed(index, r, g, b uint8) *dbus.Error { return nil }
func (f *nativeFixture) MoveEar(index, position uint8, backward bool) *dbus.Error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if index == 0 {
		f.status.Left = int16(position)
	} else {
		f.status.Right = int16(position)
	}
	return nil
}
func (f *nativeFixture) StepEar(index, steps uint8, backward bool) *dbus.Error { return nil }
func (f *nativeFixture) ReadEars(detect bool) (int16, int16, *dbus.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status.Left, f.status.Right, nil
}
func (f *nativeFixture) WaitEarsIdle() *dbus.Error { return nil }
func (f *nativeFixture) StartWrite(tech string, uid []byte, picture, app uint8, data []byte, timeout uint32) (uint64, *dbus.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, rabbit.TagWrite{Tech: tech, UID: uid, Picture: picture, App: app, Data: data, Timeout: timeout})
	return uint64(len(f.writes)), nil
}
func (f *nativeFixture) WaitWrite(id uint64) (string, *dbus.Error) { return "completed", nil }
func (f *nativeFixture) CancelWrite(id uint64) *dbus.Error         { return nil }
func (f *nativeFixture) Get(iface, name string) (dbus.Variant, *dbus.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if name == "Status" {
		return dbus.MakeVariant(f.status), nil
	}
	if name == "Ready" {
		return dbus.MakeVariant(f.status.Ready()), nil
	}
	return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.UnknownProperty", nil)
}
func (f *nativeFixture) Start(kind, source string) (string, *dbus.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := &nativeAudioPlay{ID: fmt.Sprintf("audio-%d", len(f.plays)), Kind: kind, Source: source, done: make(chan struct{}), Outcome: "completed"}
	f.plays = append(f.plays, p)
	if f.hold == nil || !f.hold(source) {
		close(p.done)
	}
	return p.ID, nil
}
func (f *nativeFixture) Stop(id string) *dbus.Error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.plays {
		if p.ID == id {
			select {
			case <-p.done:
			default:
				p.Outcome = "stopped"
				close(p.done)
			}
			f.stopped = append(f.stopped, id)
			return nil
		}
	}
	return dbus.MakeFailedError(fmt.Errorf("unknown audio ID"))
}
func (f *nativeFixture) Wait(id string) (string, *dbus.Error) {
	f.mu.Lock()
	var play *nativeAudioPlay
	for _, p := range f.plays {
		if p.ID == id {
			play = p
			break
		}
	}
	f.mu.Unlock()
	if play == nil {
		return "", dbus.MakeFailedError(fmt.Errorf("unknown audio ID"))
	}
	<-play.done
	f.mu.Lock()
	defer f.mu.Unlock()
	return play.Outcome, nil
}
func (f *nativeFixture) finish(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.plays {
		if p.ID == id {
			select {
			case <-p.done:
			default:
				close(p.done)
			}
			return
		}
	}
}
func (f *nativeFixture) play(t *testing.T, after int, suffix string) (int, string) {
	t.Helper()
	index := -1
	id := ""
	pynabWait(t, "audio "+suffix, 3*time.Second, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		for i, p := range f.plays {
			if i >= after && strings.HasSuffix(p.Source, suffix) {
				index, id = i, p.ID
				return true
			}
		}
		return false
	})
	return index, id
}
func (f *nativeFixture) length() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.plays) }
func (f *nativeFixture) canceled(t *testing.T, id string) {
	t.Helper()
	pynabWait(t, "targeted audio stop", time.Second, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, s := range f.stopped {
			if s == id {
				return true
			}
		}
		return false
	})
}
func (f *nativeFixture) event(t *testing.T, member string, values ...any) {
	t.Helper()
	if err := f.conn.Emit(hardware.Path, hardware.Destination+"."+member, values...); err != nil {
		t.Fatal(err)
	}
}

func (f *nativeFixture) observedEvent(t *testing.T, match func(appEvent) bool) appEvent {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-f.observed:
			if match(event) {
				return event
			}
		case <-timer.C:
			t.Fatal("authenticated hardware event was not delivered")
		}
	}
}

func startNative(t *testing.T, a *App) *nativeFixture {
	t.Helper()
	return startNativeDispatch(t, a, nil)
}

func startNativeDispatch(t *testing.T, a *App, dispatch func(appEvent)) *nativeFixture {
	t.Helper()
	f := &nativeFixture{conn: appFixture(t, a).Conn, status: hardware.Status{Model: "test", LeftEar: "ok", RightEar: "ok", Leds: true, Button: true, RFID: "st25tb"}, observed: make(chan appEvent, 128)}
	for _, name := range []string{hardware.Destination} {
		if _, err := f.conn.RequestName(name, dbus.NameFlagDoNotQueue); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range []struct {
		path  dbus.ObjectPath
		iface string
	}{
		{hardware.Path, hardware.Destination}, {hardware.Path, "org.freedesktop.DBus.Properties"},
		{device.Path("Audio"), device.Interface("Audio")},
	} {
		if err := f.conn.Export(f, v.path, v.iface); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.ctx = ctx
	root := t.TempDir()
	for _, name := range []string{"fr_FR/book/intro.mp3", "fr_FR/book/interrupt.mp3", "fr_FR/book/outro-noalt.mp3", "book/next.mp3", "book/previous.mp3", "book/books/9782092512593/default/1.mp3", "book/books/9782092512593/default/2.mp3", "eightball/listen.mp3", "eightball/acquired.mp3", "fr_FR/eightball/answers/yes.mp3", "clock/signature.mp3", "clock/12/chime.mp3", "sleep/sleep.mp3", "wakeup/wakeup.mp3", "system/abort.wav", "radio/start.mp3", "rfid/rfid.wav"} {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("audio fixture"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	a.env.SoundsDirs = []string{root}
	a.rabbit = rabbit.New(rabbit.Options{SoundsDirs: []string{root}, ChorDirs: []string{root}}, rabbit.Handlers{OnState: a.onState, OnEvent: func(e rabbit.Event) {
		event := appEvent{Event: e, received: time.Now()}
		if dispatch != nil {
			dispatch(event)
		} else {
			select {
			case a.events <- event:
			case <-ctx.Done():
			}
		}
		select {
		case f.observed <- event:
		default:
		}
	}})
	if err := a.rabbit.Start(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); a.eventLoop(ctx) }()
	t.Cleanup(func() {
		a.stopMedia()
		cancel()
		stop, finish := context.WithTimeout(context.Background(), 3*time.Second)
		defer finish()
		a.rabbit.Stop(stop)
		<-done
	})
	pynabWait(t, "native hardware online", 3*time.Second, func() bool { state, _ := a.rabbit.State(); return state.HardwareReady() })
	if err := a.rabbit.SetMaintenance(ctx, false); err != nil {
		t.Fatal(err)
	}
	pynabWait(t, "native rabbit ready", 3*time.Second, a.rabbit.Ready)
	a.publishSettings(ctx)
	return f
}

func TestPynabNativeIntegration(t *testing.T) {
	a := testApp(t)
	f := startNative(t, a)
	serviceSession(t, a)
	adminHash := a.store.Get().Admin.Hash
	f.mu.Lock()
	f.hold = func(source string) bool {
		return strings.Contains(source, "/book/books/") || strings.HasSuffix(source, "/book/next.mp3") || strings.HasSuffix(source, "/eightball/listen.mp3")
	}
	f.mu.Unlock()
	t.Run("book navigation cancellation and exclusive chime", func(t *testing.T) {
		defer a.stopInteraction()
		f.event(t, "Tag", false, "st25tb", []byte{0xd0, 2, 0x18, 1, 2, 3, 4, 5}, "formatted", false, true, uint8(0), uint8(4), []byte("default/9782092512593"))
		first, firstID := f.play(t, 0, "book/books/9782092512593/default/1.mp3")
		a.chime(12)
		f.event(t, "EarMoved", uint8(1))
		f.canceled(t, firstID)
		next, nextID := f.play(t, first+1, "book/next.mp3")
		for len(f.observed) > 0 {
			<-f.observed
		}
		f.event(t, "EarMoved", uint8(1))
		delayed := f.observedEvent(t, func(e appEvent) bool { return e.Kind == "ear_moved" })
		f.finish(nextID)
		second, secondID := f.play(t, next+1, "book/books/9782092512593/default/2.mp3")
		a.events <- delayed
		time.Sleep(30 * time.Millisecond)
		f.mu.Lock()
		for _, id := range f.stopped {
			if id == secondID {
				t.Error("queued ear skipped chapter 2")
			}
		}
		f.mu.Unlock()
		f.event(t, "EarMoved", uint8(0))
		f.canceled(t, secondID)
		third, thirdID := f.play(t, second+1, "book/books/9782092512593/default/1.mp3")
		f.mu.Lock()
		for i, p := range f.plays {
			if i > first && i < third && strings.Contains(p.Source, "/clock/") {
				t.Error("chime interleaved between chapters")
			}
		}
		f.mu.Unlock()
		f.event(t, "Button", "click", uint64(0))
		f.canceled(t, thirdID)
		abort, _ := f.play(t, third+1, "system/abort.wav")
		spoken, _ := f.play(t, abort+1, "fr_FR/book/interrupt.mp3")
		chime, _ := f.play(t, spoken+1, "clock/12/chime.mp3")
		if chime <= spoken {
			t.Fatal("chime overtook interrupt prompt")
		}
		pynabWait(t, "book released", time.Second, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.interaction == nil })
	})
	t.Run("eightball hold and release preserves admin", func(t *testing.T) {
		pynabWait(t, "rabbit idle after book and chime", time.Second, func() bool { state, _ := a.rabbit.State(); return state.State == "idle" && len(a.mediaGate) == 0 })
		start := f.length()
		f.event(t, "Button", "click_and_hold", uint64(0))
		listen, listenID := f.play(t, start, "eightball/listen.mp3")
		f.event(t, "Button", "up", uint64(0))
		f.canceled(t, listenID)
		acquired, _ := f.play(t, listen+1, "eightball/acquired.mp3")
		f.play(t, acquired+1, "fr_FR/eightball/answers/yes.mp3")
		pynabWait(t, "eightball released", time.Second, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.interaction == nil })
		if !a.auth.Configured() || a.store.Get().Admin.Hash != adminHash {
			t.Fatal("eightball reset administrator")
		}
	})
	t.Run("rapid eightball release is reserved before the next event", func(t *testing.T) {
		pynabWait(t, "rabbit idle before rapid release", time.Second, func() bool { state, _ := a.rabbit.State(); return state.State == "idle" && len(a.mediaGate) == 0 })
		start := f.length()
		f.event(t, "Button", "click_and_hold", uint64(0))
		f.event(t, "Button", "up", uint64(0))
		acquired, _ := f.play(t, start, "eightball/acquired.mp3")
		f.play(t, acquired+1, "fr_FR/eightball/answers/yes.mp3")
		pynabWait(t, "rapid eightball interaction finished", time.Second, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.interaction == nil })
		if a.store.Get().Admin.Hash != adminHash {
			t.Fatal("rapid release reset admin")
		}
	})
	t.Run("RFID webhook saved association and disable", func(t *testing.T) {
		hits := make(chan struct{}, 2)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" {
				t.Error("webhook method", r.Method)
			}
			hits <- struct{}{}
		}))
		defer srv.Close()
		uid := []byte{0xd0, 2, 0x18, 1, 2, 3, 4, 6}
		key := tagFromHardware(hardware.Tag{UID: uid}).UID
		if _, err := a.store.Update(func(s *config.Settings) error {
			s.Tags[key] = config.TagAction{App: "webhook", Value: srv.URL}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		emit := func() {
			f.event(t, "Tag", false, "st25tb", uid, "formatted", false, true, uint8(0), uint8(13), []byte("DATA_IN_LOCAL_DB"))
		}
		emit()
		select {
		case <-hits:
		case <-time.After(time.Second):
			t.Fatal("associated webhook not called")
		}
		a.store.Update(func(s *config.Settings) error { s.Services.Webhooks = false; return nil })
		emit()
		pynabWait(t, "disabled webhook rejected", time.Second, func() bool {
			a.mu.Lock()
			defer a.mu.Unlock()
			return strings.Contains(a.serviceErrors["webhook"], "désactivés")
		})
		select {
		case <-hits:
			t.Fatal("disabled webhook called")
		default:
		}
	})
	for _, reason := range []string{"resync", "context cancellation", "hardware loss"} {
		t.Run("ordinary media releases on "+reason, func(t *testing.T) {
			f.mu.Lock()
			f.hold = func(source string) bool { return strings.HasSuffix(source, "system/abort.wav") }
			f.mu.Unlock()
			start := f.length()
			ctx, cancel := context.WithCancel(a.ctx)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- a.media(ctx, sequence("system/abort.wav", ""), time.Minute) }()
			_, id := f.play(t, start, "system/abort.wav")
			switch reason {
			case "resync":
				a.resync()
			case "context cancellation":
				cancel()
			case "hardware loss":
				if _, err := f.conn.ReleaseName(hardware.Destination); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("lost playback succeeded")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("media gate stranded")
			}
			f.canceled(t, id)
			pynabWait(t, "released media gate", time.Second, func() bool { return len(a.mediaGate) == 0 })
			f.mu.Lock()
			f.hold = nil
			f.mu.Unlock()
			if reason == "hardware loss" {
				f.conn.RequestName(hardware.Destination, dbus.NameFlagDoNotQueue)
				pynabWait(t, "hardware reconnect", 3*time.Second, a.rabbit.Ready)
			}
			pynabWait(t, "rabbit ready after cleanup", 3*time.Second, a.rabbit.Ready)
			if err := a.media(a.ctx, sequence("system/abort.wav", ""), time.Second); err != nil {
				t.Fatal("next playback blocked", err)
			}
		})
	}
}

func TestRadioRetainsGateUntilNativeCancellationFinishes(t *testing.T) {
	a := testApp(t)
	f := startNative(t, a)
	station := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write([]byte("ID3fixture"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer station.Close()
	f.mu.Lock()
	f.hold = func(source string) bool { return strings.HasPrefix(source, "http://") }
	f.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- a.playRadio(a.ctx, station.URL) }()
	id := ""
	pynabWait(t, "radio on native audio connection", time.Second, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, p := range f.plays {
			if p.Kind == "stream" {
				id = p.ID
				return true
			}
		}
		return false
	})
	if len(a.mediaGate) != 1 {
		t.Fatal("radio released its gate before stream completed")
	}
	if _, err := a.device.Call(a.ctx, "Manager", "RegisterAgent", device.Path("Agent")); err != nil {
		t.Fatal(err)
	}
	if appFixture(t, a).Agent(a.ctx, "Acquire", "radio-update").Err == nil {
		t.Fatal("radio admitted maintenance")
	}
	a.stopRadio()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stopped radio returned success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("radio did not stop")
	}
	f.canceled(t, id)
	if len(a.mediaGate) != 0 {
		t.Fatal("radio left gate held")
	}
	if err := a.media(a.ctx, sequence("system/abort.wav", ""), time.Second); err != nil {
		t.Fatal("next sound blocked", err)
	}
}
