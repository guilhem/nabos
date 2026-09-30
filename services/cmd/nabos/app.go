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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/guilhem/nabos/services/internal/airquality"
	"github.com/guilhem/nabos/services/internal/clock"
	"github.com/guilhem/nabos/services/internal/config"
	"github.com/guilhem/nabos/services/internal/device"
	"github.com/guilhem/nabos/services/internal/ha"
	"github.com/guilhem/nabos/services/internal/network"
	"github.com/guilhem/nabos/services/internal/rabbit"
	"github.com/guilhem/nabos/services/internal/triggers"
	"github.com/guilhem/nabos/services/internal/weather"
	"github.com/guilhem/nabos/services/internal/web"
)

const cmdTTL = time.Minute

type App struct {
	env             Env
	store           *config.Store
	rabbit          *rabbit.Engine
	auth            *web.Auth
	wx              *weather.Client
	device          *device.Client
	net             *network.Client
	agent           *maintenanceAgent
	ha              *ha.Bridge
	started         time.Time
	ctx             context.Context
	events          chan appEvent
	mediaGate       chan struct{}
	interaction     *interaction
	radioCancel     context.CancelFunc
	mediaCancel     context.CancelFunc
	serviceErrors   map[string]string
	aq              *airquality.Client
	triggers        *triggers.Client
	airKick         chan struct{}
	aqMu            sync.Mutex
	airResult       *airquality.Result
	airFetched      time.Time
	airQuery        airquality.Query
	mastodonMu      sync.Mutex
	wifiMu          sync.Mutex
	setupGeneration string
	wifi            *wifiSession
	mastodonKick    chan struct{}
	oauth           *oauthLogin

	clockKick, weatherKick, haKick chan struct{}

	mu          sync.Mutex
	forecast    *weather.Forecast
	wxErr       string
	clk         clock.State
	lastTag     *productTag
	network     string
	nextWeather time.Time
	wakeupDone  bool
	bedtimeDone bool
	indicator   string
	haErr       string
}

