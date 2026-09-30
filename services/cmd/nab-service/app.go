package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/guilhem/nabos/services/internal/airquality"
	"github.com/guilhem/nabos/services/internal/bus"
	"github.com/guilhem/nabos/services/internal/clock"
	"github.com/guilhem/nabos/services/internal/config"
	"github.com/guilhem/nabos/services/internal/ha"
	"github.com/guilhem/nabos/services/internal/system"
	"github.com/guilhem/nabos/services/internal/triggers"
	"github.com/guilhem/nabos/services/internal/update"
	"github.com/guilhem/nabos/services/internal/voice"
	"github.com/guilhem/nabos/services/internal/weather"
	"github.com/guilhem/nabos/services/internal/web"
)

const cmdTTL = time.Minute

type App struct {
	env           Env
	store         *config.Store
	bus           *bus.Bus
	auth          *web.Auth
	wx            *weather.Client
	upd           *update.Updater
	ha            *ha.Bridge
	voice         *voice.Client
	started       time.Time
	ctx           context.Context
	events        chan appEvent
	mediaGate     chan struct{}
	interaction   *interaction
	radioCancel   context.CancelFunc
	mediaCancel   context.CancelFunc
	serviceErrors map[string]string
	aq            *airquality.Client
	triggers      *triggers.Client
	airKick       chan struct{}
	aqMu          sync.Mutex
	airResult     *airquality.Result
	airFetched    time.Time
	airQuery      airquality.Query
	mastodonMu    sync.Mutex
	sshMu         sync.Mutex
	wifiMu        sync.Mutex
	wifi          *wifiSession
	mastodonKick  chan struct{}
	oauth         *oauthLogin

	clockKick, weatherKick chan struct{}
	updateKick             chan struct{}
	rebootSystem           func() error

	mu             sync.Mutex
	forecast       *weather.Forecast
	wxErr          string
	clk            clock.State
	lastTag        map[string]any
	network        string
	nextWeather    time.Time
	wakeupDone     bool
	bedtimeDone    bool
	indicator      string
	voiceCancel    context.CancelFunc
	haErr          string
	updateBusy     bool
	updateError    string
	updateRebooted bool
}

func NewApp(env Env) (*App, error) {
	store, err := config.Open(filepath.Join(env.DataDir, "config.json"))
	if err != nil {
		return nil, err
	}
	if store.Recovered != "" {
		slog.Error("settings file was corrupt, defaults restored", "saved_as", store.Recovered)
	}
	identity, err := os.ReadFile("/etc/machine-id")
	if err != nil || len(strings.TrimSpace(string(identity))) != 32 {
		return nil, errors.New("device machine-id is missing or invalid")
	}
	// All images have the same hostname. Use the per-device persistent identity,
	// hashed for this application instead of exposing the system machine-id.
	node := fmt.Sprintf("nabos_%x", sha256.Sum256([]byte("nabos-ha:"+strings.TrimSpace(string(identity)))))
	a := &App{
		env:           env,
		store:         store,
		auth:          web.NewAuth(store),
		wx:            weather.NewClient(env.WeatherURL, env.GeocodingURL),
		upd:           update.New(env.UpdateRepo, env.UpdateAsset, env.Version, filepath.Join(env.DataDir, "updates")),
		started:       time.Now(),
		clockKick:     make(chan struct{}, 1),
		weatherKick:   make(chan struct{}, 1),
		updateKick:    make(chan struct{}, 1),
		rebootSystem:  system.Reboot,
		clk:           clock.State{LastChime: -1},
		network:       "ok",
		ctx:           context.Background(),
		events:        make(chan appEvent, 128),
		mediaGate:     make(chan struct{}, 1),
		serviceErrors: map[string]string{},
		aq:            airquality.NewClient(),
		triggers:      triggers.NewClient(),
		airKick:       make(chan struct{}, 1),
		mastodonKick:  make(chan struct{}, 1),
	}
	a.upd.APIBase, a.upd.DownloadBase = env.GitHubAPI, env.GitHubDownload
	a.ha = &ha.Bridge{Node: node, Model: "Nabaztag", Version: env.Version, OnCommand: a.haCommand}
	a.bus = bus.New(env.MQTTHost, env.MQTTPort, "nab-service", bus.Handlers{
		OnState: a.onState,
		OnEvent: func(kind string, p map[string]any) {
			select {
			case a.events <- appEvent{kind: kind, data: p, received: time.Now()}:
			default:
				slog.Warn("event queue full", "event", kind)
			}
		},
		OnCoreOnline: func() { go a.resync() },
	})
	return a, nil
}

