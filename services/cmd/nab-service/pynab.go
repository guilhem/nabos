package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/guilhem/nabos/services/internal/bus"
	"github.com/guilhem/nabos/services/internal/config"
	"github.com/guilhem/nabos/services/internal/pynab"
)

type appEvent struct {
	kind     string
	data     map[string]any
	received time.Time
}
type interaction struct {
	kind      string
	events    chan appEvent
	ctx       context.Context
	cancel    context.CancelFunc
	earsAfter time.Time // guarded by App.mu
}

func (a *App) eventLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-a.events:
			a.onEvent(e)
		}
	}
}

func (a *App) serviceError(name string, err error) {
	a.mu.Lock()
	if err == nil {
		delete(a.serviceErrors, name)
	} else if !errors.Is(err, context.Canceled) {
		a.serviceErrors[name] = err.Error()
	}
	a.mu.Unlock()
	if err != nil && !errors.Is(err, context.Canceled) {
		slog.Warn("service", "name", name, "err", err)
	}
}

func (a *App) acquireMedia(ctx context.Context) error {
	select {
	case a.mediaGate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *App) stopRadio() {
	a.mu.Lock()
	cancel := a.radioCancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// All media passes through this gate. Interactive sequences keep it between
// chapters, so a chime cannot slip between a book's cancellation and next page.
func (a *App) media(ctx context.Context, action string, args any, wait time.Duration) error {
	a.stopRadio()
	queueCtx, cancel := context.WithTimeout(ctx, cmdTTL)
	err := a.acquireMedia(queueCtx)
	cancel()
	if err != nil {
		return err
	}
	defer func() { <-a.mediaGate }()
	ctx, cancel = context.WithTimeout(ctx, wait)
	defer cancel()
	return a.playOwned(ctx, action, args)
}

func (a *App) cancelMedia(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := a.bus.Do(ctx, "cancel", map[string]any{"target": id}, 3*time.Second)
	if err != nil {
		slog.Debug("cancel command", "err", err)
	}
}

func (a *App) playOwned(ctx context.Context, action string, args any) error {
	id := bus.NewID()
	r, err := a.bus.DoID(ctx, id, action, args, cmdTTL, nil)
	if err != nil {
		a.cancelMedia(id)
		return err
	}
	return r.Err()
}

func (a *App) servicesLoop(ctx context.Context) {
	go a.airQualityLoop(ctx)
	go a.mastodonLoop(ctx)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			a.servicesTick(now.In(a.location()))
			connected, online := a.bus.Healthy()
			if !connected || !online {
				a.stopInteraction()
				a.stopRadio()
			}
		}
	}
}

func (a *App) servicesTick(now time.Time) {
	quality, _ := a.clockQuality()
	if quality == 0 {
		return
	}
	s := a.store.Get().Services
	for _, job := range []struct {
		name      string
		frequency int
		next      time.Time
	}{
		{"taichi", s.TaichiFrequency, s.NextTaichi}, {"surprise", s.SurpriseFrequency, s.NextSurprise},
	} {
		if job.frequency == 0 || (!job.next.IsZero() && now.Before(job.next)) {
			continue
		}
		next := now.Add(pynab.Delay(job.name, job.frequency, rand.Float64()))
		_, err := a.store.Update(func(c *config.Settings) error {
			if job.name == "taichi" {
				c.Services.NextTaichi = next
			} else {
				c.Services.NextSurprise = next
			}
			return nil
		})
		if err != nil {
			a.serviceError(job.name, err)
			continue
		}
		state, online := a.bus.State()
		// Expired jobs are not replayed in a burst after sleep or power loss.
		if !job.next.IsZero() && online && state.State == "idle" && now.Sub(job.next) < time.Minute {
			a.mu.Lock()
			busy := a.interaction != nil || a.radioCancel != nil
			a.mu.Unlock()
			if !busy {
				go func(name string) { a.serviceError(name, a.performService(a.ctx, name, "", "")) }(job.name)
			}
		}
	}
}

