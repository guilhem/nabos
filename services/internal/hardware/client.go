// Package hardware owns a dedicated, authenticated connection to nab-hardware.
package hardware

import (
	"context"
	"errors"
	"log"
	"os"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/guilhem/nabos/services/internal/busidentity"
)

const Destination = "io.github.guilhem.NabHardware1"
const Path = dbus.ObjectPath("/io/github/guilhem/NabHardware1")
const Timeout = 5 * time.Second

var ErrUnavailable = errors.New("hardware unavailable")
var ErrStale = busidentity.ErrStale

type Status struct {
	Model     string
	Simulated bool
	LeftEar   string
	RightEar  string
	Leds      bool
	Button    bool
	RFID      string
	Left      int16
	Right     int16
}

func (s Status) Ready() bool { return s.Leds && s.Button && s.LeftEar == "ok" && s.RightEar == "ok" }

type Tag struct {
	Removed   bool
	Tech      string
	UID       []byte
	Support   string
	Locked    bool
	Formatted bool
	Picture   uint8
	App       uint8
	Data      []byte
}
type Color struct{ Index, Red, Green, Blue uint8 }
type Event struct {
	Kind            string
	Status          Status
	Button          string
	EdgeMonotonicNS uint64
	Ear             uint8
	Tag             *Tag
	Generation      uint64
}
type Client struct {
	mu         sync.RWMutex
	conn       *dbus.Conn
	owner      string
	generation uint64
	status     Status
	claimed    bool
	callback   func(Event)
	cancel     context.CancelFunc
	done       chan struct{}
}

