package rabbit

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/guilhem/nabos/services/internal/device"
	"github.com/guilhem/nabos/services/internal/hardware"
)

type busFixture struct {
	mu                                  sync.Mutex
	conn                                *dbus.Conn
	status                              hardware.Status
	owner                               string
	frames                              [][]hardware.Color
	frameTimes                          []time.Time
	ledDelay                            time.Duration
	pulses                              int
	starts, stops, waits                []string
	startOwners, waitOwners, stopOwners []string
	startDelay, playDelay, stopDelay    time.Duration
	startEntered                        chan struct{}
	startRelease                        chan struct{}
	waitRelease                         chan struct{}
	loseReply                           bool
	idleError                           bool
	tagWrites                           []TagWrite
	writeOwners, writeWaitOwners        []string
	writeOutcome                        string
	readDelay                           time.Duration
	readEntered                         chan struct{}
}

func newBus(t *testing.T) *busFixture {
	t.Helper()
	cmd := exec.Command("dbus-daemon", "--session", "--nofork", "--print-address=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	address, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("NABOS_DEVICE_BUS_ADDRESS", strings.TrimSpace(address))
	conn, err := dbus.Connect(strings.TrimSpace(address))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	f := &busFixture{conn: conn, status: hardware.Status{Model: "test", Simulated: true, LeftEar: "ok", RightEar: "ok", Leds: true, Button: true, RFID: "st25tb", Left: 0, Right: 0}}
	for _, name := range []string{hardware.Destination, device.Destination, "org.freedesktop.systemd1"} {
		reply, err := conn.RequestName(name, dbus.NameFlagDoNotQueue)
		if err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
			t.Fatal(name, reply, err)
		}
	}
	for _, entry := range []struct {
		value any
		path  dbus.ObjectPath
		iface string
	}{{f, hardware.Path, hardware.Destination}, {f, device.Path("Audio"), device.Interface("Audio")}, {fixtureProperties{f}, hardware.Path, "org.freedesktop.DBus.Properties"}, {fixtureSystemd{t}, "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager"}, {fixtureUnit{}, "/org/freedesktop/systemd1/unit/hardware", "org.freedesktop.DBus.Properties"}} {
		if err = conn.Export(entry.value, entry.path, entry.iface); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

type fixtureProperties struct{ f *busFixture }

func (p fixtureProperties) Get(iface, name string) (dbus.Variant, *dbus.Error) {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	if iface != hardware.Destination {
		return dbus.Variant{}, dbus.MakeFailedError(errors.New("unknown interface"))
	}
	switch name {
	case "Status":
		return dbus.MakeVariant(p.f.status), nil
	case "Ready":
		return dbus.MakeVariant(p.f.status.Ready()), nil
	}
	return dbus.Variant{}, dbus.MakeFailedError(errors.New("unknown property"))
}

type fixtureSystemd struct{ t *testing.T }

func (s fixtureSystemd) GetUnitByPIDFD(fd dbus.UnixFD) (dbus.ObjectPath, string, []byte, *dbus.Error) {
	defer syscall.Close(int(fd))
	info, err := os.ReadFile(fmt.Sprintf("/proc/self/fdinfo/%d", fd))
	if err != nil {
		return "", "", nil, dbus.MakeFailedError(err)
	}
	if !strings.Contains(string(info), "Pid:\t"+strconv.Itoa(os.Getpid())+"\n") {
		return "", "", nil, dbus.MakeFailedError(errors.New("wrong process fd"))
	}
	return "/org/freedesktop/systemd1/unit/hardware", "nab-hardware.service", make([]byte, 16), nil
}

type fixtureUnit struct{}

func (fixtureUnit) Get(iface, name string) (dbus.Variant, *dbus.Error) {
	if iface == "org.freedesktop.systemd1.Unit" && name == "Id" {
		return dbus.MakeVariant("nab-hardware.service"), nil
	}
	return dbus.Variant{}, dbus.MakeFailedError(errors.New("unknown property"))
}
func (f *busFixture) Claim(sender dbus.Sender) *dbus.Error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.owner != "" && f.owner != string(sender) {
		return dbus.MakeFailedError(errors.New("claimed"))
	}
	f.owner = string(sender)
	return nil
}
func (f *busFixture) Release(sender dbus.Sender) *dbus.Error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.owner != string(sender) {
		return dbus.MakeFailedError(errors.New("not owner"))
	}
	f.owner = ""
	return nil
}
func (f *busFixture) SetLeds(sender dbus.Sender, colors []hardware.Color) *dbus.Error {
	f.mu.Lock()
	if f.owner != string(sender) {
		f.mu.Unlock()
		return dbus.MakeFailedError(errors.New("not owner"))
	}
	f.frames = append(f.frames, append([]hardware.Color(nil), colors...))
	f.frameTimes = append(f.frameTimes, time.Now())
	delay := f.ledDelay
	f.mu.Unlock()
	time.Sleep(delay)
	return nil
}
func (f *busFixture) PulseLed(sender dbus.Sender, index, r, g, b uint8) *dbus.Error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.owner != string(sender) {
		return dbus.MakeFailedError(errors.New("not owner"))
	}
	f.pulses++
	return nil
}
func (f *busFixture) MoveEar(sender dbus.Sender, index, pos uint8, backward bool) *dbus.Error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.owner != string(sender) {
		return dbus.MakeFailedError(errors.New("not owner"))
	}
	if index == 0 {
		f.status.Left = int16(pos)
	} else {
		f.status.Right = int16(pos)
	}
	return nil
}
func (f *busFixture) StepEar(sender dbus.Sender, index, steps uint8, backward bool) *dbus.Error {
	return f.MoveEar(sender, index, steps, backward)
}
func (f *busFixture) ReadEars(detect bool) (int16, int16, *dbus.Error) {
	f.mu.Lock()
	delay, entered := f.readDelay, f.readEntered
	f.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	time.Sleep(delay)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status.Left, f.status.Right, nil
}