func (a *App) performService(ctx context.Context, name, language, kind string) error {
	s := a.store.Get().Services
	switch name {
	case "taichi":
		return a.media(ctx, "play", map[string]any{"sequence": []any{map[string]any{"choreography": "taichi/taichi.chor"}}}, 5*time.Minute)
	case "surprise":
		return a.media(ctx, "message", pynab.Surprise(time.Now().In(a.location()), language, kind), 5*time.Minute)
	case "eightball":
		if !s.Eightball {
			return errors.New("boule magique désactivée")
		}
		return a.media(ctx, "message", pynab.Eightball(language), time.Minute)
	case "airquality":
		return a.announceAirQuality(ctx)
	}
	return errors.New("service inconnu")
}

func (a *App) serviceTag(tag map[string]any) {
	app, data := str(tag, "app"), str(tag, "data")
	s := a.store.Get()
	var err error
	switch app {
	case "taichi", "surprise", "eightball", "airquality":
		language, kind := pynab.Decode(app, data)
		err = a.performService(a.ctx, app, language, kind)
	case "book":
		err = a.startInteraction("book", data)
	case "radio", "ifttt", "webhook":
		association, ok := s.Tags[str(tag, "uid")]
		if !ok || association.App != app {
			err = errors.New("étiquette non configurée sur ce lapin")
			break
		}
		err = a.performTagAction(a.ctx, app, str(tag, "uid"), association.Value)
	default:
		return
	}
	a.serviceError(app, err)
}

func (a *App) stopInteraction() bool {
	a.mu.Lock()
	s := a.interaction
	a.mu.Unlock()
	if s != nil {
		s.cancel()
		return true
	}
	return false
}

func (a *App) interactionEvent(e appEvent) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.interaction
	if s == nil {
		return false
	}
	if e.kind == "ears" || e.kind == "ear_moved" && e.received.Before(s.earsAfter) {
		return true
	}
	if e.kind != "button" && e.kind != "ear_moved" {
		return false
	}
	select {
	case s.events <- e:
	default:
		s.cancel()
	}
	return true
}

// Reserve the session before returning, so the next button event (notably up)
// cannot overtake the goroutine that runs the interaction.
func (a *App) startInteraction(kind, data string) error {
	cfg := a.store.Get().Services
	if kind == "book" && !cfg.Books || kind == "eightball" && !cfg.Eightball {
		return errors.New("service désactivé")
	}
	if kind == "book" {
		voice, isbn, err := pynab.ParseBook(data)
		if err != nil {
			return err
		}
		if !pynab.ChapterExists(a.env.SoundsDirs, voice, isbn, 1) {
			return errors.New("livre ou voix non installé")
		}
	}
	a.stopRadio()
	ctx, cancel := context.WithTimeout(a.ctx, 3*time.Second)
	err := a.acquireMedia(ctx)
	cancel()
	if err != nil {
		return errors.New("lapin occupé")
	}
	state, online := a.bus.State()
	if !online || state.State != "idle" {
		<-a.mediaGate
		return errors.New("lapin occupé ou endormi")
	}
	ctx, cancel = context.WithCancel(a.ctx)
	s := &interaction{kind: kind, ctx: ctx, cancel: cancel, events: make(chan appEvent, 32)}
	a.mu.Lock()
	a.interaction = s
	a.mu.Unlock()
	go func() {
		defer func() { <-a.mediaGate }()
		var err error
		if kind == "book" {
			err = a.readBook(s, data)
		} else {
			err = a.askEightball(s)
		}
		cancel()
		a.mu.Lock()
		a.interaction = nil
		a.mu.Unlock()
		ears := a.store.Get().Ears
		a.send("ears", map[string]any{"left": ears[0], "right": ears[1]})
		a.serviceError(kind, err)
	}()
	return nil
}

func sequence(audio, chor string) map[string]any {
	item := map[string]any{}
	if audio != "" {
		item["audio"] = []string{audio}
	}
	if chor != "" {
		item["choreography"] = chor
	}
	return map[string]any{"sequence": []any{item}, "cancelable": false}
}

