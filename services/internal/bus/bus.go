// Package bus is the MQTT 5 link between nab-service and nab-core
// (docs/protocol-v1.md).
package bus

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"
)

const (
	TopicCmd          = "nabos/v1/core/cmd"
	TopicResult       = "nabos/v1/core/result"
	TopicState        = "nabos/v1/core/state"
	TopicCoreAvail    = "nabos/v1/core/availability"
	TopicEventPrefix  = "nabos/v1/core/event/"
	TopicSettings     = "nabos/v1/service/settings"
	TopicServiceAvail = "nabos/v1/service/availability"
)

// Result of a core command.
type Result struct {
	V      int             `json:"v"`
	ID     string          `json:"id"`
	Status string          `json:"status"`
	Error  *string         `json:"error"`
	Data   json.RawMessage `json:"data"`
}

func (r Result) Err() error {
	if r.Status == "ok" {
		return nil
	}
	if r.Error != nil {
		return fmt.Errorf("%s: %s", r.Status, *r.Error)
	}
	return errors.New(r.Status)
}

// CoreState is the retained core state.
type CoreState struct {
	V       int     `json:"v"`
	State   string  `json:"state"`
	Playing *string `json:"playing"`
	Ears    struct {
		Left  int `json:"left"`
		Right int `json:"right"`
	} `json:"ears"`
	Hardware map[string]any `json:"hardware"`
	Version  string         `json:"version"`
}

// HardwareReady prevents confirming an update that lost a required driver.
// RFID is optional on original Nabaztag hardware; its reader is probed separately.
func (s CoreState) HardwareReady() bool {
	h := s.Hardware
	return h["simulated"] == true ||
		h["left_ear"] == "ok" && h["right_ear"] == "ok" && h["leds"] == true && h["button"] == true
}

// Handlers are called from the MQTT goroutine and must not block.
type Handlers struct {
	OnState      func(CoreState)
	OnEvent      func(kind string, payload map[string]any)
	OnCoreOnline func() // core (re)started: infos and positions must be sent again
}

var ErrNotConnected = errors.New("MQTT broker not connected")

type Bus struct {
	url      *url.URL
	clientID string
	h        Handlers

	mu         sync.Mutex
	cm         *autopaho.ConnectionManager
	connected  bool
	coreOnline bool
	state      *CoreState
	settings   []byte
	waiters    map[string]chan Result
}

func New(host string, port int, clientID string, h Handlers) *Bus {
	u := &url.URL{Scheme: "mqtt", Host: fmt.Sprintf("%s:%d", host, port)}
	return &Bus{url: u, clientID: clientID, h: h, waiters: map[string]chan Result{}}
}

func (b *Bus) Start(ctx context.Context) error {
	cfg := autopaho.ClientConfig{
		ServerUrls:                    []*url.URL{b.url},
		KeepAlive:                     15,
		CleanStartOnInitialConnection: true,
		SessionExpiryInterval:         0,
		ConnectRetryDelay:             time.Second,
		ReconnectBackoff:              func(int) time.Duration { return time.Second },
		ConnectTimeout:                5 * time.Second,
		WillMessage:                   &paho.WillMessage{Topic: TopicServiceAvail, Payload: []byte("offline"), QoS: 1, Retain: true},
		OnConnectionUp:                b.up,
		OnConnectionDown: func() bool {
			b.mu.Lock()
			b.connected = false
			// Unknown until the broker tells again: the core may restart meanwhile.
			b.coreOnline = false
			b.mu.Unlock()
			slog.Warn("MQTT connection lost")
			return true
		},
		OnConnectError: func(err error) { slog.Debug("MQTT connect failed", "err", err) },
		ClientConfig: paho.ClientConfig{
			ClientID:          b.clientID,
			OnPublishReceived: []func(paho.PublishReceived) (bool, error){b.received},
		},
	}
	cm, err := autopaho.NewConnection(ctx, cfg)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.cm = cm
	b.mu.Unlock()
	return nil
}

func (b *Bus) up(cm *autopaho.ConnectionManager, _ *paho.Connack) {
	b.mu.Lock()
	b.connected = true
	settings := b.settings
	b.mu.Unlock()
	slog.Info("connected to MQTT broker")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := cm.Subscribe(ctx, &paho.Subscribe{Subscriptions: []paho.SubscribeOptions{
			{Topic: TopicResult, QoS: 1},
			{Topic: TopicState, QoS: 1},
			{Topic: TopicCoreAvail, QoS: 1},
			{Topic: TopicEventPrefix + "#", QoS: 1},
		}})
		if err != nil {
			slog.Error("MQTT subscribe", "err", err)
		}
		b.publish(ctx, TopicServiceAvail, true, []byte("online"))
		if settings != nil {
			b.publish(ctx, TopicSettings, true, settings)
		}
	}()
}

