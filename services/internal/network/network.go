// Package network wraps device-core's typed Network API.
// It never contacts NetworkManager directly or stores Wi-Fi secrets.
package network

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/godbus/dbus/v5"
	"github.com/guilhem/nabos/services/internal/device"
)

const (
	Destination = device.Destination
	Path        = dbus.ObjectPath("/io/github/guilhem/DeviceCore1/Network")
	Interface   = Destination + ".Network"
)

type Client struct{ *device.Client }

var (
	ErrUnavailable = errors.New("configuration Wi-Fi indisponible, réessayez")
	ErrRefused     = errors.New("opération Wi-Fi refusée, vérifiez le réseau et la confirmation physique")
)

// SSID preserves the wire bytes, including non-UTF-8 names. JSON uses byte
// arrays, as in the Rust contract, rather than encoding/json's base64 strings.
type SSID []byte

func (s SSID) MarshalJSON() ([]byte, error) {
	v := make([]int, len(s))
	for i, b := range s {
		v[i] = int(b)
	}
	return json.Marshal(v)
}

func (s SSID) String() string {
	if len(s) == 0 {
		return "Réseau masqué"
	}
	if utf8.Valid(s) && !strings.ContainsFunc(string(s), unicode.IsControl) {
		return string(s)
	}
	return "0x" + hex.EncodeToString(s)
}

type Status struct {
	Mode        string `json:"mode"`
	Generation  string `json:"generation"`
	Ready       bool   `json:"ready"`
	Address     string `json:"address"`
	SSID        SSID   `json:"ssid"`
	ProfileUUID string `json:"profile_uuid"`
	AttemptID   uint64 `json:"attempt_id"`
	Phase       string `json:"phase"`
	Error       string `json:"error"`
}

func (s Status) ClientReady() bool { return s.Mode == "client" && s.Ready }

type Network struct {
	SSID     SSID   `json:"ssid"`
	Strength uint8  `json:"strength"`
	Security string `json:"security"`
}

type Profile struct {
	UUID string `json:"uuid"`
	SSID SSID   `json:"ssid"`
}

type Snapshot struct {
	Status   Status    `json:"status"`
	Networks []Network `json:"networks"`
	Profiles []Profile `json:"profiles"`
}

func (c *Client) call(ctx context.Context, method string, args ...any) (*dbus.Call, error) {
	call, err := c.Client.Call(ctx, "Network", method, args...)
	if errors.Is(err, device.ErrRefused) {
		return nil, ErrRefused
	}
	if err != nil {
		return nil, ErrUnavailable
	}
	return call, nil
}

func (c *Client) ReadStatus(ctx context.Context) (Status, error) {
	reply, err := c.call(ctx, "org.freedesktop.DBus.Properties.Get", Interface, "Status")
	if err != nil {
		return Status{}, err
	}
	var v dbus.Variant
	var s Status
	if reply.Store(&v) != nil {
		return s, ErrUnavailable
	}
	if v.Store(&s) != nil {
		return Status{}, ErrUnavailable
	}
	return s, nil
}

func (c *Client) Read(ctx context.Context) (Snapshot, error) {
	reply, err := c.call(ctx, "org.freedesktop.DBus.Properties.GetAll", Interface)
	if err != nil {
		return Snapshot{}, err
	}
	var properties map[string]dbus.Variant
	if reply.Store(&properties) != nil {
		return Snapshot{}, ErrUnavailable
	}
	var s Snapshot
	for name, dst := range map[string]any{"Status": &s.Status, "Networks": &s.Networks, "Profiles": &s.Profiles} {
		v, ok := properties[name]
		if !ok || v.Store(dst) != nil {
			return Snapshot{}, ErrUnavailable
		}
	}
	return s, nil
}

func (c *Client) Scan(ctx context.Context) error {
	_, err := c.call(ctx, Interface+".Scan")
	return err
}
func (c *Client) Reserve(ctx context.Context, token string) (string, error) {
	reply, err := c.call(ctx, Interface+".Reserve", token)
	if err != nil {
		return "", err
	}
	var value string
	if reply.Store(&value) != nil || value == "" {
		return "", ErrUnavailable
	}
	return value, nil
}
func (c *Client) Authorized(ctx context.Context, token string) (bool, error) {
	reply, err := c.call(ctx, Interface+".Authorized", token)
	if err != nil {
		return false, err
	}
	var value bool
	if reply.Store(&value) != nil {
		return false, ErrUnavailable
	}
	return value, nil
}
func (c *Client) Release(ctx context.Context, token string) error {
	_, err := c.call(ctx, Interface+".Release", token)
	return err
}
func (c *Client) Connect(ctx context.Context, ssid SSID, security, password, uuid, token string) (uint64, error) {
	reply, err := c.call(ctx, Interface+".Connect", []byte(ssid), security, password, uuid, token)
	if err != nil {
		return 0, err
	}
	var id uint64
	if reply.Store(&id) != nil || id == 0 {
		return 0, ErrUnavailable
	}
	return id, nil
}
func (c *Client) Cancel(ctx context.Context, id uint64) error {
	_, err := c.call(ctx, Interface+".Cancel", id)
	return err
}
func (c *Client) Forget(ctx context.Context, uuid string) error {
	_, err := c.call(ctx, Interface+".Forget", uuid)
	return err
}

// Guard returns an FD whose lifetime covers the application commit. The daemon
// checks generation under the same exclusive lock used by every radio mutation.
func (c *Client) Guard(ctx context.Context, generation string) (*os.File, error) {
	reply, err := c.call(ctx, "AcquireGuard", generation)
	if err != nil {
		return nil, err
	}
	var fd dbus.UnixFD
	if reply.Store(&fd) != nil || fd < 0 {
		return nil, ErrUnavailable
	}
	return os.NewFile(uintptr(fd), "device-core-network-guard"), nil
}
