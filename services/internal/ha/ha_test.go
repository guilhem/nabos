package ha

import (
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/packets"
	"github.com/guilhem/nabos/services/internal/config"
)

func TestDiscoveryWaitsForCommandSubscriptions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason byte
	}{
		{"immediate non-retained command", 0},
		{"partially rejected subscriptions", 0x87},
	} {
		t.Run(tc.name, func(t *testing.T) {
			commands := make(chan Command, 1)
			b := &Bridge{Node: "nabos_test", OnCommand: func(c Command) { commands <- c }}
			advertised, subscribed := discoveryBroker(t, b, tc.reason)
			if tc.reason == 0 {
				select {
				case <-advertised:
				case <-time.After(time.Second):
					t.Fatal("discovery was not advertised")
				}
				select {
				case c := <-commands:
					if c != (Command{Name: "sleep", Value: "ON"}) {
						t.Fatalf("unexpected command: %+v", c)
					}
				case <-time.After(time.Second):
					t.Fatal("non-retained sleep command sent at first discovery did not reach OnCommand")
				}
				return
			}
			select {
			case <-subscribed:
			case <-time.After(time.Second):
				t.Fatal("subscription was not attempted")
			}
			select {
			case topic := <-advertised:
				t.Fatalf("advertised %s despite failed command subscriptions", topic)
			case <-time.After(200 * time.Millisecond):
			}
		})
	}
}

// Model a consumer acting at the first discovery: a non-retained command is
// forwarded only if its subscription already exists, and is otherwise lost.
func discoveryBroker(t *testing.T, b *Bridge, reason byte) (<-chan string, <-chan struct{}) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	advertised := make(chan string, 32)
	subscribed, done := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var client net.Conn
	go func() {
		defer close(done)
		c, err := listener.Accept()
		if err != nil {
			return
		}
		mu.Lock()
		client = c
		mu.Unlock()
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		ready, firstDiscovery := false, true
		for {
			p, err := packets.ReadPacket(c)
			if err != nil {
				return
			}
			var reply *packets.ControlPacket
			switch p.Type {
			case packets.CONNECT:
				reply = packets.NewControlPacket(packets.CONNACK)
			case packets.SUBSCRIBE:
				sub := p.Content.(*packets.Subscribe)
				close(subscribed)
				reply = packets.NewControlPacket(packets.SUBACK)
				ack := reply.Content.(*packets.Suback)
				ack.PacketID = sub.PacketID
				ack.Reasons = make([]byte, len(sub.Subscriptions))
				var sets, presses bool
				for _, s := range sub.Subscriptions {
					sets = sets || s.Topic == b.topic("+/set")
					presses = presses || s.Topic == b.topic("+/press")
				}
				ack.Reasons[len(ack.Reasons)-1] = reason
				ready = reason == 0 && sets && presses
			case packets.PUBLISH:
				pub := p.Content.(*packets.Publish)
				if strings.HasPrefix(pub.Topic, "homeassistant/") {
					advertised <- pub.Topic
					if firstDiscovery {
						firstDiscovery = false
						if ready {
							command := packets.NewControlPacket(packets.PUBLISH)
							command.Content = &packets.Publish{Topic: b.topic("sleep/set"), Payload: []byte("ON")}
							if _, err := command.WriteTo(c); err != nil {
								return
							}
						}
					}
				}
				if pub.Topic == b.topic("availability") && string(pub.Payload) == "online" {
					advertised <- pub.Topic
				}
				if pub.QoS == 1 {
					reply = packets.NewControlPacket(packets.PUBACK)
					reply.Content.(*packets.Puback).PacketID = pub.PacketID
				}
			case packets.PINGREQ:
				reply = packets.NewControlPacket(packets.PINGRESP)
			case packets.DISCONNECT:
				return
			}
			if reply != nil {
				if _, err := reply.WriteTo(c); err != nil {
					return
				}
			}
		}
	}()
	t.Cleanup(func() {
		b.Stop()
		listener.Close()
		mu.Lock()
		if client != nil {
			client.Close()
		}
		mu.Unlock()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("MQTT fixture did not stop")
		}
	})
	if err := b.Start(config.HomeAssistant{Enabled: true, Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, Prefix: "homeassistant"}); err != nil {
		t.Fatal(err)
	}
	return advertised, subscribed
}

func TestCommandsAreValidated(t *testing.T) {
	b := &Bridge{Node: "nabos_test"}
	for topic, payload := range map[string]string{
		"nabos/nabos_test/sleep/set":     "ON",
		"nabos/nabos_test/volume/set":    "55",
		"nabos/nabos_test/left_ear/set":  "16",
		"nabos/nabos_test/weather/press": "PRESS",
	} {
		if _, ok := b.ParseCommand(topic, []byte(payload)); !ok {
			t.Fatalf("%s %s refused", topic, payload)
		}
	}
	for topic, payload := range map[string]string{
		"nabos/nabos_test/sleep/set":     "maybe",
		"nabos/nabos_test/volume/set":    "250",
		"nabos/nabos_test/right_ear/set": "-1",
		"nabos/other/volume/set":         "10",
		"nabos/nabos_test/reboot/press":  "",
	} {
		if _, ok := b.ParseCommand(topic, []byte(payload)); ok {
			t.Fatalf("%s %s accepted", topic, payload)
		}
	}
	if len(b.Discovery("homeassistant")) != 17 {
		t.Fatal("discovery entities")
	}
	for _, name := range []string{"taichi", "surprise", "eightball", "airquality", "carrot", "birthday", "autopromo", "weather_tomorrow"} {
		if _, ok := b.ParseCommand("nabos/nabos_test/"+name+"/press", []byte("PRESS")); !ok {
			t.Fatal("voice action refused", name)
		}
	}
}
