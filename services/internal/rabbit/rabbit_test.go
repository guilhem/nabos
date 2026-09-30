package rabbit

import (
	"context"
	"errors"
	"github.com/godbus/dbus/v5"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/guilhem/nabos/services/internal/device"
	"github.com/guilhem/nabos/services/internal/hardware"
)

func TestResourceBoundaryAndTypedValidation(t *testing.T) {
	if err := (Result{Status: "canceled"}).Err(); !errors.Is(err, context.Canceled) {
		t.Fatal("normal cancellation reported as a service failure", err)
	}
	if err := (Result{Status: "canceled", Error: "hardware cleanup failed"}).Err(); errors.Is(err, context.Canceled) || err == nil {
		t.Fatal("cancellation hid a cleanup failure", err)
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "fr_FR/clock/7"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "system"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"fr_FR/clock/7/a.mp3", "system/abort.wav", "system/notes.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(t.TempDir(), "secret.mp3")
	if err := os.WriteFile(outside, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "system/link.mp3")); err != nil {
		t.Fatal(err)
	}
	r := resources{sounds: []string{root}, locale: "fr_FR"}
	for _, tc := range []struct{ spec, suffix string }{{"clock/7/*.mp3", "fr_FR/clock/7/a.mp3"}, {"missing.mp3;system/abort.wav", "system/abort.wav"}} {
		if got := r.find(false, tc.spec); got != filepath.Join(root, tc.suffix) {
			t.Fatal(tc, got)
		}
	}
	for _, bad := range []string{"system/notes.txt", "system/link.mp3", "../secret.mp3", "a/../b", "/etc/passwd", "a;/b", "x\\y"} {
		if got := r.find(false, bad); got != "" {
			t.Fatal(bad, got)
		}
	}
	r.setLocale("en_US")
	if r.find(false, "clock/7/*.mp3") != "" {
		t.Fatal("locale ignored")
	}
	r.setLocale("../../")
	if r.locale != "en_US" {
		t.Fatal("locale traversal")
	}
	stream := "http://127.0.0.1:40123/radio/Ab_-0123456789xyz"
	if err := validate(Command{Action: Play, Sequence: []Item{{Stream: stream}}}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"https://127.0.0.1:40123/radio/Ab_-0123456789xyz", "http://localhost:40123/radio/Ab_-0123456789xyz", "http://127.0.0.1:04012/radio/Ab_-0123456789xyz", "http://127.0.0.1:65536/radio/Ab_-0123456789xyz", stream + "?x=1"} {
		if validStream(bad) {
			t.Fatal(bad)
		}
	}
	for _, c := range []Command{{Action: Play, Sequence: []Item{{Stream: stream, Audio: []string{}}}}, {Action: Message, Body: []Item{{Stream: stream}}}, {Action: Play, Sequence: []Item{{Choreography: "data:application/base64,AA"}}}, {Action: Play, Sequence: []Item{{Choreography: streamingURN + ":8"}}}} {
		if validate(c) == nil {
			t.Fatal("invalid item accepted", c)
		}
	}
	loaded := (&Engine{res: resources{sounds: r.sounds, locale: r.locale}}).preload([]Item{{}, {Audio: []string{}}})
	if loaded[0].audio != nil || loaded[1].audio == nil {
		t.Fatal("absent and explicit empty audio collapsed")
	}
	for _, length := range []int{3, 5, 6, 8, 9, 11} {
		if hardware.ValidTag("iso14443a_t2t", make([]byte, length), nil, 20) {
			t.Fatal("invalid T2T UID", length)
		}
	}
	for _, length := range []int{4, 7, 10} {
		if !hardware.ValidTag("iso14443a_t2t", make([]byte, length), nil, 20) {
			t.Fatal("valid T2T UID", length)
		}
	}
}
func TestAudioLateIDCleanupKeepsOwnerAndSerializesStart(t *testing.T) {
	f := newBus(t)
	f.mu.Lock()
	f.startEntered = make(chan struct{}, 2)
	f.startRelease = make(chan struct{})
	f.mu.Unlock()
	a := &audio{}
	defer a.close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := a.start(ctx, source{"file", "/late.wav"}); done <- err }()
	<-f.startEntered
	cancel()
	next := make(chan error, 1)
	go func() { _, err := a.start(context.Background(), source{"file", "/next.wav"}); next <- err }()
	time.Sleep(30 * time.Millisecond)
	f.mu.Lock()
	if len(f.starts) != 1 {
		t.Fatal("concurrent Start bypassed serialization")
	}
	f.mu.Unlock()
	close(f.startRelease)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-next; err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !reflect.DeepEqual(f.stops, []string{"audio-1"}) || f.stopOwners[0] != f.startOwners[0] || f.startOwners[0] != f.startOwners[1] {
		t.Fatal("late ID lost its owner", f.starts, f.stops, f.startOwners, f.stopOwners)
	}
}
func TestAudioLostStartReplyClosesOwnerAndNeverReplays(t *testing.T) {
	f := newBus(t)
	f.mu.Lock()
	f.loseReply = true
	f.mu.Unlock()
	a := &audio{}
	defer a.close()
	started := time.Now()
	if _, err := a.start(context.Background(), source{"file", "/ambiguous.wav"}); err == nil {
		t.Fatal("lost reply succeeded")
	}
	if time.Since(started) > device.Timeout+time.Second {
		t.Fatal("Start timeout unbounded")
	}
	a.mu.Lock()
	if a.client != nil {
		t.Fatal("ambiguous Start retained owner")
	}
	a.mu.Unlock()
	f.mu.Lock()
	oldOwner := f.startOwners[0]
	if len(f.starts) != 1 {
		t.Fatal("Start replayed")
	}
	f.loseReply = false
	f.mu.Unlock()
	var owner string
	if err := f.conn.BusObject().Call("org.freedesktop.DBus.GetNameOwner", 0, oldOwner).Store(&owner); err == nil {
		t.Fatal("ambiguous Start connection still owns audio")
	}
	a.resetLoss() // The next admitted activity follows the owner-cleanup barrier.
	if _, err := a.start(context.Background(), source{"file", "/explicit-new.wav"}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.starts) != 2 || f.startOwners[1] == oldOwner {
		t.Fatal("fresh work reused ambiguous owner", f.starts, f.startOwners)
	}
}
func TestMonotonicChoreographyBatchesFrameAndBoundsDrivers(t *testing.T) {
	f := newBus(t)
	e := readyEngine(t, f, Handlers{})
	if err := e.SetMaintenance(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.frames = nil
	f.frameTimes = nil
	f.ledDelay = 25 * time.Millisecond
	f.mu.Unlock()
	// Same timestamp's two reversed LED updates become one D-Bus frame.
	data := []byte{0, 1, 5, 0, 7, 1, 255, 0, 0, 0, 0, 0, 8, 0, 8, 0, 0, 7, 3, 0, 255, 0, 0, 0, 1, 7, 1, 0, 0, 255, 0, 0, 1, 7, 1, 255, 255, 255, 0, 0}
	it := interpreter{e: e}
	if err := it.binary(e.hw.Bind(context.Background()), data, false, 0); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	frames, times := append([][]hardware.Color(nil), f.frames...), append([]time.Time(nil), f.frameTimes...)
	f.mu.Unlock()
	if len(frames) != 3 || len(frames[0]) != 2 || frames[0][0].Index != 1 || frames[0][1].Index != 3 {
		t.Fatal("LED frame not batched/reversed", frames)
	}
	if delta := times[2].Sub(times[0]); delta < 75*time.Millisecond || delta > 130*time.Millisecond {
		t.Fatal("roundtrips accumulated on choreography deadlines", delta)
	}
	for _, bad := range [][]byte{{0, 8, 2, 0, 0}, {0, 7, 5, 0, 0, 0, 0, 0}, {0, 20, 2, 0}, {0, 7, 1}} {
		if err := it.binary(e.hw.Bind(context.Background()), bad, false, 0); err == nil {
			t.Fatal("malformed driver operands accepted", bad)
		}
	}
}
func TestQueueTTLDoesNotBoundPlaybackAndSleepOrdersBehindWork(t *testing.T) {
	f := newBus(t)
	f.mu.Lock()
	f.playDelay = 200 * time.Millisecond
	f.mu.Unlock()
	e := readyEngine(t, f, Handlers{})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "test.wav"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	e.res.sounds = []string{dir}
	started := make(chan struct{})
	playDone := make(chan Result, 1)
	go func() {
		r, err := e.DoStarted(context.Background(), Command{ID: "active", Action: Play, Deadline: time.Now().Add(70 * time.Millisecond), Sequence: []Item{{Audio: []string{"test.wav"}}}}, started)
		if err != nil {
			r = failure(err)
		}
		playDone <- r
	}()
	<-started
	sleep, err := e.submit(context.Background(), Command{Action: Sleep}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	expiredPlay, err := e.submit(context.Background(), Command{Action: Play, Deadline: time.Now().Add(50 * time.Millisecond)}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if r := <-expiredPlay.result; r.Status != "expired" {
		t.Fatal(r)
	}
	if r := <-playDone; r.Status != "ok" {
		t.Fatal("TTL stopped admitted playback", r)
	}
	if r := <-sleep.result; r.Status != "ok" {
		t.Fatal(r)
	}
	eventually(t, func() bool { s, _ := e.State(); return s.State == "asleep" })
	f.mu.Lock()
	if len(f.starts) != 1 {
		t.Fatal("expired queued audio ran", f.starts)
	}
	f.mu.Unlock()
	queued, err := e.submit(context.Background(), Command{Action: Play}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-queued.result:
		t.Fatal("asleep playback ran", r)
	case <-time.After(30 * time.Millisecond):
	}
	if r, err := e.Do(context.Background(), Command{Action: Wakeup}); err != nil || r.Err() != nil {
		t.Fatal(r, err)
	}
	if r := <-queued.result; r.Err() != nil {
		t.Fatal(r)
	}
}
func TestClickConsumesEventAndTargetedCancelDrainsBeforeNextAudio(t *testing.T) {
	f := newBus(t)
	f.mu.Lock()
	f.waitRelease = make(chan struct{})
	f.stopDelay = 60 * time.Millisecond
	f.mu.Unlock()
	events := make(chan Event, 8)
	e := readyEngine(t, f, Handlers{OnEvent: func(ev Event) { events <- ev }})
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "system"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"test.wav", "system/abort.wav"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	e.res.sounds = []string{dir}
	started := make(chan struct{})
	done := make(chan Result, 1)
	go func() {
		r, err := e.DoStarted(context.Background(), Command{ID: "click-play", Action: Play, Cancelable: true, Sequence: []Item{{Audio: []string{"test.wav"}}}}, started)
		if err != nil {
			r = failure(err)
		}
		done <- r
	}()
	<-started
	eventually(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.waits) == 1 })
	if err := f.conn.Emit(hardware.Path, hardware.Destination+".Button", "click", uint64(0)); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.starts) == 2 })
	close(f.waitRelease)
	if r := <-done; r.Status != "canceled" {
		t.Fatal(r)
	}
	select {
	case ev := <-events:
		t.Fatal("cancel click propagated", ev)
	default:
	}
	f.mu.Lock()
	if f.starts[1] != filepath.Join(dir, "system/abort.wav") || len(f.stops) == 0 || f.stopOwners[0] != f.startOwners[0] || f.waitOwners[0] != f.startOwners[0] {
		t.Fatal("feedback/owner cleanup", f.starts, f.stops)
	}
	f.mu.Unlock()
}
func TestFailedPhysicalCleanupBlocksReadiness(t *testing.T) {
	f := newBus(t)
	e := readyEngine(t, f, Handlers{})
	f.mu.Lock()
	f.idleError = true
	f.mu.Unlock()
	if err := e.SetMaintenance(context.Background(), true); err == nil {
		t.Fatal("physical cleanup failure claimed safety")
	}
	if e.Ready() {
		t.Fatal("ready after failed physical cleanup")
	}
	if err := e.SetMaintenance(context.Background(), false); err == nil {
		t.Fatal("unsafe maintenance release")
	}
}

