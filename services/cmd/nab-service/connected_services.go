package main

import (
	"context"
	"errors"
	"time"

	"github.com/guilhem/nabos/services/internal/airquality"
	"github.com/guilhem/nabos/services/internal/radio"
)

func (a *App) airQualityLoop(ctx context.Context) {
	for {
		a.refreshAirQuality(ctx, false)
		select {
		case <-ctx.Done():
			return
		case <-a.airKick:
		case <-time.After(30 * time.Minute):
		}
	}
}

func (a *App) refreshAirQuality(ctx context.Context, cached bool) (*airquality.Result, error) {
	a.aqMu.Lock()
	defer a.aqMu.Unlock()
	s := a.store.Get()
	cfg := s.Services.AirQuality
	q := airquality.Query{Token: cfg.Token, Index: cfg.Index, Located: s.Weather.Location != "", Latitude: s.Weather.Latitude, Longitude: s.Weather.Longitude}
	if !cfg.Enabled {
		a.airResult = nil
		a.airFetched = time.Time{}
		a.serviceError("airquality", nil)
		a.send("info", map[string]any{"info_id": "airquality", "animation": nil})
		return nil, errors.New("qualité de l'air désactivée")
	}
	if cached && a.airResult != nil && a.airQuery == q && time.Since(a.airFetched) < 15*time.Minute {
		return a.airResult, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	f, err := a.aq.Fetch(ctx, q)
	a.airResult = f
	a.airQuery = q
	a.airFetched = time.Now()
	a.send("info", map[string]any{"info_id": "airquality", "animation": airquality.Info(cfg.Visual, f)})
	a.serviceError("airquality", err)
	return f, err
}

func (a *App) announceAirQuality(ctx context.Context) error {
	if !a.store.Get().Services.AirQuality.Enabled {
		return errors.New("qualité de l'air désactivée")
	}
	f, fetchErr := a.refreshAirQuality(ctx, true)
	if err := a.media(ctx, "message", airquality.Message(f), time.Minute); err != nil {
		return err
	}
	return fetchErr
}

func (a *App) performTagAction(ctx context.Context, app, uid, value string) error {
	s := a.store.Get().Services
	switch app {
	case "webhook":
		if !s.Webhooks {
			return errors.New("webhooks désactivés")
		}
		return a.triggers.Webhook(ctx, value)
	case "ifttt":
		if !s.IFTTT {
			return errors.New("IFTTT désactivé")
		}
		return a.triggers.IFTTT(ctx, s.IFTTTKey, value, uid)
	case "radio":
		if !s.Radio {
			return errors.New("radio désactivée")
		}
		return a.playRadio(ctx, value)
	}
	return errors.New("application inconnue")
}

func (a *App) playRadio(ctx context.Context, station string) error {
	a.stopRadio()
	queueCtx, stopWait := context.WithTimeout(ctx, 3*time.Second)
	err := a.acquireMedia(queueCtx)
	stopWait()
	if err != nil {
		return errors.New("lapin occupé")
	}
	defer func() { <-a.mediaGate }()
	state, online := a.bus.State()
	if !online || state.State == "asleep" {
		return errors.New("lapin endormi ou indisponible")
	}
	ctx, cancel := context.WithCancel(ctx)
	a.mu.Lock()
	a.radioCancel = cancel
	a.mu.Unlock()
	defer func() { cancel(); a.mu.Lock(); a.radioCancel = nil; a.mu.Unlock() }()
	relay := radio.New(nil)
	defer relay.Close()
	u, err := relay.Start(ctx, station)
	if err != nil {
		return err
	}
	err = a.playOwned(ctx, "play", map[string]any{"sequence": []any{
		map[string]any{"audio": []string{"radio/*.mp3"}},
		map[string]any{"stream": u, "choreography": "urn:x-chor:streaming"},
		map[string]any{"audio": []string{"radio/*.mp3"}},
	}})
	if err != nil {
		return err
	}
	return relay.Err()
}