func New(callback func(Event)) *Client { return &Client{callback: callback} }
func Connect() (*dbus.Conn, error) {
	if addr := os.Getenv("NABOS_DEVICE_BUS_ADDRESS"); addr != "" {
		return dbus.Connect(addr)
	}
	return dbus.ConnectSystemBus()
}
func (c *Client) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done != nil {
		return errors.New("hardware already started")
	}
	ctx, c.cancel = context.WithCancel(ctx)
	c.done = make(chan struct{})
	go c.run(ctx)
	return nil
}
func (c *Client) Snapshot() (Status, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.status, c.claimed && c.conn != nil && c.conn.Connected()
}
func (c *Client) Generation() uint64 { c.mu.RLock(); defer c.mu.RUnlock(); return c.generation }
func (c *Client) SameGeneration(e Event) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return e.Generation == c.generation
}
func (c *Client) Current(e Event) bool {
	c.mu.RLock()
	conn, owner, generation, claimed := c.conn, c.owner, c.generation, c.claimed
	c.mu.RUnlock()
	if !claimed || e.Generation != generation || conn == nil || !conn.Connected() {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	current, err := ownerOf(ctx, conn)
	return err == nil && current == owner
}

func (c *Client) emit(e Event) {
	if c.callback != nil {
		c.callback(e)
	}
}
func (c *Client) invalidate(conn *dbus.Conn) { c.invalidateAt(conn, 0) }
func (c *Client) invalidateAt(conn *dbus.Conn, generation uint64) bool {
	c.mu.Lock()
	if c.conn != conn || (generation != 0 && c.generation != generation) {
		c.mu.Unlock()
		return false
	}
	c.claimed = false
	c.owner = ""
	c.generation++
	c.status = Status{Left: -1, Right: -1}
	e := Event{Kind: "offline", Status: c.status, Generation: c.generation}
	c.mu.Unlock()
	c.emit(e)
	return true
}

func (c *Client) run(ctx context.Context) {
	defer close(c.done)
	for ctx.Err() == nil {
		conn, err := Connect()
		if err != nil {
			if !pause(ctx, time.Second) {
				return
			}
			continue
		}
		c.mu.Lock()
		c.conn = conn
		c.mu.Unlock()
		signals := make(chan *dbus.Signal, 128)
		conn.Signal(signals)
		err = conn.AddMatchSignal(dbus.WithMatchInterface("org.freedesktop.DBus"), dbus.WithMatchMember("NameOwnerChanged"), dbus.WithMatchArg(0, Destination))
		if err == nil {
			err = conn.AddMatchSignal(dbus.WithMatchObjectPath(Path), dbus.WithMatchInterface(Destination))
		}
		if err == nil {
			c.monitor(ctx, conn, signals)
		}
		c.invalidate(conn)
		conn.RemoveSignal(signals)
		conn.Close()
		c.mu.Lock()
		if c.conn == conn {
			c.conn = nil
		}
		c.mu.Unlock()
		if !pause(ctx, time.Second) {
			return
		}
	}
}
func pause(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
func ownerOf(ctx context.Context, conn *dbus.Conn) (string, error) {
	var owner string
	err := conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, Destination).Store(&owner)
	return owner, err
}
func authenticate(ctx context.Context, conn *dbus.Conn, owner string) error {
	return busidentity.Authenticate(ctx, conn, Destination, owner, busidentity.ExpectedUser("NABOS_HARDWARE_USER", "nab-hardware"))
}
func (c *Client) recover(ctx context.Context, conn *dbus.Conn) error {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	owner, err := ownerOf(ctx, conn)
	if err != nil {
		return err
	}
	if err = authenticate(ctx, conn, owner); err != nil {
		return err
	}
	if err = conn.Object(owner, Path).CallWithContext(ctx, Destination+".Claim", 0).Store(); err != nil {
		return err
	}
	var v dbus.Variant
	var s Status
	if err = conn.Object(owner, Path).CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, Destination, "Status").Store(&v); err != nil {
		return err
	}
	if err = v.Store(&s); err != nil {
		return err
	}
	current, err := ownerOf(ctx, conn)
	if err != nil {
		return err
	}
	if current != owner {
		return ErrStale
	}
	c.mu.Lock()
	c.owner = owner
	c.generation++
	c.status = s
	c.claimed = true
	e := Event{Kind: "online", Status: s, Generation: c.generation}
	c.mu.Unlock()
	c.emit(e)
	return nil
}
func (c *Client) monitor(ctx context.Context, conn *dbus.Conn, signals <-chan *dbus.Signal) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	if err := c.recover(ctx, conn); err != nil {
		log.Printf("hardware recovery: %v", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-conn.Context().Done():
			return
		case <-ticker.C:
			_, ok := c.Snapshot()
			if !ok {
				if err := c.recover(ctx, conn); err != nil {
					log.Printf("hardware recovery: %v", err)
				}
			}
		case sig, ok := <-signals:
			if !ok {
				return
			}
			if sig == nil {
				continue
			}
			if sig.Name == "org.freedesktop.DBus.NameOwnerChanged" && sig.Sender == "org.freedesktop.DBus" && len(sig.Body) == 3 && sig.Body[0] == Destination {
				c.mu.RLock()
				owner := c.owner
				c.mu.RUnlock()
				if owner != "" && (sig.Body[2] == owner || sig.Body[1] != owner) {
					continue
				}
				c.invalidate(conn)
				if err := c.recover(ctx, conn); err != nil {
					log.Printf("hardware recovery: %v", err)
				}
				continue
			}
			c.mu.RLock()
			owner, generation, claimed := c.owner, c.generation, c.claimed
			c.mu.RUnlock()
			if !claimed || sig.Sender != owner || sig.Path != Path {
				continue
			}
			// Check the bus owner at dispatch, so a queued old signal cannot cross recovery.
			check, cancel := context.WithTimeout(ctx, Timeout)
			current, err := ownerOf(check, conn)
			cancel()
			if err != nil || current != owner {
				c.invalidateAt(conn, generation)
				continue
			}
			e := Event{Generation: generation}
			switch sig.Name {
			case Destination + ".Changed":
				e.Kind = "changed"
				err = dbus.Store(sig.Body, &e.Status)
			case Destination + ".Button":
				e.Kind = "button"
				err = dbus.Store(sig.Body, &e.Button, &e.EdgeMonotonicNS)
			case Destination + ".EarMoved":
				e.Kind = "ear_moved"
				err = dbus.Store(sig.Body, &e.Ear)
				if e.Ear > 1 {
					continue
				}
			case Destination + ".Tag":
				e.Kind = "rfid"
				e.Tag = &Tag{}
				t := e.Tag
				err = dbus.Store(sig.Body, &t.Removed, &t.Tech, &t.UID, &t.Support, &t.Locked, &t.Formatted, &t.Picture, &t.App, &t.Data)
			default:
				continue
			}
			if err != nil {
				continue
			}
			if e.Kind == "changed" {
				c.mu.Lock()
				c.status = e.Status
				c.mu.Unlock()
			}
			c.emit(e)
		}
	}
}

type generationKey struct{}

