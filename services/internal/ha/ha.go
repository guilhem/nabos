// Package ha exposes the rabbit to Home Assistant through MQTT discovery on
// the Home Assistant broker (usually the Mosquitto add-on).
package ha

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"
	"github.com/guilhem/nabos/services/internal/config"
)

// Command received from Home Assistant: sleep (ON/OFF), chime, weather,
// volume (0-100), left_ear / right_ear (0-16).
type Command struct {
	Name, Value string
}

type Bridge struct {
	Node, Model, Version string
	OnCommand            func(Command)

	mu     sync.Mutex
	cm     *autopaho.ConnectionManager
	cancel context.CancelFunc
	prefix string
	last   map[string][]byte
}

func (b *Bridge) topic(s string) string { return "nabos/" + b.Node + "/" + s }

// Discovery returns the retained discovery messages (topic -> payload).
func (b *Bridge) Discovery(prefix string) map[string]any {
	device := map[string]any{
		"identifiers": []string{b.Node}, "name": "Nabaztag", "manufacturer": "Violet / TagTagTag",
		"model": b.Model, "sw_version": b.Version,
	}
	base := func(name, obj string) map[string]any {
		return map[string]any{
			"name": name, "unique_id": b.Node + "_" + obj, "object_id": b.Node + "_" + obj,
			"availability_topic": b.topic("availability"), "device": device,
		}
	}
	with := func(m map[string]any, kv ...any) map[string]any {
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	d := func(component, obj string) string {
		return fmt.Sprintf("%s/%s/%s/%s/config", prefix, component, b.Node, obj)
	}
	discovery := map[string]any{
		d("sensor", "state"): with(base("État", "state"), "state_topic", b.topic("state"), "value_template", "{{ value_json.state }}", "icon", "mdi:rabbit"),
		d("switch", "sleep"): with(base("Sommeil", "sleep"), "state_topic", b.topic("state"), "value_template", "{{ 'ON' if value_json.state == 'asleep' else 'OFF' }}",
			"command_topic", b.topic("sleep/set"), "icon", "mdi:sleep"),
		d("button", "chime"):   with(base("Donner l'heure", "chime"), "command_topic", b.topic("chime/press"), "icon", "mdi:clock-outline"),
		d("button", "weather"): with(base("Météo", "weather"), "command_topic", b.topic("weather/press"), "icon", "mdi:weather-partly-cloudy"),
		d("number", "volume"): with(base("Volume", "volume"), "state_topic", b.topic("state"), "value_template", "{{ value_json.volume }}",
			"command_topic", b.topic("volume/set"), "min", 0, "max", 100, "step", 5, "icon", "mdi:volume-high"),
		d("number", "left_ear"): with(base("Oreille gauche", "left_ear"), "state_topic", b.topic("state"), "value_template", "{{ value_json.left_ear }}",
			"command_topic", b.topic("left_ear/set"), "min", 0, "max", 16, "mode", "slider"),
		d("number", "right_ear"): with(base("Oreille droite", "right_ear"), "state_topic", b.topic("state"), "value_template", "{{ value_json.right_ear }}",
			"command_topic", b.topic("right_ear/set"), "min", 0, "max", 16, "mode", "slider"),
		d("event", "button"): with(base("Bouton", "button"), "state_topic", b.topic("button"),
			"event_types", []string{"click", "double_click", "triple_click", "hold", "click_and_hold", "double_click_and_hold"}),
		d("sensor", "tag"): with(base("Dernière étiquette", "tag"), "state_topic", b.topic("rfid"), "value_template", "{{ value_json.uid }}",
			"json_attributes_topic", b.topic("rfid"), "icon", "mdi:nfc"),
	}
	for name, label := range map[string]string{"taichi": "Tai-chi", "surprise": "Surprise", "eightball": "Boule magique", "airquality": "Qualité de l'air", "carrot": "Carotte", "birthday": "Anniversaire", "autopromo": "Autopromotion", "weather_tomorrow": "Météo de demain"} {
		discovery[d("button", name)] = with(base(label, name), "command_topic", b.topic(name+"/press"))
	}
	return discovery
}

// ParseCommand validates a message received on nabos/<node>/<x>/(set|press).
func (b *Bridge) ParseCommand(topic string, payload []byte) (Command, bool) {
	rest, ok := strings.CutPrefix(topic, b.topic(""))
	if !ok {
		return Command{}, false
	}
	v := strings.TrimSpace(string(payload))
	switch rest {
	case "sleep/set":
		return Command{"sleep", v}, v == "ON" || v == "OFF"
	case "chime/press", "weather/press", "weather_tomorrow/press", "taichi/press", "surprise/press", "eightball/press", "airquality/press", "carrot/press", "birthday/press", "autopromo/press":
		return Command{strings.TrimSuffix(rest, "/press"), ""}, true
	case "volume/set", "left_ear/set", "right_ear/set":
		var n float64
		max := 16.0
		if rest == "volume/set" {
			max = 100
		}
		if json.Unmarshal(payload, &n) != nil || n < 0 || n > max {
			return Command{}, false
		}
		return Command{strings.TrimSuffix(rest, "/set"), fmt.Sprint(int(n))}, true
	}
	return Command{}, false
}

// Start (re)connects to the Home Assistant broker described by cfg.
func (b *Bridge) Start(cfg config.HomeAssistant) error {
	b.Stop()
	if !cfg.Enabled {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	u := &url.URL{Scheme: "mqtt", Host: fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)}
	prefix := cfg.Prefix
	cc := autopaho.ClientConfig{
		ServerUrls:                    []*url.URL{u},
		KeepAlive:                     30,
		CleanStartOnInitialConnection: true,
		ConnectRetryDelay:             10 * time.Second,
		ConnectUsername:               cfg.Username,
		ConnectPassword:               []byte(cfg.Password),
		WillMessage:                   &paho.WillMessage{Topic: b.topic("availability"), Payload: []byte("offline"), QoS: 1, Retain: true},
		OnConnectionUp: func(cm *autopaho.ConnectionManager, _ *paho.Connack) {
			go b.announce(ctx, cm, prefix)
		},
		OnConnectError: func(err error) { slog.Warn("Home Assistant broker", "err", err) },
		ClientConfig: paho.ClientConfig{
			ClientID: b.Node,
			OnPublishReceived: []func(paho.PublishReceived) (bool, error){func(pr paho.PublishReceived) (bool, error) {
				if c, ok := b.ParseCommand(pr.Packet.Topic, pr.Packet.Payload); ok && b.OnCommand != nil {
					b.OnCommand(c)
				}
				return true, nil
			}},
		},
	}
	cm, err := autopaho.NewConnection(ctx, cc)
	if err != nil {
		cancel()
		return err
	}
	b.mu.Lock()
	b.cm, b.cancel, b.prefix = cm, cancel, prefix
	b.mu.Unlock()
	return nil
}

func (b *Bridge) announce(ctx context.Context, cm *autopaho.ConnectionManager, prefix string) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for topic, cfg := range b.Discovery(prefix) {
		raw, _ := json.Marshal(cfg)
		cm.Publish(ctx, &paho.Publish{Topic: topic, QoS: 1, Retain: true, Payload: raw})
	}
	cm.Subscribe(ctx, &paho.Subscribe{Subscriptions: []paho.SubscribeOptions{{Topic: b.topic("+/set"), QoS: 1}, {Topic: b.topic("+/press"), QoS: 1}}})
	cm.Publish(ctx, &paho.Publish{Topic: b.topic("availability"), QoS: 1, Retain: true, Payload: []byte("online")})
	b.mu.Lock()
	last := b.last
	b.mu.Unlock()
	if raw, ok := last["state"]; ok {
		cm.Publish(ctx, &paho.Publish{Topic: b.topic("state"), QoS: 1, Retain: true, Payload: raw})
	}
}