func kick(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (a *App) Run(ctx context.Context) error {
	a.ctx = ctx
	go a.eventLoop(ctx)
	defer a.stopMedia()
	if err := a.bus.Start(ctx); err != nil {
		return err
	}
	a.publishSettings(ctx)
	// The wheel owns the hardware level. Start without software attenuation.
	if a.store.Get().Volume != 100 {
		if _, err := a.store.Update(func(s *config.Settings) error { s.Volume = 100; return nil }); err != nil {
			slog.Warn("startup volume not saved", "err", err)
		}
	}
	if err := system.SetVolume(ctx, 100); err != nil {
		slog.Warn("startup volume not applied", "err", err)
	}
	if err := a.ha.Start(a.store.Get().HomeAssistant); err != nil {
		a.setHAErr(err)
	}
	if a.voiceEnabled() {
		a.startVoice()
	}
	srv := &http.Server{Addr: a.env.HTTPAddr, Handler: a.routes(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	go a.clockLoop(ctx)
	go a.weatherLoop(ctx)
	go a.networkLoop(ctx)
	go a.updateLoop(ctx)
	go a.servicesLoop(ctx)
	slog.Info("nab-service started", "version", a.env.Version, "http", a.env.HTTPAddr)
	select {
	case <-ctx.Done():
	case err := <-errc:
		return err
	}
	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	srv.Shutdown(sctx)
	a.ha.Stop()
	a.bus.Stop(sctx)
	return nil
}

func (a *App) send(action string, args any) {
	if action == "play" || action == "message" {
		go func() { a.serviceError(action, a.media(a.ctx, action, args, 10*time.Minute)) }()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := a.bus.Send(ctx, action, args, cmdTTL); err != nil {
		slog.Warn("core command not sent", "action", action, "err", err)
	}
}

// do sends a command and waits for its result (UI actions).
func (a *App) do(ctx context.Context, action string, args any, wait time.Duration) error {
	if action == "play" || action == "message" {
		return a.media(ctx, action, args, wait)
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	r, err := a.bus.Do(ctx, action, args, wait)
	if err != nil {
		return err
	}
	return r.Err()
}

func (a *App) location() *time.Location {
	if loc, err := time.LoadLocation(a.store.Get().Timezone); err == nil {
		return loc
	}
	return time.Local
}

// clockQuality: NTP sync, manual setting during this boot, time restored by
// timesyncd from its saved clock (coarse after a power cut), or unknown.
func (a *App) clockQuality() (clock.Quality, string) {
	if a.env.TimesyncFile == "" || a.env.TimesyncFile == "none" {
		return clock.Exact, "ntp"
	}
	if _, err := os.Stat(a.env.TimesyncFile); err == nil {
		return clock.Exact, "ntp"
	}
	if b, err := os.ReadFile(a.manualClockFile()); err == nil {
		if id := system.BootID(); id != "" && strings.TrimSpace(string(b)) == id {
			return clock.Exact, "manual"
		}
	}
	// timesyncd sets the clock to at least this file's mtime at boot.
	if fi, err := os.Stat(a.env.TimesyncClock); err == nil && !time.Now().Before(fi.ModTime().Add(-time.Minute)) {
		return clock.Coarse, "restored"
	}
	return clock.Unknown, "unknown"
}

func (a *App) manualClockFile() string { return filepath.Join(a.env.DataDir, "clock-manual") }

// SetClock sets the time by hand (offline rabbit); trusted until next boot.
func (a *App) SetClock(t time.Time) error {
	if err := system.SetTime(t); err != nil {
		return err
	}
	if err := os.WriteFile(a.manualClockFile(), []byte(system.BootID()+"\n"), 0o640); err != nil {
		return err
	}
	kick(a.clockKick)
	return nil
}

func (a *App) publishSettings(ctx context.Context) {
	a.mu.Lock()
	n := a.network
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	a.bus.PublishSettings(ctx, map[string]any{"v": 1, "locale": a.store.Get().Locale, "network": n})
}

func (a *App) applyVolume(ctx context.Context) {
	if err := system.SetVolume(ctx, a.store.Get().Volume); err != nil {
		slog.Warn("volume not applied", "err", err)
	}
}

// resync sends what the core keeps in memory after it (re)starts.
func (a *App) resync() {
	a.stopMedia()
	st := a.store.Get()
	a.send("ears", map[string]any{"left": st.Ears[0], "right": st.Ears[1]})
	a.mu.Lock()
	a.indicator = ""
	a.mu.Unlock()
	a.pushInfos()
	kick(a.airKick)
	kick(a.clockKick)
}

func (a *App) onState(s bus.CoreState) {
	a.mu.Lock()
	if s.State != "playing" {
		asleep := s.State == "asleep"
		a.clk.Asleep = &asleep
	}
	a.mu.Unlock()
	a.ha.State(s.State, a.store.Get().Volume, s.Ears.Left, s.Ears.Right)
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func (a *App) setOverride(sleep bool) {
	if _, err := a.store.Update(func(s *config.Settings) error { s.Clock.Override = &sleep; return nil }); err != nil {
		slog.Error("settings", "err", err)
	}
	kick(a.clockKick)
}

func (a *App) onEvent(e appEvent) {
	kind, p := e.kind, e.data
	if kind == "button" {
		a.ha.Button(str(p, "event"))
		a.auth.MarkPresence()
		if str(p, "event") == "double_click_and_hold" {
			a.stopInteraction()
			a.serviceError("admin", a.auth.Reset())
			return
		}
	}
	if a.interactionEvent(e) {
		return
	}
	switch kind {
	case "button":
		ev := str(p, "event")
		switch ev {
		case "click":
			if a.voiceCommandForClick() {
				return
			}
			a.setOverride(false) // a click wakes the rabbit up
		case "hold":
			a.mu.Lock()
			c := a.voice
			a.mu.Unlock()
			if c != nil {
				c.Send(context.Background(), "start_listening")
			}
		case "triple_click":
			slog.Info("triple click: powering off")
			if err := system.PowerOff(); err != nil {
				slog.Error("power off", "err", err)
			}
		case "click_and_hold":
			if a.store.Get().Services.Eightball {
				a.serviceError("eightball", a.startInteraction("eightball", ""))
			}
		}
	case "ears":
		l, lok := p["left"].(float64)
		r, rok := p["right"].(float64)
		a.store.Update(func(s *config.Settings) error {
			if lok {
				s.Ears[0] = int(l)
			}
			if rok {
				s.Ears[1] = int(r)
			}
			return nil
		})
		a.mastodonEars(p)
	case "rfid":
		a.mu.Lock()
		a.lastTag = p
		a.mu.Unlock()
		a.ha.Tag(p)
		if str(p, "event") != "detected" {
			return
		}
		data := str(p, "data")
		switch str(p, "app") {
		case "clock":
			a.setOverride(!(len(data) > 0 && data[0] == 1))
		case "weather":
			day := 0
			if len(data) > 0 && data[0] == 2 {
				day = 1
			}
			go a.announceWeather(day)
		default:
			go a.serviceTag(p)
		}
	}
}

func (a *App) haCommand(c ha.Command) {
	switch c.Name {
	case "sleep":
		a.setOverride(c.Value == "ON")
	case "chime":
		if err := a.sayTime(); err != nil {
			slog.Warn("chime from Home Assistant", "err", err)
		}
	case "weather":
		a.announceWeather(0)
	case "weather_tomorrow":
		a.announceWeather(1)
	case "taichi", "surprise", "eightball", "airquality", "carrot", "birthday", "autopromo":
		go func() {
			name, kind := c.Name, ""
			if name == "carrot" || name == "birthday" || name == "autopromo" {
				name, kind = "surprise", name
			}
			a.serviceError(name, a.performService(a.ctx, name, "default", kind))
		}()
	case "volume":
		var v int
		fmt.Sscan(c.Value, &v)
		if _, err := a.store.Update(func(s *config.Settings) error { s.Volume = v; return nil }); err == nil {
			a.applyVolume(context.Background())
		}
	case "left_ear", "right_ear":
		var v int
		fmt.Sscan(c.Value, &v)
		st, err := a.store.Update(func(s *config.Settings) error {
			s.Ears[map[string]int{"left_ear": 0, "right_ear": 1}[c.Name]] = v
			return nil
		})
		if err == nil {
			a.send("ears", map[string]any{"left": st.Ears[0], "right": st.Ears[1]})
		}
	}
}

// Clock

func (a *App) chime(hour int) {
	a.send("message", map[string]any{
		"signature": map[string]any{"audio": []string{"clock/signature.mp3"}},
		"body":      []any{map[string]any{"audio": []string{fmt.Sprintf("clock/%d/*.mp3", hour)}}},
	})
}

var errClockUntrusted = errors.New("l'heure du lapin n'est pas fiable : connectez-le à Internet ou réglez l'heure dans Réglages")

// sayTime announces the nearest hour, only when the clock is exact.
func (a *App) sayTime() error {
	if q, _ := a.clockQuality(); q != clock.Exact {
		return errClockUntrusted
	}
	now := time.Now().In(a.location())
	h := now.Hour()
	if now.Minute() >= 55 {
		h = (h + 1) % 24
	}
	a.chime(h)
	return nil
}

func (a *App) clockLoop(ctx context.Context) {
	for {
		wait := time.Until(time.Now().Truncate(time.Minute).Add(time.Minute))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		case <-a.clockKick:
		}
		a.clockTick(time.Now().In(a.location()))
	}
}

func (a *App) clockTick(now time.Time) {
	st := a.store.Get()
	q, _ := a.clockQuality()
	a.mu.Lock()
	act := clock.Decide(st.Clock, now, &a.clk, q)
	if act.Sleep || act.Wakeup {
		a.clk.Asleep = nil // wait for the core state
	}
	a.mu.Unlock()
	if act.ClearOverride {
		a.store.Update(func(s *config.Settings) error { s.Clock.Override = nil; return nil })
	}
	sounds := st.Clock.SleepSounds && time.Since(a.started) > time.Minute
	if act.Sleep {
		a.stopRadio()
		go func() {
			ctx, cancel := context.WithCancel(a.ctx)
			defer cancel()
			if a.acquireMedia(ctx) != nil {
				return
			}
			defer func() { <-a.mediaGate }()
			// The schedule may have changed while an interactive book finished.
			current := a.store.Get().Clock
			should := clock.ShouldSleep(current, time.Now().In(a.location()))
			if current.Override != nil {
				should = *current.Override
			}
			if !should {
				a.mu.Lock()
				awake := false
				a.clk.Asleep = &awake
				a.mu.Unlock()
				kick(a.clockKick)
				return
			}
			if sounds {
				c, stop := context.WithTimeout(ctx, time.Minute)
				a.playOwned(c, "play", sequence("sleep/*.mp3", ""))
				stop()
			}
			a.send("sleep", nil)
		}()
	}
	if act.Wakeup {
		a.send("wakeup", nil)
		if sounds {
			a.send("play", map[string]any{"sequence": []any{map[string]any{"audio": []string{"wakeup/*.mp3"}}}})
		}
	}
	if act.Chime {
		a.chime(now.Hour())
	}
	a.weatherSchedule(now, st)
}

// Weather

func (a *App) refreshWeather(ctx context.Context) {
	cfg := a.store.Get().Weather
	if cfg.Location == "" {
		a.mu.Lock()
		a.forecast, a.wxErr = nil, ""
		a.mu.Unlock()
		a.pushInfos()
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	f, err := a.wx.Fetch(ctx, cfg.Latitude, cfg.Longitude)
	a.mu.Lock()
	if err != nil {
		a.wxErr = err.Error()
		// Keep a recent forecast when the Internet is briefly unavailable.
		if a.forecast != nil && time.Since(a.forecast.Fetched) > 6*time.Hour {
			a.forecast = nil
		}
	} else {
		a.forecast, a.wxErr = f, ""
	}
	a.mu.Unlock()
	if err != nil {
		slog.Warn("weather unavailable", "err", err)
	}
	a.pushInfos()
}

func (a *App) pushInfos() {
	cfg := a.store.Get().Weather
	a.mu.Lock()
	f := a.forecast
	a.mu.Unlock()
	wi, ri := weather.Infos(cfg, f)
	a.send("info", map[string]any{"info_id": "weather", "animation": wi})
	a.send("info", map[string]any{"info_id": "weather_rain", "animation": ri})
}

func (a *App) weatherLoop(ctx context.Context) {
	for {
		a.refreshWeather(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Minute):
		case <-a.weatherKick:
		}
	}
}

func (a *App) announceWeather(day int) {
	a.mu.Lock()
	stale := a.forecast == nil || time.Since(a.forecast.Fetched) > time.Hour
	a.mu.Unlock()
	if stale {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		a.refreshWeather(ctx)
		cancel()
	}
	a.mu.Lock()
	f := a.forecast
	a.mu.Unlock()
	a.send("message", weather.Message(a.store.Get().Weather, f, day))
}

func (a *App) weatherSchedule(now time.Time, st config.Settings) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch st.Weather.Frequency {
	case 1, 2:
		if a.nextWeather.IsZero() {
			a.nextWeather = weather.NextAnnouncement(st.Weather.Frequency, now, rand.IntN)
		}
		if now.After(a.nextWeather) {
			a.nextWeather = weather.NextAnnouncement(st.Weather.Frequency, now, rand.IntN)
			day := 0
			if now.Hour() > 18 {
				day = 1
			}
			go a.announceWeather(day)
		}
	case 3:
		a.nextWeather = time.Time{}
		w, s := clock.Schedule(st.Clock, now)
		at := func(h config.HM) time.Time {
			return time.Date(now.Year(), now.Month(), now.Day(), h.Hour, h.Min, 0, 0, now.Location())
		}
		wake, bed := at(w), at(s)
		if now.After(wake) && now.Before(wake.Add(5*time.Minute)) {
			if !a.wakeupDone {
				a.wakeupDone = true
				go a.announceWeather(0)
			}
		} else {
			a.wakeupDone = false
		}
		if now.After(bed.Add(-5*time.Minute)) && now.Before(bed) {
			if !a.bedtimeDone {
				a.bedtimeDone = true
				go a.announceWeather(1)
			}
		} else {
			a.bedtimeDone = false
		}
	default:
		a.nextWeather = time.Time{}
	}
}

// Network, updates

func (a *App) networkLoop(ctx context.Context) {
	for {
		n := system.Network(ctx, a.env.NetProbe)
		a.mu.Lock()
		changed := n != a.network
		a.network = n
		a.mu.Unlock()
		if changed {
			slog.Info("network", "status", n)
			a.publishSettings(ctx)
			if n == "ok" {
				kick(a.weatherKick)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}

// Home Assistant

func (a *App) setHAErr(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.haErr = err.Error()
	} else {
		a.haErr = ""
	}
}

// Voice assistant (Linux Voice Assistant peripheral API)

func (a *App) voiceSupported() bool { return a.env.LVAUnit != "" }

func (a *App) voiceFlag() string { return filepath.Join(a.env.DataDir, "voice-enabled") }

func (a *App) voiceEnabled() bool {
	_, err := os.Stat(a.voiceFlag())
	return a.voiceSupported() && err == nil
}

func (a *App) startVoice() {
	ctx, cancel := context.WithCancel(context.Background())
	c := &voice.Client{URL: a.env.LVAURL, OnEvent: a.voiceEvent}
	a.mu.Lock()
	if a.voiceCancel != nil {
		a.voiceCancel()
	}
	a.voice, a.voiceCancel = c, cancel
	a.mu.Unlock()
	go c.Run(ctx)
}

func (a *App) stopVoice() {
	a.mu.Lock()
	cancel := a.voiceCancel
	a.voice, a.voiceCancel = nil, nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	a.setIndicator("")
}

// SetVoice enables or disables the voice assistant service.
func (a *App) SetVoice(on bool) error {
	if !a.voiceSupported() {
		return errors.New("voice assistant is not available on this board")
	}
	if on == a.voiceEnabled() {
		return nil
	}
	if on {
		if err := os.WriteFile(a.voiceFlag(), nil, 0o640); err != nil {
			return err
		}
		a.startVoice()
		return system.StartUnit(a.env.LVAUnit)
	}
	os.Remove(a.voiceFlag())
	a.stopVoice()
	return system.StopUnit(a.env.LVAUnit)
}

var indicators = map[string]*weather.Animation{
	"listening": {Tempo: 50, Colors: []map[string]string{{"left": "0000ff", "center": "0000ff", "right": "0000ff"}}},
	"thinking":  {Tempo: 25, Colors: []map[string]string{{"left": "8000ff", "center": "000000", "right": "8000ff"}, {"left": "000000", "center": "8000ff", "right": "000000"}}},
	"speaking":  {Tempo: 20, Colors: []map[string]string{{"left": "000000", "center": "00ff00", "right": "000000"}, {"left": "00ff00", "center": "00ff00", "right": "00ff00"}}},
	"error":     {Tempo: 15, Colors: []map[string]string{{"left": "ff0000", "center": "ff0000", "right": "ff0000"}, {"left": "000000", "center": "000000", "right": "000000"}}},
	"timer":     {Tempo: 25, Colors: []map[string]string{{"left": "ff8000", "center": "000000", "right": "ff8000"}, {"left": "000000", "center": "ff8000", "right": "000000"}}},
}

func (a *App) setIndicator(name string) {
	a.mu.Lock()
	same := a.indicator == name
	a.indicator = name
	a.mu.Unlock()
	if !same {
		a.send("indicator", map[string]any{"animation": indicators[name]})
	}
}

func (a *App) voiceEvent(event string, _ map[string]any) {
	switch event {
	case "wake_word_detected", "listening":
		a.setIndicator("listening")
	case "thinking":
		a.setIndicator("thinking")
	case "tts_speaking":
		a.setIndicator("speaking")
	case "timer_ringing":
		a.setIndicator("timer")
	case "pipeline_error":
		a.setIndicator("error")
		time.AfterFunc(1500*time.Millisecond, func() { a.setIndicator("") })
	case "idle", "tts_finished", "disconnected", "snapshot":
		a.setIndicator("")
	}
}

// voiceCommandForClick sends the context command of LVA's action button and
// reports whether the click was consumed.
func (a *App) voiceCommandForClick() bool {
	a.mu.Lock()
	c := a.voice
	a.mu.Unlock()
	if c == nil {
		return false
	}
	cmd := ""
	switch c.State() {
	case "timer_ringing":
		cmd = "stop_timer_ringing"
	case "wake_word_detected", "listening", "thinking", "tts_speaking":
		cmd = "stop_pipeline"
	case "media_player_playing":
		cmd = "stop_media_player"
	}
	return cmd != "" && c.Send(context.Background(), cmd) == nil
}
