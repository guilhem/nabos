// Package device is the typed client of device-core. All domains share one
// permanent D-Bus connection; application code never implements OS policy.
package device

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

const Destination = "io.github.guilhem.DeviceCore1"
const Root = dbus.ObjectPath("/io/github/guilhem/DeviceCore1")
const Timeout = 5 * time.Second

var ErrUnavailable = errors.New("service système indisponible, réessayez")
var ErrRefused = errors.New("opération système refusée")

type Client struct{ Conn *dbus.Conn }

func Open() (*Client, error) {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, ErrUnavailable
	}
	return &Client{Conn: conn}, nil
}
func (c *Client) Close() { c.Conn.Close() }
func Path(domain string) dbus.ObjectPath {
	if domain == "Manager" {
		return Root
	}
	return dbus.ObjectPath(string(Root) + "/" + domain)
}
func Interface(domain string) string { return Destination + "." + domain }

// Call bounds roundtrips and discards remote error bodies, which can contain secrets.
func (c *Client) Call(ctx context.Context, domain, method string, args ...any) (*dbus.Call, error) {
	if !strings.Contains(method, ".") {
		method = Interface(domain) + "." + method
	}
	timeout := Timeout
	if domain == "Updates" && method == Interface(domain)+".Check" {
		timeout = time.Minute // Catalogue HTTP requests can take longer than a local roundtrip.
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	call := c.Conn.Object(Destination, Path(domain)).CallWithContext(ctx, method, 0, args...)
	if call.Err != nil {
		var e dbus.Error
		if errors.As(call.Err, &e) && (strings.HasPrefix(e.Name, Destination+".") || e.Name == "org.freedesktop.DBus.Error.Failed" || e.Name == "org.freedesktop.DBus.Error.InvalidArgs" || e.Name == "org.freedesktop.DBus.Error.AccessDenied" || e.Name == "org.freedesktop.DBus.Error.NotSupported") {
			return nil, ErrRefused
		}
		return nil, ErrUnavailable
	}
	return call, nil
}
func (c *Client) Property(ctx context.Context, domain, name string, dst any) error {
	call, err := c.Call(ctx, domain, "org.freedesktop.DBus.Properties.Get", Interface(domain), name)
	if err != nil {
		return err
	}
	var v dbus.Variant
	if call.Store(&v) != nil || v.Store(dst) != nil {
		return ErrUnavailable
	}
	return nil
}

type HM struct {
	Hour uint32
	Min  uint32
}
type Updates struct {
	Automatic bool
	Channel   string
	Start     HM
	End       HM
}
type Settings struct {
	Locale       string
	Timezone     string
	Volume       uint32
	AutoCheck    bool
	Updates      Updates
	VoiceEnabled bool
}

func (c *Client) ReadConfig(ctx context.Context) (string, Settings, error) {
	call, err := c.Call(ctx, "Config", "Read")
	var revision string
	var settings Settings
	if err == nil && call.Store(&revision, &settings) != nil {
		err = ErrUnavailable
	}
	return revision, settings, err
}
func (c *Client) UpdateConfig(ctx context.Context, revision string, settings Settings) (string, error) {
	call, err := c.Call(ctx, "Config", "Update", revision, settings)
	var next string
	if err == nil && call.Store(&next) != nil {
		err = ErrUnavailable
	}
	return next, err
}
func (c *Client) Reboot(ctx context.Context) error {
	_, err := c.Call(ctx, "System", "Reboot")
	return err
}
func (c *Client) PowerOff(ctx context.Context) error {
	_, err := c.Call(ctx, "System", "PowerOff")
	return err
}
func (c *Client) SetTime(ctx context.Context, t time.Time) error {
	_, err := c.Call(ctx, "System", "SetTime", t.UnixMicro())
	return err
}
func (c *Client) SSHKeys(ctx context.Context) (string, string, error) {
	call, err := c.Call(ctx, "System", "GetSSHKeys")
	var revision, keys string
	if err == nil && call.Store(&revision, &keys) != nil {
		err = ErrUnavailable
	}
	return revision, keys, err
}
func (c *Client) SetSSHKeys(ctx context.Context, revision, keys string) (string, error) {
	call, err := c.Call(ctx, "System", "SetSSHKeys", revision, keys)
	var next string
	if err == nil && call.Store(&next) != nil {
		err = ErrUnavailable
	}
	return next, err
}
func (c *Client) Clock(ctx context.Context) (string, time.Time, error) {
	call, err := c.Call(ctx, "System", "Clock")
	var quality string
	var unix int64
	if err == nil && call.Store(&quality, &unix) != nil {
		err = ErrUnavailable
	}
	return quality, time.Unix(unix, 0), err
}
func (c *Client) Connectivity(ctx context.Context) (string, error) {
	call, err := c.Call(ctx, "System", "Connectivity")
	var state string
	if err == nil && call.Store(&state) != nil {
		err = ErrUnavailable
	}
	return state, err
}
func (c *Client) VoiceSupported(ctx context.Context) (bool, error) {
	var supported bool
	err := c.Property(ctx, "Voice", "Supported", &supported)
	return supported, err
}
func (c *Client) VoiceState(ctx context.Context) (string, error) {
	var state string
	err := c.Property(ctx, "Voice", "Status", &state)
	return state, err
}
func (c *Client) EnableVoice(ctx context.Context, on bool) error {
	_, err := c.Call(ctx, "Voice", "Enable", on)
	return err
}
func (c *Client) VoiceCommand(ctx context.Context, command string) error {
	_, err := c.Call(ctx, "Voice", "Command", command)
	return err
}
func (c *Client) StartAudio(ctx context.Context, kind, source string) (string, error) {
	call, err := c.Call(ctx, "Audio", "Start", kind, source)
	var id string
	if err == nil && call.Store(&id) != nil {
		err = ErrUnavailable
	}
	return id, err
}
func (c *Client) StopAudio(ctx context.Context, id string) error {
	_, err := c.Call(ctx, "Audio", "Stop", id)
	return err
}
func (c *Client) SetVolume(ctx context.Context, percent uint32) error {
	_, err := c.Call(ctx, "Audio", "SetVolume", percent)
	return err
}
func (c *Client) WaitAudio(ctx context.Context, id string) (string, error) {
	call := c.Conn.Object(Destination, Path("Audio")).CallWithContext(ctx, Interface("Audio")+".Wait", 0, id)
	var outcome string
	if call.Store(&outcome) != nil {
		return "", ErrUnavailable
	}
	return outcome, nil
}

type AudioStatus struct {
	ID     string
	State  string
	Volume uint32
}

func (c *Client) AudioStatus(ctx context.Context) (AudioStatus, error) {
	var status AudioStatus
	err := c.Property(ctx, "Audio", "Status", &status)
	return status, err
}