func (f *busFixture) WaitEarsIdle() *dbus.Error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.idleError {
		return dbus.MakeFailedError(errors.New("physical idle unconfirmed"))
	}
	return nil
}
func (f *busFixture) Start(sender dbus.Sender, kind, src string) (string, *dbus.Error) {
	f.mu.Lock()
	id := fmt.Sprintf("audio-%d", len(f.starts)+1)
	f.starts = append(f.starts, src)
	f.startOwners = append(f.startOwners, string(sender))
	delay, entered, release, lose := f.startDelay, f.startEntered, f.startRelease, f.loseReply
	f.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if release != nil {
		<-release
	}
	time.Sleep(delay)
	if lose {
		time.Sleep(device.Timeout + 200*time.Millisecond)
	}
	return id, nil
}
func (f *busFixture) Stop(sender dbus.Sender, id string) *dbus.Error {
	f.mu.Lock()
	f.stops = append(f.stops, id)
	f.stopOwners = append(f.stopOwners, string(sender))
	delay := f.stopDelay
	f.mu.Unlock()
	time.Sleep(delay)
	return nil
}
func (f *busFixture) Wait(sender dbus.Sender, id string) (string, *dbus.Error) {
	f.mu.Lock()
	f.waits = append(f.waits, id)
	f.waitOwners = append(f.waitOwners, string(sender))
	delay, release := f.playDelay, f.waitRelease
	f.mu.Unlock()
	if release != nil {
		<-release
	}
	time.Sleep(delay)
	return "completed", nil
}
func (f *busFixture) StartWrite(sender dbus.Sender, tech string, uid []byte, picture, app uint8, data []byte, timeout uint32) (uint64, *dbus.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.owner != string(sender) {
		return 0, dbus.MakeFailedError(errors.New("not owner"))
	}
	f.tagWrites = append(f.tagWrites, TagWrite{tech, append([]byte(nil), uid...), picture, app, append([]byte(nil), data...), timeout})
	f.writeOwners = append(f.writeOwners, string(sender))
	return 123, nil
}
func (f *busFixture) WaitWrite(sender dbus.Sender, id uint64) (string, *dbus.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.owner != string(sender) {
		return "", dbus.MakeFailedError(errors.New("not owner"))
	}
	f.writeWaitOwners = append(f.writeWaitOwners, string(sender))
	if f.writeOutcome != "" {
		return f.writeOutcome, nil
	}
	return "completed", nil
}
func (f *busFixture) CancelWrite(sender dbus.Sender, id uint64) *dbus.Error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.owner != string(sender) {
		return dbus.MakeFailedError(errors.New("not owner"))
	}
	return nil
}

func eventually(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not converge")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func readyEngine(t *testing.T, f *busFixture, handlers Handlers) *Engine {
	t.Helper()
	e := New(Options{Version: "test"}, handlers)
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ctx, cancel := cleanupContext(); defer cancel(); e.Stop(ctx) })
	eventually(t, func() bool { _, ok := e.hw.Snapshot(); return ok })
	if e.Ready() {
		t.Fatal("maintenance must start blocked")
	}
	if err := e.SetMaintenance(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	eventually(t, e.Ready)
	return e
}
