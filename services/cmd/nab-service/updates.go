package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/guilhem/nabos/services/internal/clock"
	"github.com/guilhem/nabos/services/internal/config"
	"github.com/guilhem/nabos/services/internal/update"
)

// A window is identified by its local start date, including across midnight
// and the repeated hour at the end of daylight saving time.
func updateWindow(u config.Updates, now time.Time) string {
	start, end := u.Start.Hour*60+u.Start.Min, u.End.Hour*60+u.End.Min
	minute := now.Hour()*60 + now.Minute()
	if start == end || start < end && (minute < start || minute >= end) || start > end && minute >= end && minute < start {
		return ""
	}
	if start > end && minute < end {
		now = now.AddDate(0, 0, -1)
	}
	return now.Format("2006-01-02")
}

func (a *App) updatePolicy(now time.Time, channel string) error {
	s := a.store.Get()
	switch {
	case !s.AutoCheck || !s.Updates.Automatic:
		return errors.New("installation automatique désactivée")
	case s.Updates.Channel != channel:
		return errors.New("canal modifié : redémarrage ou installation à confirmer manuellement")
	}
	if q, _ := a.clockQuality(); q != clock.Exact {
		return errors.New("en attente d’une heure fiable")
	}
	if updateWindow(s.Updates, now.In(a.location())) == "" {
		return fmt.Errorf("en attente du créneau %02d:%02d–%02d:%02d (%s)", s.Updates.Start.Hour, s.Updates.Start.Min, s.Updates.End.Hour, s.Updates.End.Min, s.Timezone)
	}
	return nil
}

// Keep the existing media gate while checking inactivity and requesting reboot.
// In particular, a book's silence between chapters is not an idle rabbit.
func (a *App) withIdleUpdate(fn func() error) error {
	select {
	case a.mediaGate <- struct{}{}:
		defer func() { <-a.mediaGate }()
	default:
		return errors.New("en attente : lecture ou interaction en cours")
	}
	connected, online := a.bus.Healthy()
	state, _ := a.bus.State()
	if !connected || !online || state.Playing != nil || state.State != "idle" && state.State != "asleep" {
		return errors.New("en attente : lapin occupé ou indisponible")
	}
	a.mu.Lock()
	busy, voice := a.interaction != nil || a.radioCancel != nil || a.mediaCancel != nil, a.voice
	a.mu.Unlock()
	if busy || voice != nil && voice.State() != "idle" {
		return errors.New("en attente : lecture ou assistant vocal actif")
	}
	return fn()
}

func (a *App) proposedUpdate() string {
	for _, rel := range a.upd.Releases(a.store.Get().Updates.Channel) {
		if rel.Ready && rel.Blocked == "" {
			return rel.Tag
		}
	}
	return ""
}

func (a *App) startUpdate(tag, channel string, automatic, retry bool, window string) error {
	a.mu.Lock()
	if a.updateBusy {
		a.mu.Unlock()
		return errors.New("une mise à jour est déjà en cours")
	}
	a.updateBusy, a.updateError = true, ""
	a.mu.Unlock()
	finish := func(err error) {
		a.mu.Lock()
		a.updateBusy = false
		if err != nil {
			a.updateError = err.Error()
		}
		a.mu.Unlock()
	}
	if automatic {
		claimed, err := a.upd.ClaimWindow(window)
		if err != nil || !claimed {
			finish(err)
			return err
		}
	}
	go func() {
		err := a.upd.InstallRelease(a.ctx, tag, update.InstallOptions{
			Channel: channel, Automatic: automatic, Retry: retry,
			BeforeInstall: func() error {
				if a.store.Get().Updates.Channel != channel {
					return errors.New("le canal a changé : relancez l’installation")
				}
				if automatic {
					return a.withIdleUpdate(func() error { return a.updatePolicy(time.Now(), channel) })
				}
				return nil
			},
		})
		if err != nil {
			slog.Error("update installation", "tag", tag, "err", err)
		}
		finish(err)
	}()
	return nil
}

func (a *App) updateTick(now time.Time) {
	if !a.upd.Configured() {
		return
	}
	if err := a.upd.Reconcile(); err != nil {
		slog.Debug("update recovery", "err", err)
		return
	}
	s := a.store.Get()
	st := a.upd.Status()
	a.mu.Lock()
	busy, rebooted := a.updateBusy, a.updateRebooted
	a.mu.Unlock()
	if busy || rebooted || st.Suspended || !s.Updates.Automatic {
		return
	}
	if st.State == "reboot" {
		if !st.PendingAuto || a.updatePolicy(now, st.PendingChannel) != nil {
			return
		}
		err := a.withIdleUpdate(func() error {
			if err := a.updatePolicy(now, st.PendingChannel); err != nil {
				return err
			}
			return a.rebootSystem()
		})
		if err != nil {
			slog.Debug("update reboot deferred", "err", err)
		} else {
			a.mu.Lock()
			a.updateRebooted = true
			a.mu.Unlock()
		}
		return
	}
	if st.State == "installing" || st.State == "downloading" || st.State == "confirming" || st.Checking || st.CheckError != "" || st.Checked.IsZero() || now.Sub(st.Checked) > 24*time.Hour {
		return
	}
	if err := a.updatePolicy(now, s.Updates.Channel); err != nil {
		return
	}
	tag := a.proposedUpdate()
	window := updateWindow(s.Updates, now.In(a.location()))
	if tag == "" || window == st.LastWindow {
		return
	}
	if err := a.withIdleUpdate(func() error { return nil }); err != nil {
		slog.Debug("automatic update deferred", "err", err)
		return
	}
	if err := a.startUpdate(tag, s.Updates.Channel, true, false, window); err != nil {
		slog.Warn("automatic update", "err", err)
	}
}

