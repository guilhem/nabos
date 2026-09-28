// Package config stores the rabbit settings in one versioned JSON file,
// written atomically (temp file, fsync, rename, fsync directory).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
	_ "time/tzdata" // timezones without system tzdata
)

const Version = 1

type HM struct {
	Hour int `json:"hour"`
	Min  int `json:"min"`
}

type Day struct {
	Wakeup HM `json:"wakeup"`
	Sleep  HM `json:"sleep"`
}

type Clock struct {
	Chime       bool   `json:"chime"`
	SleepSounds bool   `json:"sleep_sounds"`
	PerDay      bool   `json:"per_day"`
	Wakeup      HM     `json:"wakeup"`
	Sleep       HM     `json:"sleep"`
	Days        [7]Day `json:"days"` // Monday first
	// Override forces sleep (true) or awake (false) until the schedule agrees.
	Override *bool `json:"override"`
}

type Weather struct {
	Location  string  `json:"location"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Unit      string  `json:"unit"`      // celsius | fahrenheit
	Animation string  `json:"animation"` // weather_and_rain | weather_only | rain_only | nothing
	Frequency int     `json:"frequency"` // 0 never, 1 ~hourly, 2 ~every 2-3 h, 3 wakeup and bedtime
}

type HomeAssistant struct {
	Enabled  bool   `json:"enabled"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	Prefix   string `json:"discovery_prefix"`
}

type Admin struct {
	Salt       string `json:"salt"`
	Hash       string `json:"hash"`
	Iterations int    `json:"iterations"`
}

type Settings struct {
	Version       int           `json:"version"`
	Locale        string        `json:"locale"`
	Timezone      string        `json:"timezone"`
	Volume        int           `json:"volume"`
	Ears          [2]int        `json:"ears"`
	Admin         Admin         `json:"admin"`
	Clock         Clock         `json:"clock"`
	Weather       Weather       `json:"weather"`
	HomeAssistant HomeAssistant `json:"home_assistant"`
	AutoCheck     bool          `json:"auto_check_updates"`
}

func Defaults() Settings {
	s := Settings{
		Version:   Version,
		Locale:    "fr_FR",
		Timezone:  "Europe/Paris",
		Volume:    70,
		Clock:     Clock{Chime: true, SleepSounds: true, Wakeup: HM{7, 0}, Sleep: HM{22, 0}},
		Weather:   Weather{Unit: "celsius", Animation: "weather_and_rain", Frequency: 0},
		AutoCheck: true,
	}
	s.HomeAssistant.Port = 1883
	s.HomeAssistant.Prefix = "homeassistant"
	for i := range s.Clock.Days {
		s.Clock.Days[i] = Day{Wakeup: HM{7, 0}, Sleep: HM{22, 0}}
	}
	return s
}

var (
	localeRe = regexp.MustCompile(`^[a-z]{2}_[A-Z]{2}$`)
	hostRe   = regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}$`)
)

func (h HM) valid() bool { return h.Hour >= 0 && h.Hour < 24 && h.Min >= 0 && h.Min < 60 }

// Validate rejects values the rest of the program cannot handle.
func (s *Settings) Validate() error {
	switch {
	case !localeRe.MatchString(s.Locale):
		return fmt.Errorf("invalid locale %q", s.Locale)
	case s.Volume < 0 || s.Volume > 100:
		return errors.New("volume must be 0-100")
	case s.Ears[0] < 0 || s.Ears[0] > 16 || s.Ears[1] < 0 || s.Ears[1] > 16:
		return errors.New("ears must be 0-16")
	case !s.Clock.Wakeup.valid() || !s.Clock.Sleep.valid():
		return errors.New("invalid clock time")
	case s.Weather.Unit != "celsius" && s.Weather.Unit != "fahrenheit":
		return errors.New("invalid weather unit")
	case s.Weather.Frequency < 0 || s.Weather.Frequency > 3:
		return errors.New("invalid weather frequency")
	case s.Weather.Latitude < -90 || s.Weather.Latitude > 90 || s.Weather.Longitude < -180 || s.Weather.Longitude > 180:
		return errors.New("invalid coordinates")
	case s.HomeAssistant.Enabled && (!hostRe.MatchString(s.HomeAssistant.Host) || s.HomeAssistant.Port < 1 || s.HomeAssistant.Port > 65535):
		return errors.New("invalid Home Assistant broker")
	case s.HomeAssistant.Prefix == "" || len(s.HomeAssistant.Prefix) > 64:
		return errors.New("invalid discovery prefix")
	}
	switch s.Weather.Animation {
	case "weather_and_rain", "weather_only", "rain_only", "nothing":
	default:
		return errors.New("invalid weather animation")
	}
	for _, d := range s.Clock.Days {
		if !d.Wakeup.valid() || !d.Sleep.valid() {
			return errors.New("invalid clock time")
		}
	}
	if _, err := time.LoadLocation(s.Timezone); err != nil {
		return fmt.Errorf("invalid timezone %q", s.Timezone)
	}
	return nil
}

// Store guards the settings file. Readers get copies.
type Store struct {
	path string
	mu   sync.Mutex
	cur  Settings
	// Recovered is set when a corrupt file was moved aside at load.
	Recovered string
}

// Open loads path. A missing file gives defaults; an unreadable or invalid
// file is renamed to <path>.corrupt-<unix> and defaults are used, so a damaged
// SD card sector never prevents the rabbit from starting.
func Open(path string) (*Store, error) {
	st := &Store{path: path, cur: Defaults()}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err == nil {
		s := Defaults()
		if err = json.Unmarshal(raw, &s); err == nil {
			if s.Version > Version {
				// Written by a newer release (rollback): keep known fields.
				s.Version = Version
			}
			err = s.Validate()
		}
		if err == nil {
			st.cur = s
			return st, nil
		}
	}
	aside := fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix())
	if rerr := os.Rename(path, aside); rerr != nil {
		return nil, fmt.Errorf("settings unreadable (%v) and cannot be moved aside: %w", err, rerr)
	}
	st.Recovered = aside
	return st, nil
}

func (st *Store) Get() Settings {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.cur
}

// Update applies fn to a copy, validates and persists it before publishing it.
func (st *Store) Update(fn func(*Settings) error) (Settings, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	next := st.cur
	if next.Clock.Override != nil {
		o := *next.Clock.Override
		next.Clock.Override = &o
	}
	if err := fn(&next); err != nil {
		return st.cur, err
	}
	next.Version = Version
	if err := next.Validate(); err != nil {
		return st.cur, err
	}
	if err := writeAtomic(st.path, next); err != nil {
		return st.cur, err
	}
	st.cur = next
	return next, nil
}

func writeAtomic(path string, s Settings) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(append(raw, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
