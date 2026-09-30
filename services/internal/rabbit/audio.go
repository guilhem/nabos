package rabbit

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/guilhem/nabos/services/internal/device"
	"github.com/guilhem/nabos/services/internal/hardware"
)

type source struct{ kind, value string }
type audio struct {
	mu      sync.Mutex
	client  *device.Client
	current string
	loss    error
}

func (a *audio) connect() error {
	conn, err := hardware.Connect()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), device.Timeout)
	defer cancel()
	signals := make(chan *dbus.Signal, 16)
	conn.Signal(signals)
	if err = conn.AddMatchSignalContext(ctx, dbus.WithMatchInterface("org.freedesktop.DBus"), dbus.WithMatchMember("NameOwnerChanged"), dbus.WithMatchArg(0, device.Destination)); err != nil {
		conn.Close()
		return err
	}
	var owner string
	if err = conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, device.Destination).Store(&owner); err != nil {
		conn.Close()
		return err
	}
	client := &device.Client{Conn: conn}
	a.client = client
	a.loss = nil
	go func() {
		defer conn.RemoveSignal(signals)
		for {
			select {
			case <-conn.Context().Done():
				return
			case sig := <-signals:
				if sig == nil {
					continue
				}
				if sig.Name != "org.freedesktop.DBus.NameOwnerChanged" || sig.Sender != "org.freedesktop.DBus" || len(sig.Body) != 3 || sig.Body[0] != device.Destination || sig.Body[1] != owner {
					continue
				}
				// Closing does not take the Start mutex: owner loss must interrupt an in-flight reply.
				client.Close()
				a.mu.Lock()
				if a.client == client {
					a.loss = device.ErrUnavailable
					a.client = nil
					a.current = ""
				}
				a.mu.Unlock()
				return
			}
		}
	}()
	return nil
}

// The mutex spans Start's reply and late-ID cleanup; all IDs stay on their owner connection.
func (a *audio) start(ctx context.Context, s source) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if a.loss != nil {
		return "", a.loss
	}
	if a.client == nil {
		if err := a.connect(); err != nil {
			return "", err
		}
	}
	client := a.client
	// Caller cancellation cannot discard an admitted Start's returned ID.
	callCtx, cancel := context.WithTimeout(context.Background(), device.Timeout)
	defer cancel()
	id, err := client.StartAudio(callCtx, s.kind, s.value)
	if err != nil || id == "" {
		client.Close()
		a.client = nil
		a.current = ""
		if err == nil {
			err = errors.New("empty audio ID")
		}
		a.loss = err
		return "", err
	}
	a.current = id
	if ctx.Err() != nil {
		clean, cancel := context.WithTimeout(context.Background(), device.Timeout)
		defer cancel()
		err = client.StopAudio(clean, id)
		if err != nil {
			client.Close()
			a.client = nil
			a.loss = err
		}
		a.current = ""
		if err != nil {
			return "", err
		}
		return "", ctx.Err()
	}
	return id, nil
}
func (a *audio) stopOn(client *device.Client, id string) error {
	if client == nil {
		return device.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), device.Timeout)
	defer cancel()
	err := client.StopAudio(ctx, id)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		client.Close()
		if a.client == client {
			a.client = nil
			a.current = ""
		}
		a.loss = err
	}
	if a.client == client && a.current == id {
		a.current = ""
	}
	return err
}
func (a *audio) stop() error {
	a.mu.Lock()
	id, client, loss := a.current, a.client, a.loss
	a.mu.Unlock()
	if id == "" {
		return loss
	}
	return errors.Join(loss, a.stopOn(client, id))
}
func (a *audio) wait(ctx context.Context, id string) error {
	a.mu.Lock()
	client := a.client
	a.mu.Unlock()
	if client == nil {
		return device.ErrUnavailable
	}
	outcome, err := client.WaitAudio(ctx, id)
	if ctx.Err() != nil {
		if clean := a.stopOn(client, id); clean != nil {
			return clean
		}
		return ctx.Err()
	}
	if err != nil {
		return errors.Join(err, a.stopOn(client, id))
	}
	if outcome != "completed" && outcome != "stopped" && outcome != "preempted" {
		return errors.New("audio playback: " + outcome)
	}
	a.mu.Lock()
	if a.client == client && a.current == id {
		a.current = ""
	}
	a.mu.Unlock()
	return nil
}
func (a *audio) waitCurrent(ctx context.Context) error {
	a.mu.Lock()
	id := a.current
	a.mu.Unlock()
	if id == "" {
		return nil
	}
	return a.wait(ctx, id)
}
func (a *audio) list(ctx context.Context, files []source) error {
	if err := a.stop(); err != nil {
		return err
	}
	for _, s := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		id, err := a.start(ctx, s)
		if err != nil {
			return err
		}
		if err = a.wait(ctx, id); err != nil {
			return err
		}
	}
	return nil
}
func (a *audio) resetLoss() { a.mu.Lock(); a.loss = nil; a.mu.Unlock() }
func (a *audio) close() error {
	err := a.stop()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client != nil {
		a.client.Close()
		a.client = nil
	}
	return err
}
func cleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}
