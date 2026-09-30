// Package network is the bounded D-Bus client of nab-core's Network1 API.
// It never contacts NetworkManager directly or stores Wi-Fi secrets.
package network

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/godbus/dbus/v5"
)

const (
	Destination = "org.nabaztag.Core"
	Path        = dbus.ObjectPath("/org/nabaztag/Core/Network")
	Interface   = "org.nabaztag.Core.Network1"
	callTimeout = 5 * time.Second
)

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
	Address     string `json:"address"`
	SSID        SSID   `json:"ssid"`
	ProfileUUID string `json:"profile_uuid"`
	AttemptID   uint64 `json:"attempt_id"`
	Phase       string `json:"phase"`
	Error       string `json:"error"`
}

func (s Status) ClientReady() bool {
	ip := net.ParseIP(s.Address)
	return s.Mode == "client" && ip != nil && ip.IsGlobalUnicast() && !ip.IsUnspecified()
}

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

func call(ctx context.Context, method string, args ...any) (*dbus.Call, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	conn, err := dbus.ConnectSystemBus(dbus.WithContext(ctx))
	if err != nil {
		return nil, ErrUnavailable
	}
	defer conn.Close()
	c := conn.Object(Destination, Path).CallWithContext(ctx, method, 0, args...)
	if c.Err != nil {
		// Error bodies may contain the submitted secret. Only fixed messages
		// cross the HTTP boundary; neither requests nor errors are logged.
		var e dbus.Error
		if errors.As(c.Err, &e) {
			switch e.Name {
			case "org.freedesktop.DBus.Error.Failed", "org.freedesktop.DBus.Error.InvalidArgs", "org.freedesktop.DBus.Error.AccessDenied", "org.freedesktop.DBus.Error.NotSupported":
				return nil, ErrRefused
			}
			if strings.HasPrefix(e.Name, "org.nabaztag.") {
				return nil, ErrRefused
			}
		}
		return nil, ErrUnavailable
	}
	return c, nil
}

func ReadStatus(ctx context.Context) (Status, error) {
	c, err := call(ctx, "org.freedesktop.DBus.Properties.Get", Interface, "Status")
	if err != nil {
		return Status{}, err
	}
	var v dbus.Variant
	var s Status
	if c.Store(&v) != nil {
		return s, ErrUnavailable
	}
	text, ok := v.Value().(string)
	if !ok || json.Unmarshal([]byte(text), &s) != nil {
		return Status{}, ErrUnavailable
	}
	return s, nil
}

func Read(ctx context.Context) (Snapshot, error) {
	c, err := call(ctx, "org.freedesktop.DBus.Properties.GetAll", Interface)
	if err != nil {
		return Snapshot{}, err
	}
	var properties map[string]dbus.Variant
	if c.Store(&properties) != nil {
		return Snapshot{}, ErrUnavailable
	}
	var s Snapshot
	for name, dst := range map[string]any{"Status": &s.Status, "Networks": &s.Networks, "Profiles": &s.Profiles} {
		v, ok := properties[name].Value().(string)
		if !ok || json.Unmarshal([]byte(v), dst) != nil {
			return Snapshot{}, ErrUnavailable
		}
	}
	return s, nil
}

func Scan(ctx context.Context) error { _, err := call(ctx, Interface+".Scan"); return err }
func Reserve(ctx context.Context, token string) (string, error) {
	c, err := call(ctx, Interface+".Reserve", token)
	if err != nil {
		return "", err
	}
	var value string
	if c.Store(&value) != nil || value == "" {
		return "", ErrUnavailable
	}
	return value, nil
}
func Authorized(ctx context.Context, token string) (bool, error) {
	c, err := call(ctx, Interface+".Authorized", token)
	if err != nil {
		return false, err
	}
	var value bool
	if c.Store(&value) != nil {
		return false, ErrUnavailable
	}
	return value, nil
}
func Release(ctx context.Context, token string) error {
	_, err := call(ctx, Interface+".Release", token)
	return err
}
func Connect(ctx context.Context, ssid SSID, security, password, uuid, token string) (uint64, error) {
	c, err := call(ctx, Interface+".Connect", []byte(ssid), security, password, uuid, token)
	if err != nil {
		return 0, err
	}
	var id uint64
	if c.Store(&id) != nil || id == 0 {
		return 0, ErrUnavailable
	}
	return id, nil
}
func Cancel(ctx context.Context, id uint64) error {
	_, err := call(ctx, Interface+".Cancel", id)
	return err
}
func Forget(ctx context.Context, uuid string) error {
	_, err := call(ctx, Interface+".Forget", uuid)
	return err
}