func NewApp(env Env) (*App, error) {
	store, err := config.Open(filepath.Join(env.DataDir, "application.json"))
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
		started:       time.Now(),
		clockKick:     make(chan struct{}, 1),
		weatherKick:   make(chan struct{}, 1),
		haKick:        make(chan struct{}, 1),
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
	client, err := device.Open()
	if err != nil {
		return nil, err
	}
	a.device, a.net = client, &network.Client{Client: client}
	a.agent = &maintenanceAgent{app: a}
	if err := a.device.Conn.Export(a.agent, device.Path("Agent"), device.Interface("Agent")); err != nil {
		client.Close()
		return nil, err
	}
	a.ha = &ha.Bridge{Node: node, Model: "Nabaztag", Version: env.Version, OnCommand: a.haCommand}
	a.rabbit = rabbit.New(rabbit.Options{SoundsDirs: env.SoundsDirs, ChorDirs: env.ChorDirs, Version: env.Version}, rabbit.Handlers{
		OnState: a.onState,
		OnEvent: func(e rabbit.Event) {
			select {
			case a.events <- appEvent{Event: e, received: time.Now()}:
			default:
				slog.Warn("event queue full", "event", e.Kind)
			}
		},
		OnOnline: func() { go a.resync() },
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
	ctx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	a.ctx = ctx
	defer a.device.Close()
	a.agent.holdRecovery()
	go a.eventLoop(ctx)
	defer func() {
		cancelRun()
		a.stopMedia()
		stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		a.rabbit.Stop(stopCtx)
		a.ha.Stop()
	}()
	if err := a.rabbit.Start(ctx); err != nil {
		return err
	}
	a.publishSettings(ctx)
	if err := a.ha.Start(a.store.Get().HomeAssistant); err != nil {
		a.setHAErr(err)
	}
	go a.deviceLoop(ctx)
	srv := &http.Server{Addr: a.env.HTTPAddr, Handler: a.routes(), ReadHeaderTimeout: 10 * time.Second}
	defer func() {
		shutdown, done := context.WithTimeout(context.Background(), 3*time.Second)
		defer done()
		_ = srv.Shutdown(shutdown)
	}()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	go a.clockLoop(ctx)
	go a.weatherLoop(ctx)
	go a.networkLoop(ctx)
	go a.servicesLoop(ctx)
	slog.Info("nabos started", "version", a.env.Version, "http", a.env.HTTPAddr)
	select {
	case <-ctx.Done():
	case <-a.device.Conn.Context().Done():
		return errors.New("system bus disconnected")
	case err := <-errc:
		return err
	}
	return nil
}

func (a *App) send(command rabbit.Command) {
	if command.Action == rabbit.Play || command.Action == rabbit.Message {
		go func() { a.serviceError("media", a.media(a.ctx, command, 10*time.Minute)) }()
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, 3*time.Second)
	defer cancel()
	command.Deadline = time.Now().Add(cmdTTL)
	if _, err := a.rabbit.Send(ctx, command); err != nil {
		slog.Warn("rabbit command not sent", "action", command.Action, "err", err)
	}
}

// do sends a command and waits for its result (UI actions).
func (a *App) do(ctx context.Context, command rabbit.Command, wait time.Duration) error {
	if command.Action == rabbit.Play || command.Action == rabbit.Message {
		return a.media(ctx, command, wait)
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	command.Deadline = time.Now().Add(wait)
	r, err := a.rabbit.Do(ctx, command)
	if err != nil {
		return err
	}
	return r.Err()
}

func earsCommand(left, right int) rabbit.Command {
	l, r := uint8(left), uint8(right)
	return rabbit.Command{Action: rabbit.Ears, Left: &l, Right: &r}
}

func (a *App) systemSettings(ctx context.Context) (device.Settings, error) {
	_, settings, err := a.device.ReadConfig(ctx)
	return settings, err
}
func (a *App) location() *time.Location {
	settings, err := a.systemSettings(a.ctx)
	if err == nil {
		if loc, err := time.LoadLocation(settings.Timezone); err == nil {
			return loc
		}
	}
	return time.UTC
}
func (a *App) clockSnapshot() (clock.Quality, string, time.Time) {
	source, now, err := a.device.Clock(a.ctx)
	if err != nil {
		return clock.Unknown, "unknown", time.Now()
	}
	switch source {
	case "ntp", "manual":
		return clock.Exact, source, now
	case "restored":
		return clock.Coarse, source, now
	default:
		return clock.Unknown, "unknown", now
	}
}
func (a *App) clockQuality() (clock.Quality, string) {
	q, source, _ := a.clockSnapshot()
	return q, source
}
func (a *App) SetClock(t time.Time) error {
	if err := a.device.SetTime(a.ctx, t); err != nil {
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
	settings, err := a.systemSettings(ctx)
	if err != nil {
		return
	}
	a.rabbit.SetSettings(settings.Locale, n)
}

// resync restores product settings after hardware reconnects.
func (a *App) resync() {
	a.agent.mu.Lock()
	blocked := a.agent.recovering || a.agent.held
	a.agent.mu.Unlock()
	if blocked {
		return
	}
	a.stopMedia()
	a.restoreRabbit()
}

func (a *App) restoreRabbit() {
	st := a.store.Get()
	a.send(earsCommand(st.Ears[0], st.Ears[1]))
	a.mu.Lock()
	indicator := a.indicator
	a.mu.Unlock()
	a.send(rabbit.Command{Action: rabbit.Indicator, Animation: indicators[indicator]})
	a.pushInfos()
	kick(a.airKick)
	kick(a.clockKick)
}

func (a *App) onState(s rabbit.State) {
	a.mu.Lock()
	if s.State != "playing" {
		asleep := s.State == "asleep"
		a.clk.Asleep = &asleep
	}
	a.mu.Unlock()
	// ponytail: coalesce state snapshots; queue transitions if every one is needed.
	kick(a.haKick)
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
	if !a.rabbit.EventCurrent(e.Event) {
		return
	}
	if e.Kind == "button" {
		a.ha.Button(e.Button)
		if e.Button == "down" {
			a.auth.MarkPresence(e.EdgeMonotonicNS)
		}
		if e.Button == "double_click_and_hold" {
			a.stopInteraction()
			a.serviceError("admin", a.auth.Reset())
			return
		}
	}
	if a.interactionEvent(e) {
		return
	}
	switch e.Kind {
	case "button":
		switch e.Button {
		case "click":
			if a.voiceCommandForClick() {
				return
			}
			a.setOverride(false)
		case "hold":
			a.device.VoiceCommand(a.ctx, "start_listening")
		case "triple_click":
			if err := a.device.PowerOff(a.ctx); err != nil {
				slog.Error("power off", "err", err)
			}
		case "click_and_hold":
			if a.store.Get().Services.Eightball {
				a.serviceError("eightball", a.startInteraction("eightball", ""))
			}
		}
	case "ears":
		a.store.Update(func(s *config.Settings) error {
			if e.Left != nil {
				s.Ears[0] = int(*e.Left)
			}
			if e.Right != nil {
				s.Ears[1] = int(*e.Right)
			}
			return nil
		})
		a.mastodonEars(e.Left, e.Right)
	case "rfid":
		if e.Tag == nil {
			return
		}
		tag := tagFromHardware(*e.Tag)
		if !e.received.IsZero() {
			tag.Received = e.received
		}
		a.mu.Lock()
		a.lastTag = &tag
		a.mu.Unlock()
		a.ha.Tag(tag.payload())
		if tag.Removed {
			return
		}
		switch tag.App {
		case "clock":
			a.setOverride(!(len(tag.Data) > 0 && tag.Data[0] == 1))
		case "weather":
			day := 0
			if len(tag.Data) > 0 && tag.Data[0] == 2 {
				day = 1
			}
			go a.announceWeather(day)
		default:
			go a.serviceTag(tag)
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
		v, err := strconv.ParseUint(c.Value, 10, 32)
		if err != nil || v > 100 {
			return
		}
		revision, settings, err := a.device.ReadConfig(a.ctx)
		if err == nil {
			settings.Volume = uint32(v)
			_, err = a.device.UpdateConfig(a.ctx, revision, settings)
		}
		a.serviceError("volume", err)
	case "left_ear", "right_ear":
		var v int
		fmt.Sscan(c.Value, &v)
		st, err := a.store.Update(func(s *config.Settings) error {
			s.Ears[map[string]int{"left_ear": 0, "right_ear": 1}[c.Name]] = v
			return nil
		})
		if err == nil {
			a.send(earsCommand(st.Ears[0], st.Ears[1]))
		}
	}
}

// Clock

func (a *App) chime(hour int) {
	a.send(rabbit.Command{Action: rabbit.Message, Cancelable: true,
		Signature: &rabbit.Item{Audio: []string{"clock/signature.mp3"}},
		Body:      []rabbit.Item{{Audio: []string{fmt.Sprintf("clock/%d/*.mp3", hour)}}},
	})
}

var errClockUntrusted = errors.New("l'heure du lapin n'est pas fiable : connectez-le à Internet ou réglez l'heure dans Réglages")

// sayTime announces the nearest hour, only when the clock is exact.
func (a *App) sayTime() error {
	q, _, now := a.clockSnapshot()
	if q != clock.Exact {
		return errClockUntrusted
	}
	now = now.In(a.location())
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
		_, _, now := a.clockSnapshot()
		a.clockTick(now.In(a.location()))
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
				a.playOwned(c, sequence("sleep/*.mp3", ""))
				stop()
			}
			a.send(rabbit.Command{Action: rabbit.Sleep})
		}()
	}
	if act.Wakeup {
		a.send(rabbit.Command{Action: rabbit.Wakeup})
		if sounds {
			a.send(rabbit.Command{Action: rabbit.Play, Cancelable: true, Sequence: []rabbit.Item{{Audio: []string{"wakeup/*.mp3"}}}})
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
	a.send(rabbit.Command{Action: rabbit.Info, InfoID: "weather", Animation: wi})
	a.send(rabbit.Command{Action: rabbit.Info, InfoID: "weather_rain", Animation: ri})
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
	a.send(weather.Message(a.store.Get().Weather, f, day))
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
		n, err := a.device.Connectivity(ctx)
		if err != nil {
			n = "offline"
		}
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

// Voice lifecycle belongs to device-core; only product LED/button policy lives here.
func (a *App) voiceSupported() bool {
	supported, err := a.device.VoiceSupported(a.ctx)
	return err == nil && supported
}
func (a *App) voiceEnabled() bool {
	settings, err := a.systemSettings(a.ctx)
	return err == nil && settings.VoiceEnabled
}
func (a *App) SetVoice(on bool) error { return a.device.EnableVoice(a.ctx, on) }

var indicators = map[string]*rabbit.Animation{
	"listening": {Tempo: 50, Frames: [][3]rabbit.RGB{{{0, 0, 255}, {0, 0, 255}, {0, 0, 255}}}},
	"thinking":  {Tempo: 25, Frames: [][3]rabbit.RGB{{{128, 0, 255}, {}, {128, 0, 255}}, {{}, {128, 0, 255}, {}}}},
	"speaking":  {Tempo: 20, Frames: [][3]rabbit.RGB{{{}, {0, 255, 0}, {}}, {{0, 255, 0}, {0, 255, 0}, {0, 255, 0}}}},
	"error":     {Tempo: 15, Frames: [][3]rabbit.RGB{{{255, 0, 0}, {255, 0, 0}, {255, 0, 0}}, {}}},
	"timer":     {Tempo: 25, Frames: [][3]rabbit.RGB{{{255, 128, 0}, {}, {255, 128, 0}}, {{}, {255, 128, 0}, {}}}},
}

func (a *App) setIndicator(name string) {
	a.mu.Lock()
	same := a.indicator == name
	a.indicator = name
	a.mu.Unlock()
	if !same {
		a.send(rabbit.Command{Action: rabbit.Indicator, Animation: indicators[name]})
	}
}

func (a *App) voiceEvent(event string, data map[string]any) {
	if event == "status" {
		event = str(data, "status")
	}
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
	case "idle", "tts_finished", "disconnected", "snapshot", "disabled", "unsupported", "connecting", "muted", "media_player_playing":
		a.setIndicator("")
	}
}

// voiceCommandForClick sends the context command of LVA's action button and
// reports whether the click was consumed.
func (a *App) voiceCommandForClick() bool {
	state, err := a.device.VoiceState(a.ctx)
	if err != nil {
		return false
	}
	command := ""
	switch state {
	case "timer_ringing":
		command = "stop_timer_ringing"
	case "wake_word_detected", "listening", "thinking", "tts_speaking":
		command = "stop_pipeline"
	case "media_player_playing":
		command = "stop_media_player"
	}
	return command != "" && a.device.VoiceCommand(a.ctx, command) == nil
}