func (b *Bus) received(pr paho.PublishReceived) (bool, error) {
	p := pr.Packet
	switch {
	case p.Topic == TopicResult:
		var r Result
		if json.Unmarshal(p.Payload, &r) == nil && r.ID != "" {
			b.mu.Lock()
			ch := b.waiters[r.ID]
			delete(b.waiters, r.ID)
			b.mu.Unlock()
			if ch != nil {
				ch <- r
			} else if r.Status != "ok" {
				slog.Info("core command result", "id", r.ID, "status", r.Status, "error", r.Error)
			}
		}
	case p.Topic == TopicState:
		var s CoreState
		if json.Unmarshal(p.Payload, &s) == nil {
			b.mu.Lock()
			b.state = &s
			b.mu.Unlock()
			if b.h.OnState != nil {
				b.h.OnState(s)
			}
		}
	case p.Topic == TopicCoreAvail:
		online := string(p.Payload) == "online"
		b.mu.Lock()
		was := b.coreOnline
		b.coreOnline = online
		b.mu.Unlock()
		if online && !was && b.h.OnCoreOnline != nil {
			b.h.OnCoreOnline()
		}
	case strings.HasPrefix(p.Topic, TopicEventPrefix):
		var m map[string]any
		if json.Unmarshal(p.Payload, &m) == nil && b.h.OnEvent != nil {
			b.h.OnEvent(strings.TrimPrefix(p.Topic, TopicEventPrefix), m)
		}
	}
	return true, nil
}

func (b *Bus) publish(ctx context.Context, topic string, retain bool, payload []byte) error {
	b.mu.Lock()
	cm := b.cm
	b.mu.Unlock()
	if cm == nil {
		return ErrNotConnected
	}
	_, err := cm.Publish(ctx, &paho.Publish{Topic: topic, QoS: 1, Retain: retain, Payload: payload})
	if errors.Is(err, autopaho.ConnectionDownError) {
		return ErrNotConnected
	}
	return err
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "svc-" + hex.EncodeToString(b[:])
}

func (b *Bus) send(ctx context.Context, id, action string, args any, ttl time.Duration) error {
	if args == nil {
		args = map[string]any{}
	}
	payload, err := json.Marshal(map[string]any{
		"v": 1, "id": id, "expires_at": time.Now().Add(ttl).Unix(), "action": action, "args": args,
	})
	if err != nil {
		return err
	}
	// Never retained, never queued while disconnected: a command is either
	// delivered now or dropped (docs/protocol-v1.md).
	return b.publish(ctx, TopicCmd, false, payload)
}

// Send publishes a command without waiting for its result.
func (b *Bus) Send(ctx context.Context, action string, args any, ttl time.Duration) (string, error) {
	id := newID()
	return id, b.send(ctx, id, action, args, ttl)
}

// Do publishes a command and waits for its result until ctx ends.
func (b *Bus) Do(ctx context.Context, action string, args any, ttl time.Duration) (Result, error) {
	id := newID()
	ch := make(chan Result, 1)
	b.mu.Lock()
	b.waiters[id] = ch
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.waiters, id)
		b.mu.Unlock()
	}()
	if err := b.send(ctx, id, action, args, ttl); err != nil {
		return Result{}, err
	}
	select {
	case r := <-ch:
		return r, nil
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

// PublishSettings stores and publishes the retained runtime settings.
func (b *Bus) PublishSettings(ctx context.Context, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	b.mu.Lock()
	changed := string(raw) != string(b.settings)
	b.settings = raw
	b.mu.Unlock()
	if changed {
		_ = b.publish(ctx, TopicSettings, true, raw)
	}
}

// Healthy: broker connected and core online.
func (b *Bus) Healthy() (connected, coreOnline bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.connected, b.coreOnline
}

func (b *Bus) State() (CoreState, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == nil {
		return CoreState{}, false
	}
	return *b.state, b.coreOnline
}

func (b *Bus) Stop(ctx context.Context) {
	_ = b.publish(ctx, TopicServiceAvail, true, []byte("offline"))
	b.mu.Lock()
	cm := b.cm
	b.mu.Unlock()
	if cm != nil {
		_ = cm.Disconnect(ctx)
	}
}
