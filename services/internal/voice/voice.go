// Package voice connects to Linux Voice Assistant's peripheral WebSocket API
// (ws://127.0.0.1:6055, docs/peripheral_api.md of LVA).
package voice

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type Client struct {
	URL     string
	OnEvent func(event string, data map[string]any)

	mu    sync.Mutex
	conn  *websocket.Conn
	state string
}

func (c *Client) State() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// Run keeps a connection open until ctx ends.
func (c *Client) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := c.session(ctx); err != nil && ctx.Err() == nil {
			slog.Debug("voice assistant unreachable", "err", err)
		}
		c.mu.Lock()
		c.conn, c.state = nil, ""
		c.mu.Unlock()
		c.OnEvent("disconnected", nil)
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
}

func (c *Client) session(ctx context.Context) error {
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	conn, _, err := websocket.Dial(dctx, c.URL, nil)
	cancel()
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(64 << 10)
	c.mu.Lock()
	c.conn, c.state = conn, "idle"
	c.mu.Unlock()
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var msg struct {
			Event string         `json:"event"`
			Data  map[string]any `json:"data"`
		}
		if json.Unmarshal(raw, &msg) != nil || msg.Event == "" {
			continue
		}
		c.mu.Lock()
		switch msg.Event {
		case "wake_word_detected", "listening", "thinking", "tts_speaking", "timer_ringing", "idle", "media_player_playing":
			c.state = msg.Event
		case "tts_finished", "pipeline_error":
			c.state = "idle"
		}
		c.mu.Unlock()
		c.OnEvent(msg.Event, msg.Data)
	}
}

// Send a peripheral command such as start_listening or stop_pipeline.
func (c *Client) Send(ctx context.Context, command string) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return errors.New("voice assistant not connected")
	}
	raw, _ := json.Marshal(map[string]string{"command": command})
	return conn.Write(ctx, websocket.MessageText, raw)
}
