package main

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/guilhem/nabos/services/internal/config"
	"github.com/guilhem/nabos/services/internal/pynab"
)

func (a *App) serviceRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /services", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		errs := make(map[string]string, len(a.serviceErrors))
		for name, message := range a.serviceErrors {
			errs[name] = message
		}
		a.mu.Unlock()
		a.render(w, r, "services", "Services", map[string]any{
			"Errors": errs, "Books": pynab.Books(a.env.SoundsDirs),
			"Languages": pynab.Languages, "SurpriseKinds": pynab.SurpriseKinds,
		})
	})
	m.HandleFunc("POST /services/settings", a.saveServices)
	m.HandleFunc("POST /services/action", a.serviceAction)
	m.HandleFunc("POST /tags/remove", a.removeTag)
	a.mastodonRoutes(m)
}

func (a *App) saveServices(w http.ResponseWriter, r *http.Request) {
	f := r.FormValue
	var old config.Services
	st, err := a.store.Update(func(s *config.Settings) error {
		c := &s.Services
		old = *c
		var e error
		if c.TaichiFrequency, e = atoi(f("taichi_frequency")); e != nil {
			return e
		}
		if c.SurpriseFrequency, e = atoi(f("surprise_frequency")); e != nil {
			return e
		}
		if c.TaichiFrequency != old.TaichiFrequency {
			c.NextTaichi = time.Time{}
		}
		if c.SurpriseFrequency != old.SurpriseFrequency {
			c.NextSurprise = time.Time{}
		}
		c.Eightball, c.Books, c.Radio = f("eightball") == "on", f("books") == "on", f("radio") == "on"
		c.Webhooks, c.IFTTT = f("webhooks") == "on", f("ifttt") == "on"
		c.AirQuality.Enabled = f("air_enabled") == "on"
		c.AirQuality.Index, c.AirQuality.Visual = f("air_index"), f("air_visual")
		if f("clear_ifttt_key") == "on" {
			c.IFTTTKey = ""
		} else if v := f("ifttt_key"); v != "" {
			c.IFTTTKey = v
		}
		if f("clear_air_token") == "on" {
			c.AirQuality.Token = ""
		} else if v := f("air_token"); v != "" {
			c.AirQuality.Token = v
		}
		return nil
	})
	if err != nil {
		back(w, r, "/services", err, "")
		return
	}
	if !st.Services.Books || !st.Services.Eightball {
		a.mu.Lock()
		active := a.interaction
		a.mu.Unlock()
		if active != nil && (active.kind == "book" && !st.Services.Books || active.kind == "eightball" && !st.Services.Eightball) {
			active.cancel()
		}
	}
	if !st.Services.Radio {
		a.stopRadio()
	}
	if old != st.Services {
		kick(a.airKick)
	}
	back(w, r, "/services", nil, "Services enregistrés")
}

func (a *App) serviceAction(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	s := a.store.Get()
	var err error
	switch name {
	case "taichi", "surprise", "eightball", "airquality":
		if name == "eightball" && !s.Services.Eightball || name == "airquality" && !s.Services.AirQuality.Enabled {
			err = errors.New("service désactivé")
		} else {
			language, kind := r.FormValue("language"), r.FormValue("surprise_kind")
			if language != "" && !contains(pynab.Languages, language) {
				err = errors.New("langue inconnue")
			}
			if name == "surprise" && kind != "" && !contains(pynab.SurpriseKinds, kind) {
				err = errors.New("type de surprise inconnu")
			}
			if err == nil {
				err = a.performService(r.Context(), name, language, kind)
			}
		}
	case "book":
		if !s.Services.Books {
			err = errors.New("livres désactivés")
			break
		}
		data, e := pynab.BookTag(r.FormValue("voice"), r.FormValue("isbn"))
		if e != nil {
			err = e
			break
		}
		if !pynab.ChapterExists(a.env.SoundsDirs, r.FormValue("voice"), strings.ReplaceAll(r.FormValue("isbn"), "-", ""), 1) {
			err = errors.New("livre ou voix non installé")
			break
		}
		err = a.startInteraction("book", data)
	case "radio", "ifttt", "webhook":
		if name == "radio" && !s.Services.Radio || name == "ifttt" && !s.Services.IFTTT || name == "webhook" && !s.Services.Webhooks {
			err = errors.New("service désactivé")
			break
		}
		uid := r.FormValue("uid")
		association, ok := s.Tags[uid]
		if !ok || association.App != name {
			err = errors.New("association introuvable")
			break
		}
		go a.serviceTag(map[string]any{"app": name, "uid": uid})
	default:
		err = errors.New("service inconnu")
	}
	back(w, r, "/services", err, "Action lancée")
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func (a *App) removeTag(w http.ResponseWriter, r *http.Request) {
	uid := r.FormValue("uid")
	_, err := a.store.Update(func(s *config.Settings) error {
		if _, ok := s.Tags[uid]; !ok {
			return errors.New("association inconnue")
		}
		delete(s.Tags, uid)
		return nil
	})
	back(w, r, "/tags", err, "Association supprimée")
}