func TestHardwareReconnectionRejectsOldSignalsAndBoundWorker(t *testing.T) {
	f := newBus(t)
	events := make(chan hardware.Event, 32)
	client := hardware.New(func(ev hardware.Event) { events <- ev })
	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { ctx, cancel := cleanupContext(); defer cancel(); client.Stop(ctx) }()
	eventually(t, func() bool { _, ok := client.Snapshot(); return ok })
	bound := client.Bind(context.Background())
	for len(events) > 0 {
		<-events
	}
	if _, err := f.conn.ReleaseName(hardware.Destination); err != nil {
		t.Fatal(err)
	}
	conn, err := hardware.Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	g := &busFixture{conn: conn, status: hardware.Status{Model: "recovered", Simulated: true, LeftEar: "ok", RightEar: "ok", Leds: true, Button: true, RFID: "none"}}
	if err := conn.Export(g, hardware.Path, hardware.Destination); err != nil {
		t.Fatal(err)
	}
	if err := conn.Export(fixtureProperties{g}, hardware.Path, "org.freedesktop.DBus.Properties"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.RequestName(hardware.Destination, dbus.NameFlagDoNotQueue); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { s, ok := client.Snapshot(); return ok && s.Model == "recovered" })
	if err := client.SetLeds(bound, []hardware.Color{{Index: 1}}); !errors.Is(err, hardware.ErrStale) {
		t.Fatal("old activity mutated recovered owner", err)
	}
	for len(events) > 0 {
		<-events
	}
	if err := f.conn.Emit(hardware.Path, hardware.Destination+".Button", "click", uint64(0)); err != nil {
		t.Fatal(err)
	}
	if err := conn.Emit(hardware.Path, hardware.Destination+".Button", "down", uint64(123456789)); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Kind == "button" {
				if ev.Button != "down" || ev.EdgeMonotonicNS != 123456789 {
					t.Fatal("stale event escaped", ev)
				}
				return
			}
		case <-deadline:
			t.Fatal("current event missing")
		}
	}
}
func TestAudioOwnerDisappearanceClosesConnectionAndStopsActivity(t *testing.T) {
	f := newBus(t)
	f.mu.Lock()
	f.waitRelease = make(chan struct{})
	f.mu.Unlock()
	defer close(f.waitRelease)
	a := &audio{}
	defer a.close()
	id, err := a.start(context.Background(), source{"file", "/owned.wav"})
	if err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	client := a.client
	a.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- a.wait(context.Background(), id) }()
	eventually(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.waits) > 0 })
	if _, err := f.conn.ReleaseName(device.Destination); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("owner disappearance succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("owner loss did not interrupt Wait")
	}
	if client.Conn.Connected() {
		t.Fatal("dedicated audio owner connection survived daemon loss")
	}
	if err := a.stop(); err == nil {
		t.Fatal("unknown cleanup reported safe")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.starts) != 1 {
		t.Fatal("owner loss replayed audio", f.starts)
	}
}

