package main

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/packets"
	"github.com/godbus/dbus/v5"
	"github.com/guilhem/nabos/services/internal/config"
	"github.com/guilhem/nabos/services/internal/device"
	"github.com/guilhem/nabos/services/internal/ha"
)

// Only the optional external HA protocol uses MQTT. The product engine uses D-Bus.
func haBroker(t *testing.T, a *App) func(string) []byte {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	last := map[string][]byte{}
	done := make(chan struct{})
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
		for {
			p, err := packets.ReadPacket(c)
			if err != nil {
				return
			}
			var reply *packets.ControlPacket
			switch p.Type {
			case packets.CONNECT:
				reply = packets.NewControlPacket(packets.CONNACK)
			case packets.PUBLISH:
				pub := p.Content.(*packets.Publish)
				if pub.Topic == "nabos/"+a.ha.Node+"/state" && !pub.Retain {
					t.Error("HA state publication lost retain")
				}
				mu.Lock()
				last[pub.Topic] = append([]byte(nil), pub.Payload...)
				mu.Unlock()
				if pub.QoS == 1 {
					reply = packets.NewControlPacket(packets.PUBACK)
					reply.Content.(*packets.Puback).PacketID = pub.PacketID
				}
			case packets.SUBSCRIBE:
				sub := p.Content.(*packets.Subscribe)
				reply = packets.NewControlPacket(packets.SUBACK)
				ack := reply.Content.(*packets.Suback)
				ack.PacketID = sub.PacketID
				ack.Reasons = make([]byte, len(sub.Subscriptions))
			case packets.PINGREQ:
				reply = packets.NewControlPacket(packets.PINGRESP)
			case packets.DISCONNECT:
				return
			}
			if reply != nil {
				if _, err = reply.WriteTo(c); err != nil {
					return
				}
			}
		}
	}()
	t.Cleanup(func() {
		a.ha.Stop()
		listener.Close()
		mu.Lock()
		if client != nil {
			client.Close()
		}
		mu.Unlock()
		<-done
	})
	cfg := a.store.Get().HomeAssistant
	cfg.Enabled, cfg.Host, cfg.Port = true, "127.0.0.1", listener.Addr().(*net.TCPAddr).Port
	if err := a.ha.Start(cfg); err != nil {
		t.Fatal(err)
	}
	pynabWait(t, "HA connected", time.Second, a.ha.Connected)
	return func(sub string) []byte {
		mu.Lock()
		defer mu.Unlock()
		return append([]byte(nil), last["nabos/"+a.ha.Node+"/"+sub]...)
	}
}

func TestHAStateUsesLatestRabbitAfterDelayedDeviceRead(t *testing.T) {
	a := testApp(t)
	f := startNative(t, a)
	latest := haBroker(t, a)
	revision, settings, err := a.device.ReadConfig(a.ctx)
	if err != nil {
		t.Fatal(err)
	}
	entered, resume, overlap := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	var blockNext, inFlight atomic.Bool
	blockNext.Store(true)
	read := func() (string, device.Settings, *dbus.Error) {
		if blockNext.CompareAndSwap(true, false) {
			inFlight.Store(true)
			close(entered)
			<-resume
			inFlight.Store(false)
		} else if inFlight.Load() {
			select {
			case overlap <- struct{}{}:
			default:
			}
		}
		return revision, settings, nil
	}
	if err := f.conn.ExportMethodTable(map[string]interface{}{"Read": read}, device.Path("Config"), device.Interface("Config")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(a.ctx)
	done := make(chan struct{})
	go func() { defer close(done); a.deviceLoop(ctx) }()
	released := false
	defer func() {
		if !released {
			close(resume)
		}
		cancel()
		<-done
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("device read did not start")
	}
	a.store.Update(func(s *config.Settings) error { s.Ears = [2]int{4, 5}; return nil })
	if err := a.do(a.ctx, earsCommand(4, 5), time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-overlap:
		t.Fatal("concurrent device reads")
	case <-time.After(30 * time.Millisecond):
	}
	close(resume)
	released = true
	pynabWait(t, "latest retained HA state", time.Second, func() bool {
		var payload struct {
			State  string `json:"state"`
			Volume int    `json:"volume"`
			Left   int    `json:"left_ear"`
			Right  int    `json:"right_ear"`
		}
		return json.Unmarshal(latest("state"), &payload) == nil && payload.State == "idle" && payload.Volume == int(settings.Volume) && payload.Left == 4 && payload.Right == 5
	})
	a.haCommand(ha.Command{Name: "left_ear", Value: "6"})
	pynabWait(t, "HA ear command", time.Second, func() bool { state, _ := a.rabbit.State(); return state.Ears.Left == 6 })
	f.event(t, "Button", "double_click", uint64(0))
	pynabWait(t, "HA button protocol", time.Second, func() bool { return string(latest("button")) == `{"event_type":"double_click"}` })
}