func (b *Bridge) publish(sub string, retain bool, v any) {
	raw, _ := json.Marshal(v)
	b.mu.Lock()
	if b.last == nil {
		b.last = map[string][]byte{}
	}
	b.last[sub] = raw
	cm := b.cm
	b.mu.Unlock()
	if cm == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cm.Publish(ctx, &paho.Publish{Topic: b.topic(sub), QoS: 1, Retain: retain, Payload: raw})
}

func (b *Bridge) State(state string, volume, left, right int) {
	b.publish("state", true, map[string]any{"state": state, "volume": volume, "left_ear": left, "right_ear": right})
}

func (b *Bridge) Button(event string) {
	b.publish("button", false, map[string]string{"event_type": event})
}

func (b *Bridge) Tag(tag map[string]any) { b.publish("rfid", true, tag) }

func (b *Bridge) Connected() bool {
	b.mu.Lock()
	cm := b.cm
	b.mu.Unlock()
	if cm == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	return cm.AwaitConnection(ctx) == nil
}

func (b *Bridge) Stop() {
	b.mu.Lock()
	cm, cancel := b.cm, b.cancel
	b.cm, b.cancel = nil, nil
	b.mu.Unlock()
	if cm != nil {
		ctx, c := context.WithTimeout(context.Background(), 2*time.Second)
		cm.Publish(ctx, &paho.Publish{Topic: b.topic("availability"), QoS: 1, Retain: true, Payload: []byte("offline")})
		cm.Disconnect(ctx)
		c()
		cancel()
	}
}
