package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/guilhem/nabos/services/internal/config"
	"github.com/guilhem/nabos/services/internal/device"
	"github.com/guilhem/nabos/services/internal/hardware"
	"github.com/guilhem/nabos/services/internal/rabbit"
)

// Replace the actual bus owner while retaining the hardware behavior fixture.
func replaceHardwareOwner(t *testing.T, a *App, f *nativeFixture) *dbus.Conn {
	t.Helper()
	replacement, err := dbus.ConnectSystemBus()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { replacement.Close() })
	for _, iface := range []string{hardware.Destination, "org.freedesktop.DBus.Properties"} {
		if err := replacement.Export(f, hardware.Path, iface); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.conn.ReleaseName(hardware.Destination); err != nil {
		t.Fatal(err)
	}
	pynabWait(t, "hardware departure observed", time.Second, func() bool { return !a.rabbit.Ready() })
	if reply, err := replacement.RequestName(hardware.Destination, dbus.NameFlagDoNotQueue); err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatal("replacement hardware owner", reply, err)
	}
	pynabWait(t, "replacement hardware ready", 3*time.Second, a.rabbit.Ready)
	return replacement
}

func TestApplicationRejectsBufferedEventsFromDepartedHardware(t *testing.T) {
	a := testApp(t)
	// Capture events before app dispatch to model the buffered application queue.
	f := startNativeDispatch(t, a, func(appEvent) {})
	serviceSession(t, a)
	adminHash := a.store.Get().Admin.Hash
	var poweroffs, webhooks atomic.Int32
	if err := appFixture(t, a).Conn.ExportMethodTable(map[string]interface{}{
		"PowerOff": func() *dbus.Error { poweroffs.Add(1); return nil },
	}, device.Path("System"), device.Interface("System")); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { webhooks.Add(1) }))
	defer srv.Close()
	uid := []byte{0xd0, 2, 0x18, 1, 2, 3, 4, 5}
	_, err := a.store.Update(func(s *config.Settings) error {
		asleep := true
		s.Clock.Override = &asleep
		s.Services.Eightball, s.Services.Webhooks = true, true
		s.Tags[tagFromHardware(hardware.Tag{UID: uid}).UID] = config.TagAction{App: "webhook", Value: srv.URL}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var buffered []appEvent
	for _, button := range []string{"down", "double_click_and_hold", "triple_click", "click", "click_and_hold"} {
		f.event(t, "Button", button, a.auth.MonoNow())
		buffered = append(buffered, f.observedEvent(t, func(e appEvent) bool { return e.Kind == "button" && e.Button == button }))
	}
	f.event(t, "Tag", false, "st25tb", uid, "formatted", false, true, uint8(0), uint8(13), []byte("DATA_IN_LOCAL_DB"))
	buffered = append(buffered, f.observedEvent(t, func(e appEvent) bool { return e.Kind == "rfid" }))
	for _, event := range buffered {
		if !a.rabbit.EventCurrent(event.Event) {
			t.Fatal("fixture did not capture an authenticated current event")
		}
	}
	replacement := replaceHardwareOwner(t, a, f)
	for _, event := range buffered {
		if a.rabbit.EventCurrent(event.Event) {
			t.Fatal("departed owner's event stayed current")
		}
		a.onEvent(event)
	}
	// Zero generation is also untrusted, including while hardware is online.
	a.onEvent(appEvent{Event: rabbit.Event{Kind: "button", Button: "double_click_and_hold"}})
	a.mu.Lock()
	interaction, tag := a.interaction, a.lastTag
	a.mu.Unlock()
	if a.store.Get().Admin.Hash != adminHash || !a.auth.Configured() || poweroffs.Load() != 0 || webhooks.Load() != 0 || interaction != nil || tag != nil || !*a.store.Get().Clock.Override {
		t.Fatal("buffered departed-owner event changed the product")
	}
	// The replacement's real physical reset still works.
	if err := replacement.Emit(hardware.Path, hardware.Destination+".Button", "double_click_and_hold", uint64(0)); err != nil {
		t.Fatal(err)
	}
	fresh := f.observedEvent(t, func(e appEvent) bool { return e.Kind == "button" && e.Button == "double_click_and_hold" })
	a.onEvent(fresh)
	if a.auth.Configured() {
		t.Fatal("current hardware reset was rejected")
	}
}

func TestInteractionRejectsBufferedEventsFromDepartedHardware(t *testing.T) {
	a := testApp(t)
	f := startNativeDispatch(t, a, func(appEvent) {})
	f.event(t, "EarMoved", uint8(1))
	ear := f.observedEvent(t, func(e appEvent) bool { return e.Kind == "ear_moved" })
	f.event(t, "Button", "click", uint64(0))
	click := f.observedEvent(t, func(e appEvent) bool { return e.Kind == "button" && e.Button == "click" })
	f.event(t, "Button", "up", uint64(0))
	up := f.observedEvent(t, func(e appEvent) bool { return e.Kind == "button" && e.Button == "up" })
	replacement := replaceHardwareOwner(t, a, f)
	if err := replacement.Emit(hardware.Path, hardware.Destination+".Button", "down", uint64(1)); err != nil {
		t.Fatal(err)
	}
	// A current event with no interaction control serves as a queue barrier.
	// Consuming it proves the stale controls before it have finished dispatch.
	barrier := f.observedEvent(t, func(e appEvent) bool { return e.Kind == "button" && e.Button == "down" })

	t.Run("book cancellation and navigation", func(t *testing.T) {
		f.mu.Lock()
		f.hold = func(source string) bool { return strings.HasSuffix(source, "/system/abort.wav") }
		f.mu.Unlock()
		ctx, cancel := context.WithCancel(a.ctx)
		defer cancel()
		s := &interaction{kind: "book", ctx: ctx, cancel: cancel, events: make(chan appEvent, 4)}
		s.events <- ear
		s.events <- click
		s.events <- barrier
		type outcome struct {
			control string
			err     error
		}
		done := make(chan outcome, 1)
		start := f.length()
		go func() {
			control, err := a.interactivePlay(s, sequence("system/abort.wav", ""), true)
			done <- outcome{control, err}
		}()
		_, id := f.play(t, start, "system/abort.wav")
		pynabWait(t, "stale interaction events consumed", time.Second, func() bool { return len(s.events) == 0 })
		select {
		case result := <-done:
			t.Fatal("stale control canceled current playback", result)
		default:
		}
		f.mu.Lock()
		for _, stopped := range f.stopped {
			if stopped == id {
				t.Error("stale control stopped native audio")
			}
		}
		f.mu.Unlock()
		if err := replacement.Emit(hardware.Path, hardware.Destination+".Button", "click", uint64(0)); err != nil {
			t.Fatal(err)
		}
		s.events <- f.observedEvent(t, func(e appEvent) bool { return e.Kind == "button" && e.Button == "click" })
		select {
		case result := <-done:
			if result.err != nil || result.control != "click" {
				t.Fatal("current physical cancellation failed", result)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("current physical cancellation did not finish")
		}
		f.canceled(t, id)
	})

	t.Run("eightball release after listening", func(t *testing.T) {
		ctx, cancel := context.WithCancel(a.ctx)
		defer cancel()
		s := &interaction{kind: "eightball", ctx: ctx, cancel: cancel, events: make(chan appEvent, 4)}
		s.events <- up
		s.events <- barrier
		start := f.length()
		done := make(chan error, 1)
		go func() { done <- a.askEightball(s) }()
		f.play(t, start, "eightball/listen.mp3")
		pynabWait(t, "eightball waiting after listening", time.Second, func() bool { state, _ := a.rabbit.State(); return state.State == "idle" && len(s.events) == 0 })
		select {
		case err := <-done:
			t.Fatal("stale release answered eightball", err)
		default:
		}
		f.mu.Lock()
		for _, play := range f.plays[start:] {
			if strings.HasSuffix(play.Source, "/eightball/acquired.mp3") {
				t.Error("stale release advanced eightball")
			}
		}
		f.mu.Unlock()
		if err := replacement.Emit(hardware.Path, hardware.Destination+".Button", "up", uint64(0)); err != nil {
			t.Fatal(err)
		}
		s.events <- f.observedEvent(t, func(e appEvent) bool { return e.Kind == "button" && e.Button == "up" })
		select {
		case err := <-done:
			if err != nil {
				t.Fatal("current physical release failed", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("current physical release did not answer eightball")
		}
		f.play(t, start, "eightball/acquired.mp3")
	})
}