// interactivePlay returns click, left/right, or up (eightball); targeted
// cancellation waits for the old playback before the caller advances a chapter.
func (a *App) interactivePlay(s *interaction, args map[string]any, navigate bool) (string, error) {
	id := bus.NewID()
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	done := make(chan error, 1)
	published := make(chan struct{})
	go func() {
		r, err := a.bus.DoID(ctx, id, "play", args, cmdTTL, published)
		if err == nil {
			err = r.Err()
		}
		done <- err
	}()
	select {
	case <-published:
	case err := <-done:
		if ctx.Err() != nil {
			a.cancelMedia(id)
		}
		return "", err
	}
	for {
		select {
		case err := <-done:
			if ctx.Err() != nil {
				a.cancelMedia(id)
			}
			return "", err
		case <-s.ctx.Done():
			a.cancelMedia(id)
			return "", s.ctx.Err()
		case e := <-s.events:
			control := ""
			if e.kind == "button" {
				if str(e.data, "event") == "click" {
					control = "click"
				}
				if s.kind == "eightball" && str(e.data, "event") == "up" {
					control = "up"
				}
			} else if navigate && e.kind == "ear_moved" {
				control = str(e.data, "ear")
			}
			if control != "" {
				a.cancelMedia(id)
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					return "", errors.New("annulation de lecture sans réponse")
				}
				return control, nil
			}
		}
	}
}

func (a *App) askEightball(s *interaction) error {
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	listening := *s
	listening.ctx = ctx
	control, err := a.interactivePlay(&listening, sequence("eightball/listen.mp3", "eightball/listen.chor"), false)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if control != "up" && ctx.Err() == nil {
	wait:
		for {
			select {
			case <-ctx.Done():
				break wait
			case e := <-s.events:
				if e.kind == "button" && str(e.data, "event") == "up" {
					break wait
				}
			}
		}
	}
	if s.ctx.Err() != nil {
		return s.ctx.Err()
	}
	if err = a.playOwned(s.ctx, "play", sequence("eightball/acquired.mp3", "")); err != nil {
		return err
	}
	return a.playOwned(s.ctx, "message", pynab.Eightball("default"))
}

func (a *App) readBook(s *interaction, data string) error {
	voice, isbn, err := pynab.ParseBook(data)
	if err != nil {
		return err
	}
	if !pynab.ChapterExists(a.env.SoundsDirs, voice, isbn, 1) {
		return errors.New("livre ou voix non installé")
	}
	// PyNab only ships these spoken prompts and choreographies in French.
	control, err := a.interactivePlay(s, sequence("fr_FR/book/intro.mp3", "fr_FR/book/intro.chor"), false)
	if err != nil {
		return err
	}
	if control == "click" {
		return a.playOwned(s.ctx, "play", sequence("system/abort.wav", ""))
	}
	chapter := 1
	for pynab.ChapterExists(a.env.SoundsDirs, voice, isbn, chapter) {
		control, err = a.interactivePlay(s, sequence(fmt.Sprintf("book/books/%s/%s/%d.mp3", isbn, voice, chapter), ""), true)
		if err != nil {
			return err
		}
		switch control {
		case "click":
			_, err = a.interactivePlay(s, map[string]any{"cancelable": false, "sequence": []any{
				map[string]any{"audio": []string{"system/abort.wav"}},
				map[string]any{"audio": []string{"fr_FR/book/interrupt.mp3"}, "choreography": "fr_FR/book/interrupt.chor"},
			}}, false)
			return err
		case "left":
			chapter = max(1, chapter-1)
			if err = a.playOwned(s.ctx, "play", sequence("book/previous.mp3", "")); err != nil {
				return err
			}
		case "right":
			chapter++
			if err = a.playOwned(s.ctx, "play", sequence("book/next.mp3", "")); err != nil {
				return err
			}
		default:
			chapter++
		}
		if control == "left" || control == "right" {
			// PyNab ignores gestures during cancellation/navigation feedback.
			// Discard both delivered gestures and those still queued by MQTT.
			a.mu.Lock()
			s.earsAfter = time.Now()
			for len(s.events) > 0 {
				<-s.events
			}
			a.mu.Unlock()
		}
	}
	outro := "outro-noalt"
	for _, b := range pynab.Books(a.env.SoundsDirs) {
		if b.ISBN == isbn && len(b.Voices) > 1 {
			outro = "outro-alt"
		}
	}
	control, err = a.interactivePlay(s, sequence("fr_FR/book/"+outro+".mp3", "fr_FR/book/"+outro+".chor"), false)
	if err == nil && control == "click" {
		return a.playOwned(s.ctx, "play", sequence("system/abort.wav", ""))
	}
	return err
}