func (a *App) updateLoop(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	nextCheck := time.Now().Add(5 * time.Minute)
	a.updateTick(time.Now())
	for {
		forced := false
		select {
		case <-ctx.Done():
			return
		case <-a.updateKick:
			forced = true
		case <-tick.C:
		}
		now := time.Now()
		if a.upd.Configured() && (forced || a.store.Get().AutoCheck && !now.Before(nextCheck)) {
			if _, err := a.upd.Check(ctx); err != nil {
				slog.Warn("update check", "err", err)
			}
			nextCheck = now.Add(24 * time.Hour)
		}
		a.updateTick(time.Now())
	}
}

func updateStateLabel(state string) string {
	switch state {
	case "idle":
		return "Prêt"
	case "downloading":
		return "Téléchargement"
	case "installing":
		return "Installation"
	case "reboot":
		return "Redémarrage nécessaire"
	case "confirming":
		return "Vérification du nouveau démarrage"
	case "uncertain":
		return "Automatique suspendu : reprise manuelle nécessaire"
	case "error":
		return "Échec de la mise à jour"
	default:
		return state
	}
}

func (a *App) updateRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /updates", func(w http.ResponseWriter, r *http.Request) {
		st, settings := a.upd.Status(), a.store.Get()
		a.mu.Lock()
		busy, lastError := a.updateBusy, a.updateError
		a.mu.Unlock()
		waiting := ""
		if settings.Updates.Automatic {
			channel := settings.Updates.Channel
			if st.State == "reboot" && st.PendingAuto {
				channel = st.PendingChannel
			}
			if err := a.updatePolicy(time.Now(), channel); err != nil {
				waiting = err.Error()
			} else if err := a.withIdleUpdate(func() error { return nil }); err != nil {
				waiting = err.Error()
			} else if st.LastWindow == updateWindow(settings.Updates, time.Now().In(a.location())) && st.State != "reboot" {
				waiting = "Prochaine tentative au prochain créneau nocturne"
			}
		}
		active := busy || st.State == "installing" || st.State == "downloading" || st.State == "reboot" || st.State == "confirming"
		a.render(w, r, "updates", "Mises à jour", map[string]any{
			"Status": st, "StateLabel": updateStateLabel(st.State), "Configured": a.upd.Configured(),
			"Releases": a.upd.Releases(settings.Updates.Channel), "Busy": active,
			"Waiting": waiting, "LastError": lastError,
		})
	})
	m.HandleFunc("POST /updates/settings", a.saveUpdateSettings)
	m.HandleFunc("POST /updates/check", func(w http.ResponseWriter, r *http.Request) {
		_, err := a.upd.Check(r.Context())
		back(w, r, "/updates", err, "Vérification terminée")
	})
	m.HandleFunc("POST /updates/install", func(w http.ResponseWriter, r *http.Request) {
		err := a.startUpdate(r.FormValue("tag"), a.store.Get().Updates.Channel, false, r.FormValue("retry") == "true", "")
		back(w, r, "/updates", err, "Installation demandée ; le redémarrage restera manuel")
	})
}

func (a *App) saveUpdateSettings(w http.ResponseWriter, r *http.Request) {
	old := a.store.Get()
	s, err := a.store.Update(func(s *config.Settings) error {
		switch r.FormValue("mode") {
		case "manual":
			s.AutoCheck, s.Updates.Automatic = false, false
		case "notify":
			s.AutoCheck, s.Updates.Automatic = true, false
		case "auto":
			s.AutoCheck, s.Updates.Automatic = true, true
		default:
			return errors.New("mode de mise à jour invalide")
		}
		s.Updates.Channel = r.FormValue("channel")
		var err error
		if s.Updates.Start, err = parseHM(r.FormValue("start")); err != nil {
			return err
		}
		s.Updates.End, err = parseHM(r.FormValue("end"))
		return err
	})
	if err == nil && (s.AutoCheck || s.Updates.Channel != old.Updates.Channel) {
		kick(a.updateKick)
	}
	back(w, r, "/updates", err, "Réglages des mises à jour enregistrés")
}