func TestMaintenanceDropsQueuedWorkAndBufferedEventIsRevoked(t *testing.T) {
	f := newBus(t)
	events := make(chan Event, 4)
	e := readyEngine(t, f, Handlers{OnEvent: func(ev Event) { events <- ev }})
	if r, err := e.Do(context.Background(), Command{Action: Sleep}); err != nil || r.Err() != nil {
		t.Fatal(r, err)
	}
	queued, err := e.submit(context.Background(), Command{ID: "before-maintenance", Action: Play}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.SetMaintenance(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if r := <-queued.result; r.Status != "canceled" {
		t.Fatal("queued work survived maintenance", r)
	}
	if err = e.SetMaintenance(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if r, err := e.Do(context.Background(), Command{Action: Wakeup}); err != nil || r.Err() != nil {
		t.Fatal(r, err)
	}
	if err = f.conn.Emit(hardware.Path, hardware.Destination+".Button", "down", uint64(1234)); err != nil {
		t.Fatal(err)
	}
	var ev Event
	select {
	case ev = <-events:
	case <-time.After(3 * time.Second):
		t.Fatal("missing button")
	}
	if !e.EventCurrent(ev) {
		t.Fatal("current event refused")
	}
	e.hw.Drop()
	if e.EventCurrent(ev) {
		t.Fatal("buffered old-owner event remained trusted")
	}
}
func TestTargetedCancelResultWaitsForCleanup(t *testing.T) {
	f := newBus(t)
	f.mu.Lock()
	f.waitRelease = make(chan struct{})
	f.stopDelay = 100 * time.Millisecond
	f.mu.Unlock()
	e := readyEngine(t, f, Handlers{})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "test.wav"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	e.res.sounds = []string{dir}
	started := make(chan struct{})
	done := make(chan Result, 1)
	go func() {
		r, err := e.DoStarted(context.Background(), Command{ID: "owned", Action: Play, Sequence: []Item{{Audio: []string{"test.wav"}}}}, started)
		if err != nil {
			r = failure(err)
		}
		done <- r
	}()
	<-started
	eventually(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.waits) > 0 })
	queued, err := e.submit(context.Background(), Command{ID: "next", Action: Play, Sequence: []Item{{Audio: []string{"test.wav"}}}}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	canceled := make(chan Result, 1)
	go func() {
		r, err := e.Do(context.Background(), Command{Action: Cancel, Target: "owned"})
		if err != nil {
			r = failure(err)
		}
		canceled <- r
	}()
	time.Sleep(30 * time.Millisecond)
	select {
	case r := <-canceled:
		t.Fatal("cancel acknowledged before Stop completed", r)
	default:
	}
	f.mu.Lock()
	if len(f.starts) != 1 {
		t.Fatal("next activity overtook cleanup", f.starts)
	}
	f.mu.Unlock()
	if r := <-canceled; r.Err() != nil {
		t.Fatal(r)
	}
	if r := <-done; r.Status != "canceled" {
		t.Fatal(r)
	}
	close(f.waitRelease)
	f.mu.Lock()
	f.waitRelease = nil
	f.mu.Unlock()
	if r := <-queued.result; r.Err() != nil {
		t.Fatal(r)
	}
	// defer above must not close a nil/already closed channel.
}

func TestRFIDBytesWriteWhileAsleepAndNoseLEDPolicy(t *testing.T) {
	f := newBus(t)
	e := readyEngine(t, f, Handlers{})
	if r, err := e.Do(context.Background(), Command{Action: Sleep}); err != nil || r.Err() != nil {
		t.Fatal(r, err)
	}
	tag := TagWrite{Tech: "st25tb", UID: []byte{1, 2, 3, 4, 5, 0x18, 2, 0xd0}, Picture: 255, App: 4, Data: []byte{0, 1, 0xff, 0, 10}, Timeout: 20}
	r, err := e.Do(context.Background(), Command{Action: RfidWrite, Tag: &tag})
	if err != nil || r.Err() != nil || !reflect.DeepEqual(r.UID, tag.UID) {
		t.Fatal(r, err)
	}
	f.mu.Lock()
	if len(f.tagWrites) != 1 || !reflect.DeepEqual(f.tagWrites[0], tag) || f.writeOwners[0] != f.writeWaitOwners[0] || f.writeOwners[0] != f.owner {
		t.Fatal("tag bytes/owner changed", f.tagWrites, f.writeOwners, f.writeWaitOwners)
	}
	redNose := false
	for _, frame := range f.frames {
		if len(frame) == 1 && frame[0] == (hardware.Color{Index: 0, Red: 255}) {
			redNose = true
		}
	}
	if !redNose {
		t.Fatal("tag write did not signal nose red")
	}
	f.writeOutcome = "timeout"
	f.mu.Unlock()
	r, err = e.Do(context.Background(), Command{Action: RfidWrite, Tag: &tag})
	if err != nil || r.Status != "error" || r.Error != "timeout" {
		t.Fatal("RFID outcome changed", r, err)
	}
	s, _ := e.State()
	if s.State != "asleep" {
		t.Fatal("RFID woke sleeping rabbit", s)
	}
}

func TestChoreographyCannotReconnectAudioWithinActivity(t *testing.T) {
	f := newBus(t)
	e := readyEngine(t, f, Handlers{})
	sounds, chors := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(sounds, "choreographies"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range midi {
		if err := os.WriteFile(filepath.Join(sounds, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// The second note follows a real monotonic delay; loss occurs while the first note owns audio.
	if err := os.WriteFile(filepath.Join(chors, "notes.chor"), []byte{0, 1, 10, 0, 16, 2, 16}, 0644); err != nil {
		t.Fatal(err)
	}
	e.res.sounds, e.res.chors = []string{sounds}, []string{chors}
	done := make(chan Result, 1)
	go func() {
		r, err := e.Do(context.Background(), Command{Action: Play, Sequence: []Item{{Choreography: "notes.chor"}}})
		if err != nil {
			r = failure(err)
		}
		done <- r
	}()
	eventually(t, func() bool { e.audio.mu.Lock(); defer e.audio.mu.Unlock(); return e.audio.current != "" })
	if _, err := f.conn.ReleaseName(device.Destination); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { e.audio.mu.Lock(); defer e.audio.mu.Unlock(); return e.audio.loss != nil })
	if _, err := f.conn.RequestName(device.Destination, dbus.NameFlagDoNotQueue); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.Status != "error" {
			t.Fatal("lost audio activity succeeded", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("activity did not drain")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.starts) != 1 {
		t.Fatal("choreography reconnected and replayed a note", f.starts, f.startOwners)
	}
}
func TestInterruptedSleepAndWakeSetupRemainPending(t *testing.T) {
	f := newBus(t)
	e := readyEngine(t, f, Handlers{})
	eventually(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.pulses > 0 })
	f.mu.Lock()
	f.readDelay = 100 * time.Millisecond
	f.readEntered = make(chan struct{}, 8)
	f.mu.Unlock()
	if r, err := e.Do(context.Background(), Command{Action: Sleep}); err != nil || r.Err() != nil {
		t.Fatal(r, err)
	}
	select {
	case <-f.readEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("sleep setup did not start")
	}
	if r, err := e.Do(context.Background(), Command{Action: Indicator, Animation: &Animation{Tempo: 1, Frames: [][3]RGB{{{1, 2, 3}, {4, 5, 6}, {7, 8, 9}}}}}); err != nil || r.Err() != nil {
		t.Fatal(r, err)
	}
	eventually(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.status.Left == 10 && f.status.Right == 10 })
	for len(f.readEntered) > 0 {
		<-f.readEntered
	}
	if r, err := e.Do(context.Background(), Command{Action: Wakeup}); err != nil || r.Err() != nil {
		t.Fatal(r, err)
	}
	select {
	case <-f.readEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("wake setup did not start")
	}
	e.SetSettings("fr_FR", "lan")
	eventually(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.status.Left == 0 && f.status.Right == 0 })
}
