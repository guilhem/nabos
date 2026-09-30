package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/guilhem/nabos/services/internal/config"
	"github.com/guilhem/nabos/services/internal/hardware"
	"github.com/guilhem/nabos/services/internal/pynab"
	"github.com/guilhem/nabos/services/internal/rabbit"
)

type appEvent struct {
	rabbit.Event
	received time.Time
}

// Product names and colon-separated UIDs stay at the app/HA boundary.
type productTag struct {
	Removed                       bool
	Tech, UID, App, Data, Support string
	RawUID                        []byte
	Locked, Formatted             bool
	Picture                       uint8
	Received                      time.Time
}

func tagFromHardware(tag hardware.Tag) productTag {
	uid := make([]string, len(tag.UID))
	for i, b := range tag.UID {
		uid[i] = fmt.Sprintf("%02x", b)
	}
	data := tag.Data
	if end := bytes.IndexByte(data, 0xff); end >= 0 {
		data = data[:end]
	}
	app := ""
	if tag.Formatted && tag.App != 255 {
		app = pynab.TagApp(tag.App)
	}
	return productTag{Removed: tag.Removed, Tech: tag.Tech, UID: strings.Join(uid, ":"), RawUID: append([]byte(nil), tag.UID...), App: app, Data: strings.ToValidUTF8(string(data), "\ufffd"), Support: tag.Support, Locked: tag.Locked, Formatted: tag.Formatted, Picture: tag.Picture, Received: time.Now()}
}
func (tag productTag) payload() map[string]any {
	event := "detected"
	if tag.Removed {
		event = "removed"
	}
	payload := map[string]any{"event": event, "tech": tag.Tech, "uid": tag.UID, "v": 1, "time": float64(tag.Received.UnixNano()) / 1e9}
	if !tag.Removed {
		payload["support"], payload["locked"] = tag.Support, tag.Locked
		if tag.Formatted {
			payload["picture"] = tag.Picture
		}
		if tag.App != "" {
			payload["app"], payload["data"] = tag.App, tag.Data
		}
	}
	return payload
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
		a.agent.mu.Lock()
		blocked := a.agent.recovering || a.agent.held
		a.agent.mu.Unlock()
		if blocked {
			<-a.mediaGate
			return errors.New("maintenance en cours")
		}
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

// Results are not retained: a lost core cannot finish any of its old media.
func (a *App) stopMedia() {
	a.stopInteraction()
	a.stopRadio()
	a.mu.Lock()
	cancel := a.mediaCancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// All media passes through this gate. Interactive sequences keep it between
// chapters, so a chime cannot slip between a book's cancellation and next page.
func (a *App) media(ctx context.Context, command rabbit.Command, wait time.Duration) error {
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
	return a.playOwned(ctx, command)
}

func (a *App) cancelMedia(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := a.rabbit.Do(ctx, rabbit.Command{Action: rabbit.Cancel, Target: id, Deadline: time.Now().Add(3 * time.Second)})
	if err == nil {
		err = result.Err()
	}
	if err != nil {
		slog.Debug("cancel command", "err", err)
	}
	return err
}

func (a *App) playOwned(ctx context.Context, command rabbit.Command) error {
	// Callers hold mediaGate, so this handle belongs to the only active play.
	ctx, cancel := context.WithCancel(ctx)
	a.mu.Lock()
	a.mediaCancel = cancel
	a.mu.Unlock()
	defer func() {
		cancel()
		a.mu.Lock()
		a.mediaCancel = nil
		a.mu.Unlock()
	}()
	command.ID, command.Deadline = rabbit.NewID(), time.Now().Add(cmdTTL)
	r, err := a.rabbit.Do(ctx, command)
	if err != nil {
		a.cancelMedia(command.ID)
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
		case <-ticker.C:
			a.servicesTick(a.clockNow().In(a.location()))
			if !a.rabbit.Ready() {
				a.stopMedia()
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
		state, initialized := a.rabbit.State()
		// Expired jobs are not replayed in a burst after sleep or power loss.
		if !job.next.IsZero() && initialized && a.rabbit.Ready() && state.State == "idle" && now.Sub(job.next) < time.Minute {
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
		return a.media(ctx, rabbit.Command{Action: rabbit.Play, Cancelable: true, Sequence: []rabbit.Item{{Choreography: "taichi/taichi.chor"}}}, 5*time.Minute)
	case "surprise":
		return a.media(ctx, pynab.Surprise(a.clockNow().In(a.location()), language, kind), 5*time.Minute)
	case "eightball":
		if !s.Eightball {
			return errors.New("boule magique désactivée")
		}
		return a.media(ctx, pynab.Eightball(language), time.Minute)
	case "airquality":
		return a.announceAirQuality(ctx)
	}
	return errors.New("service inconnu")
}

func (a *App) serviceTag(tag productTag) {
	app, data := tag.App, tag.Data
	s := a.store.Get()
	var err error
	switch app {
	case "taichi", "surprise", "eightball", "airquality":
		language, kind := pynab.Decode(app, data)
		err = a.performService(a.ctx, app, language, kind)
	case "book":
		err = a.startInteraction("book", data)
	case "radio", "ifttt", "webhook":
		association, ok := s.Tags[tag.UID]
		if !ok || association.App != app {
			err = errors.New("étiquette non configurée sur ce lapin")
			break
		}
		err = a.performTagAction(a.ctx, app, tag.UID, association.Value)
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
	if e.Kind == "ears" || e.Kind == "ear_moved" && e.received.Before(s.earsAfter) {
		return true
	}
	if e.Kind != "button" && e.Kind != "ear_moved" {
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
	state, initialized := a.rabbit.State()
	if !initialized || !a.rabbit.Ready() || state.State != "idle" {
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
		a.send(earsCommand(ears[0], ears[1]))
		a.serviceError(kind, err)
	}()
	return nil
}

func sequence(audio, chor string) rabbit.Command {
	item := rabbit.Item{Choreography: chor}
	if audio != "" {
		item.Audio = []string{audio}
	}
	return rabbit.Command{Action: rabbit.Play, Sequence: []rabbit.Item{item}}
}

// interactivePlay returns click, left/right, or up (eightball); targeted
// cancellation waits for the old playback before the caller advances a chapter.
func (a *App) interactivePlay(s *interaction, command rabbit.Command, navigate bool) (string, error) {
	id := rabbit.NewID()
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	done := make(chan error, 1)
	published := make(chan struct{})
	go func() {
		command.ID, command.Deadline = id, time.Now().Add(cmdTTL)
		r, err := a.rabbit.DoStarted(ctx, command, published)
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
			if !a.rabbit.EventCurrent(e.Event) {
				continue
			}
			control := ""
			if e.Kind == "button" {
				if e.Button == "click" {
					control = "click"
				}
				if s.kind == "eightball" && e.Button == "up" {
					control = "up"
				}
			} else if navigate && e.Kind == "ear_moved" {
				control = "left"
				if e.Ear == 1 {
					control = "right"
				}
			}
			if control != "" {
				if err := a.cancelMedia(id); err != nil {
					return "", err
				}
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
				if !a.rabbit.EventCurrent(e.Event) {
					continue
				}
				if e.Kind == "button" && e.Button == "up" {
					break wait
				}
			}
		}
	}
	if s.ctx.Err() != nil {
		return s.ctx.Err()
	}
	if err = a.playOwned(s.ctx, sequence("eightball/acquired.mp3", "")); err != nil {
		return err
	}
	return a.playOwned(s.ctx, pynab.Eightball("default"))
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
		return a.playOwned(s.ctx, sequence("system/abort.wav", ""))
	}
	chapter := 1
	for pynab.ChapterExists(a.env.SoundsDirs, voice, isbn, chapter) {
		control, err = a.interactivePlay(s, sequence(fmt.Sprintf("book/books/%s/%s/%d.mp3", isbn, voice, chapter), ""), true)
		if err != nil {
			return err
		}
		switch control {
		case "click":
			_, err = a.interactivePlay(s, rabbit.Command{Action: rabbit.Play, Sequence: []rabbit.Item{
				{Audio: []string{"system/abort.wav"}},
				{Audio: []string{"fr_FR/book/interrupt.mp3"}, Choreography: "fr_FR/book/interrupt.chor"},
			}}, false)
			return err
		case "left":
			chapter = max(1, chapter-1)
			if err = a.playOwned(s.ctx, sequence("book/previous.mp3", "")); err != nil {
				return err
			}
		case "right":
			chapter++
			if err = a.playOwned(s.ctx, sequence("book/next.mp3", "")); err != nil {
				return err
			}
		default:
			chapter++
		}
		if control == "left" || control == "right" {
			// PyNab ignores gestures during cancellation/navigation feedback.
			// Discard both delivered gestures and those still queued by the hardware event loop.
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
		return a.playOwned(s.ctx, sequence("system/abort.wav", ""))
	}
	return err
}
