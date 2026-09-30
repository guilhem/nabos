package rabbit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"github.com/guilhem/nabos/services/internal/hardware"
	"regexp"
	"strings"
	"time"
)

type Action uint8

const (
	Play Action = iota
	Message
	Cancel
	Info
	Indicator
	Ears
	Sleep
	Wakeup
	RfidWrite
	Test
	Gestalt
)

type RGB = [3]uint8
type Item struct {
	Audio        []string
	Stream       string
	Choreography string
}
type Animation struct {
	Tempo  uint32
	Frames [][3]RGB
}
type TagWrite struct {
	Tech    string
	UID     []byte
	Picture uint8
	App     uint8
	Data    []byte
	Timeout uint32
}
type Command struct {
	ID         string
	Deadline   time.Time
	Action     Action
	Sequence   []Item
	Signature  *Item
	Body       []Item
	Cancelable bool
	Target     string
	InfoID     string
	Animation  *Animation
	Left       *uint8
	Right      *uint8
	Tag        *TagWrite
	Test       string
}
type Result struct {
	Status string
	Error  string
	UID    []byte
}

func (r Result) Err() error {
	if r.Error != "" {
		return errors.New(r.Error)
	}
	if r.Status == "canceled" {
		return context.Canceled
	}
	if r.Status != "ok" {
		return errors.New(r.Status)
	}
	return nil
}
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

type Event struct {
	generation      uint64
	Kind            string
	Button          string
	EdgeMonotonicNS uint64
	Ear             uint8
	Left            *uint8
	Right           *uint8
	Tag             *hardware.Tag
}
type EarPositions struct{ Left, Right int }
type State struct {
	State    string
	Playing  *string
	Ears     EarPositions
	Hardware hardware.Status
	Version  string
}

func (s State) HardwareReady() bool { return s.Hardware.Ready() }

type Options struct {
	SoundsDirs []string
	ChorDirs   []string
	Version    string
}
type Handlers struct {
	OnState  func(State)
	OnEvent  func(Event)
	OnOnline func()
}

const streamingURN = "urn:x-chor:streaming"

var streamPattern = regexp.MustCompile(`^http://127\.0\.0\.1:([1-9][0-9]{3,4})/radio/[A-Za-z0-9_-]{16,64}$`)

func validResource(s string) bool {
	if s == "" || len(s) > 256 {
		return false
	}
	for _, p := range strings.Split(s, ";") {
		if strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") {
			return false
		}
		for _, c := range strings.Split(p, "/") {
			if c == "" || c == "." || c == ".." {
				return false
			}
		}
	}
	return true
}
func validChor(s string) bool {
	if strings.HasPrefix(s, streamingURN) {
		r := strings.TrimPrefix(s, streamingURN)
		return r == "" || (len(r) == 2 && r[0] == ':' && r[1] >= '0' && r[1] <= '7')
	}
	return validResource(s) && !strings.Contains(s, ":")
}
func validStream(s string) bool {
	m := streamPattern.FindStringSubmatch(s)
	if m == nil {
		return false
	}
	p := 0
	for _, c := range m[1] {
		p = p*10 + int(c-'0')
	}
	return p >= 1024 && p <= 65535
}
func validate(c Command) error {
	bad := func(s string) error { return errors.New(s) }
	if c.Action > Gestalt {
		return bad("invalid action")
	}
	item := func(i Item, stream bool) error {
		if len(i.Audio) > 16 {
			return bad("too many audio resources")
		}
		for _, r := range i.Audio {
			if !validResource(r) {
				return bad("invalid resource")
			}
		}
		if i.Stream != "" && (!stream || i.Audio != nil || !validStream(i.Stream)) {
			return bad("invalid stream")
		}
		if i.Choreography != "" && !validChor(i.Choreography) {
			return bad("invalid choreography")
		}
		return nil
	}
	if len(c.Sequence) > 32 || len(c.Body) > 32 {
		return bad("too many items")
	}
	for _, i := range c.Sequence {
		if err := item(i, c.Action == Play); err != nil {
			return err
		}
	}
	for _, i := range c.Body {
		if err := item(i, false); err != nil {
			return err
		}
	}
	if c.Signature != nil {
		if err := item(*c.Signature, false); err != nil {
			return err
		}
	}
	if c.Action == Info && (len(c.InfoID) == 0 || len(c.InfoID) > 64) {
		return bad("invalid info ID")
	}
	if a := c.Animation; a != nil && (a.Tempo < 1 || a.Tempo > 1000 || len(a.Frames) < 1 || len(a.Frames) > 64) {
		return bad("invalid animation")
	}
	if c.Left != nil && *c.Left > 16 || c.Right != nil && *c.Right > 16 {
		return bad("invalid ear position")
	}
	if c.Action == RfidWrite && (c.Tag == nil || !hardware.ValidTag(c.Tag.Tech, c.Tag.UID, c.Tag.Data, c.Tag.Timeout)) {
		return bad("invalid tag write")
	}
	if c.Action == Test && c.Test != "ears" && c.Test != "leds" {
		return bad("invalid test")
	}
	return nil
}