// Bind prevents an old worker from mutating a recovered owner's hardware.
func (c *Client) Bind(ctx context.Context) context.Context {
	c.mu.RLock()
	g := c.generation
	c.mu.RUnlock()
	return context.WithValue(ctx, generationKey{}, g)
}
func (c *Client) call(ctx context.Context, method string, args ...any) (*dbus.Call, error) {
	c.mu.RLock()
	conn, owner, generation, claimed := c.conn, c.owner, c.generation, c.claimed
	c.mu.RUnlock()
	if g, ok := ctx.Value(generationKey{}).(uint64); ok && g != generation {
		return nil, ErrStale
	}
	if !claimed || conn == nil || !conn.Connected() {
		return nil, ErrUnavailable
	}
	limit := Timeout
	if method == "WaitWrite" {
		limit = 65 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	current, err := ownerOf(ctx, conn)
	if err != nil {
		return nil, err
	}
	if current != owner {
		c.invalidateAt(conn, generation)
		return nil, ErrStale
	}
	call := conn.Object(owner, Path).CallWithContext(ctx, Destination+"."+method, 0, args...)
	if call.Err != nil {
		return nil, call.Err
	}
	c.mu.RLock()
	valid := c.generation == generation && c.claimed
	c.mu.RUnlock()
	current, err = ownerOf(ctx, conn)
	if err != nil {
		return nil, err
	}
	if !valid || current != owner {
		if valid {
			c.invalidateAt(conn, generation)
		}
		return nil, ErrStale
	}
	return call, nil
}
func (c *Client) SetLeds(ctx context.Context, colors []Color) error {
	if len(colors) > 5 {
		return errors.New("too many LEDs")
	}
	var seen [5]bool
	for _, v := range colors {
		if v.Index > 4 || seen[v.Index] {
			return errors.New("invalid LED index")
		}
		seen[v.Index] = true
	}
	_, err := c.call(ctx, "SetLeds", colors)
	return err
}
func (c *Client) PulseLed(ctx context.Context, index, r, g, b uint8) error {
	if index > 4 {
		return errors.New("invalid LED index")
	}
	_, err := c.call(ctx, "PulseLed", index, r, g, b)
	return err
}
func (c *Client) MoveEar(ctx context.Context, index, position uint8, backward bool) error {
	if index > 1 {
		return errors.New("invalid ear index")
	}
	_, err := c.call(ctx, "MoveEar", index, position, backward)
	return err
}
func (c *Client) StepEar(ctx context.Context, index, steps uint8, backward bool) error {
	if index > 1 {
		return errors.New("invalid ear index")
	}
	_, err := c.call(ctx, "StepEar", index, steps, backward)
	return err
}
func (c *Client) ReadEars(ctx context.Context, detect bool) (int16, int16, error) {
	call, err := c.call(ctx, "ReadEars", detect)
	var l, r int16
	if err == nil {
		err = call.Store(&l, &r)
	}
	return l, r, err
}
func (c *Client) WaitEarsIdle(ctx context.Context) error {
	_, err := c.call(ctx, "WaitEarsIdle")
	return err
}
func (c *Client) StartWrite(ctx context.Context, tech string, uid []byte, picture, app uint8, data []byte, timeout uint32) (uint64, error) {
	if !ValidTag(tech, uid, data, timeout) {
		return 0, errors.New("invalid tag write")
	}
	call, err := c.call(ctx, "StartWrite", tech, uid, picture, app, data, timeout)
	var id uint64
	if err == nil {
		err = call.Store(&id)
	}
	if err != nil {
		c.DropContext(ctx)
	}
	return id, err
}
func ValidTag(tech string, uid, data []byte, timeout uint32) bool {
	return ((tech == "st25tb" && len(uid) == 8) || (tech == "iso14443a_t2t" && (len(uid) == 4 || len(uid) == 7 || len(uid) == 10))) && len(data) <= 32 && timeout >= 1 && timeout <= 60
}
func (c *Client) WaitWrite(ctx context.Context, id uint64) (string, error) {
	call, err := c.call(ctx, "WaitWrite", id)
	var outcome string
	if err == nil {
		err = call.Store(&outcome)
	}
	return outcome, err
}
func (c *Client) CancelWrite(ctx context.Context, id uint64) error {
	_, err := c.call(ctx, "CancelWrite", id)
	return err
}

// Drop makes unknown admitted operations lose their owner; never replay them.
func (c *Client) Drop() { c.DropContext(context.Background()) }

// DropContext cannot revoke a connection that belongs to a newer activity generation.
func (c *Client) DropContext(ctx context.Context) {
	c.mu.RLock()
	conn, generation := c.conn, c.generation
	c.mu.RUnlock()
	if expected, ok := ctx.Value(generationKey{}).(uint64); ok && expected != generation {
		return
	}
	if conn != nil && c.invalidateAt(conn, generation) {
		conn.Close()
	}
}

func (c *Client) Stop(ctx context.Context) error {
	c.mu.RLock()
	cancel, done := c.cancel, c.done
	c.mu.RUnlock()
	if cancel == nil {
		return nil
	}
	_, err := c.call(ctx, "Release")
	cancel()
	c.Drop()
	select {
	case <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
