package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/guilhem/nabos/services/internal/config"
	"github.com/guilhem/nabos/services/internal/pynab"
	"github.com/guilhem/nabos/services/internal/system"
	"github.com/guilhem/nabos/services/internal/web"
)

//go:embed ui.html
var uiFS embed.FS

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"hm":   func(h config.HM) string { return fmt.Sprintf("%02d:%02d", h.Hour, h.Min) },
	"json": func(v any) string { b, _ := json.MarshalIndent(v, "", "  "); return string(b) },
	"days": func() []string {
		return []string{"Lundi", "Mardi", "Mercredi", "Jeudi", "Vendredi", "Samedi", "Dimanche"}
	},
}).ParseFS(uiFS, "ui.html"))

var locales = []string{"fr_FR", "en_US", "en_GB", "de_DE", "es_ES", "it_IT", "ja_JP", "pt_BR"}

type page struct {
	Title, Flash, Error string
	Auth                bool
	App                 *App
	S                   config.Settings
	Data                map[string]any
}

func (a *App) render(w http.ResponseWriter, r *http.Request, name, title string, data map[string]any) {
	p := page{Title: title, App: a, Auth: a.auth.Valid(r), S: a.store.Get(), Data: data, Flash: r.URL.Query().Get("ok"), Error: r.URL.Query().Get("err")}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, p); err != nil {
		slog.Error("template", "page", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'")
	w.Write(buf.Bytes())
}

// back redirects after a POST with a message.
func back(w http.ResponseWriter, r *http.Request, to string, err error, ok string) {
	q := url.Values{}
	if err != nil {
		q.Set("err", err.Error())
	} else if ok != "" {
		q.Set("ok", ok)
	}
	http.Redirect(w, r, to+"?"+q.Encode(), http.StatusSeeOther)
}

func (a *App) routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", a.healthz)
	m.HandleFunc("GET /setup", func(w http.ResponseWriter, r *http.Request) {
		if a.auth.Configured() {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		a.render(w, r, "setup", "Bienvenue", nil)
	})
	m.HandleFunc("POST /setup", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("password") != r.FormValue("confirm") {
			back(w, r, "/setup", errors.New("les mots de passe diffèrent"), "")
			return
		}
		tok, err := a.auth.Setup(r.FormValue("password"))
		if err != nil {
			back(w, r, "/setup", err, "")
			return
		}
		web.SetCookie(w, tok)
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
	})
	m.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) { a.render(w, r, "login", "Connexion", nil) })
	m.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		tok, err := a.auth.Login(r.FormValue("password"))
		if err != nil {
			back(w, r, "/login", err, "")
			return
		}
		web.SetCookie(w, tok)
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	m.HandleFunc("POST /logout", func(w http.ResponseWriter, r *http.Request) {
		a.auth.Logout(r)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
	m.HandleFunc("GET /{$}", a.home)
	m.HandleFunc("POST /action", a.action)
	m.HandleFunc("GET /settings", func(w http.ResponseWriter, r *http.Request) {
		_, clk := a.clockQuality()
		a.render(w, r, "settings", "Réglages", map[string]any{"Locales": locales, "Voice": a.voiceSupported(), "VoiceOn": a.voiceEnabled(),
			"Clock": clk, "Now": time.Now().In(a.location()).Format("2006-01-02T15:04")})
	})
	m.HandleFunc("POST /settings", a.saveSettings)
	a.serviceRoutes(m)
	m.HandleFunc("POST /clock", func(w http.ResponseWriter, r *http.Request) {
		t, err := time.ParseInLocation("2006-01-02T15:04", r.FormValue("now"), a.location())
		if err != nil || t.Year() < 2024 || t.Year() > 2100 {
			back(w, r, "/settings", errors.New("date et heure invalides"), "")
			return
		}
		back(w, r, "/settings", a.SetClock(t), "Heure réglée")
	})
	m.HandleFunc("POST /password", func(w http.ResponseWriter, r *http.Request) {
		err := a.auth.ChangePassword(r.FormValue("old"), r.FormValue("password"))
		if err == nil {
			http.Redirect(w, r, "/login?ok=Mot+de+passe+modifi%C3%A9", http.StatusSeeOther)
			return
		}
		back(w, r, "/settings", err, "")
	})
	a.updateRoutes(m)
	m.HandleFunc("GET /tags", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		tag := a.lastTag
		a.mu.Unlock()
		a.render(w, r, "tags", "Étiquettes", map[string]any{"Tag": tag, "Books": pynab.Books(a.env.SoundsDirs), "Languages": pynab.Languages, "SurpriseKinds": pynab.SurpriseKinds})
	})
	m.HandleFunc("POST /tags/write", a.writeTag)
	m.HandleFunc("GET /sounds", func(w http.ResponseWriter, r *http.Request) {
		a.render(w, r, "sounds", "Sons", map[string]any{"Sounds": a.userSounds()})
	})
	m.HandleFunc("POST /sounds/upload", a.uploadSound)
	m.HandleFunc("POST /sounds/delete", func(w http.ResponseWriter, r *http.Request) {
		name, err := soundName(r.FormValue("name"))
		if err == nil {
			err = os.Remove(filepath.Join(a.userSoundDir(), name))
		}
		back(w, r, "/sounds", err, "Son supprimé")
	})
	return a.auth.Middleware(m, "/setup", "/login", "/healthz", "/services/mastodon/callback")
}

func (a *App) healthz(w http.ResponseWriter, r *http.Request) {
	if !web.Loopback(r) {
		http.NotFound(w, r)
		return
	}
	connected, core := a.bus.Healthy()
	state, _ := a.bus.State()
	hardware := state.HardwareReady()
	w.Header().Set("Content-Type", "application/json")
	if !connected || !core || !hardware {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	json.NewEncoder(w).Encode(map[string]any{"mqtt": connected, "core": core, "hardware": hardware, "version": a.env.Version})
}

func (a *App) home(w http.ResponseWriter, r *http.Request) {
	core, online := a.bus.State()
	connected, _ := a.bus.Healthy()
	_, clk := a.clockQuality()
	a.mu.Lock()
	data := map[string]any{
		"Core": core, "Online": online, "MQTT": connected, "Network": a.network,
		"Forecast": a.forecast, "WeatherError": a.wxErr, "HA": a.ha.Connected(), "HAError": a.haErr,
		"Voice": a.voiceEnabled(), "Recovered": a.store.Recovered, "Version": a.env.Version,
		"Clock": clk, "Now": time.Now().In(a.location()).Format("15:04"),
	}
	a.mu.Unlock()
	data["Update"] = a.proposedUpdate()
	a.render(w, r, "home", "Nabaztag", data)
}

func (a *App) action(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var err error
	msg := "C'est parti !"
	switch r.FormValue("name") {
	case "time":
		err = a.sayTime()
	case "weather_today":
		a.announceWeather(0)
	case "weather_tomorrow":
		a.announceWeather(1)
	case "sleep":
		a.setOverride(true)
	case "wakeup":
		a.setOverride(false)
	case "cancel":
		if a.stopInteraction() {
			a.stopRadio()
		} else {
			a.stopRadio()
			err = a.do(ctx, "cancel", nil, 5*time.Second)
		}
	case "test_ears", "test_leds":
		err = a.do(ctx, "test", map[string]any{"test": strings.TrimPrefix(r.FormValue("name"), "test_")}, 60*time.Second)
		msg = "Test réussi"
	case "play":
		res := r.FormValue("resource")
		err = a.do(ctx, "play", map[string]any{"sequence": []any{map[string]any{"audio": []string{res}}}}, 5*time.Minute)
		msg = "Son joué"
	case "reboot":
		a.mu.Lock()
		updating := a.updateBusy
		a.mu.Unlock()
		if state := a.upd.Status().State; updating || state == "installing" || state == "downloading" {
			err = errors.New("attendez la fin de la mise à jour")
		} else {
			err = a.rebootSystem()
		}
		msg = "Redémarrage…"
	case "poweroff":
		err = system.PowerOff()
		msg = "Extinction…"
	default:
		err = errors.New("action inconnue")
	}
	to := r.FormValue("back")
	if to != "/sounds" && to != "/updates" {
		to = "/"
	}
	back(w, r, to, err, msg)
}

func parseHM(s string) (config.HM, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return config.HM{}, fmt.Errorf("heure invalide %q", s)
	}
	return config.HM{Hour: t.Hour(), Min: t.Minute()}, nil
}

func atoi(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("nombre invalide %q", s)
	}
	return n, nil
}

func (a *App) saveSettings(w http.ResponseWriter, r *http.Request) {
	f := r.FormValue
	old := a.store.Get()
	place := old.Weather
	if loc := strings.TrimSpace(f("location")); loc != old.Weather.Location {
		place.Location, place.Latitude, place.Longitude = "", 0, 0
		if loc != "" {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			lang, _, _ := strings.Cut(f("locale"), "_")
			p, err := a.wx.Geocode(ctx, loc, lang)
			cancel()
			if err != nil {
				back(w, r, "/settings", fmt.Errorf("lieu introuvable (%v)", err), "")
				return
			}
			place.Location, place.Latitude, place.Longitude = p.Label, p.Latitude, p.Longitude
		}
	}
	st, err := a.store.Update(func(s *config.Settings) error {
		var err error
		s.Locale, s.Timezone = f("locale"), strings.TrimSpace(f("timezone"))
		if s.Volume, err = atoi(f("volume")); err != nil {
			return err
		}
		c := &s.Clock
		c.Chime, c.SleepSounds, c.PerDay = f("chime") == "on", f("sleep_sounds") == "on", f("per_day") == "on"
		if c.Wakeup, err = parseHM(f("wakeup")); err != nil {
			return err
		}
		if c.Sleep, err = parseHM(f("sleep")); err != nil {
			return err
		}
		for i := range c.Days {
			if c.Days[i].Wakeup, err = parseHM(f(fmt.Sprintf("wakeup_%d", i))); err != nil {
				return err
			}
			if c.Days[i].Sleep, err = parseHM(f(fmt.Sprintf("sleep_%d", i))); err != nil {
				return err
			}
		}
		s.Weather.Location, s.Weather.Latitude, s.Weather.Longitude = place.Location, place.Latitude, place.Longitude
		s.Weather.Unit, s.Weather.Animation = f("unit"), f("animation")
		if s.Weather.Frequency, err = atoi(f("frequency")); err != nil {
			return err
		}
		h := &s.HomeAssistant
		h.Enabled, h.Host, h.Username, h.Prefix = f("ha_enabled") == "on", strings.TrimSpace(f("ha_host")), f("ha_username"), strings.TrimSpace(f("ha_prefix"))
		if h.Port, err = atoi(f("ha_port")); err != nil {
			return err
		}
		if pw := f("ha_password"); pw != "" {
			h.Password = pw
		}
		return nil
	})
	if err != nil {
		back(w, r, "/settings", err, "")
		return
	}
	if st.Volume != old.Volume {
		a.applyVolume(r.Context())
	}
	if st.Locale != old.Locale {
		a.publishSettings(r.Context())
	}
	if st.Weather != old.Weather {
		kick(a.weatherKick)
		kick(a.airKick)
	}
	if st.HomeAssistant != old.HomeAssistant {
		a.setHAErr(a.ha.Start(st.HomeAssistant))
	}
	kick(a.clockKick)
	if a.voiceSupported() {
		if err := a.SetVoice(f("voice") == "on"); err != nil {
			back(w, r, "/settings", fmt.Errorf("assistant vocal : %v", err), "")
			return
		}
	}
	back(w, r, "/settings", nil, "Réglages enregistrés")
}

// Original one-byte clock and weather tag variants.
var tagKinds = map[string]struct {
	App  string
	Data string
}{
	"clock_sleep":      {"clock", "\x00"},
	"clock_wakeup":     {"clock", "\x01"},
	"weather_today":    {"weather", "\x01"},
	"weather_tomorrow": {"weather", "\x02"},
}

func (a *App) writeTag(w http.ResponseWriter, r *http.Request) {
	kindName := r.FormValue("kind")
	kind, ok := tagKinds[kindName]
	a.mu.Lock()
	tag := a.lastTag
	a.mu.Unlock()
	if tag == nil {
		back(w, r, "/tags", errors.New("posez d'abord une étiquette sur le lapin"), "")
		return
	}
	var association *config.TagAction
	if !ok {
		var err error
		kind.App = kindName
		switch kindName {
		case "taichi", "airquality":
		case "eightball", "surprise":
			kind.Data, err = pynab.Encode(kindName, r.FormValue("language"), r.FormValue("surprise_kind"))
		case "book":
			kind.Data, err = pynab.BookTag(r.FormValue("voice"), r.FormValue("isbn"))
		case "radio", "ifttt", "webhook":
			kind.Data = "DATA_IN_LOCAL_DB"
			association = &config.TagAction{App: kindName, Value: strings.TrimSpace(r.FormValue("value"))}
			check := a.store.Get()
			check.Tags[str(tag, "uid")] = *association
			err = check.Validate()
		default:
			err = errors.New("application d'étiquette inconnue")
		}
		if err != nil {
			back(w, r, "/tags", err, "")
			return
		}
	}
	if r.FormValue("mode") == "associate" {
		if association == nil || str(tag, "app") != kind.App || str(tag, "support") != "formatted" {
			back(w, r, "/tags", errors.New("choisissez l'application déjà inscrite sur l'étiquette (radio, IFTTT ou webhook)"), "")
			return
		}
		_, err := a.store.Update(func(s *config.Settings) error { s.Tags[str(tag, "uid")] = *association; return nil })
		back(w, r, "/tags", err, "Association enregistrée")
		return
	}
	picture, _ := strconv.Atoi(r.FormValue("picture"))
	err := a.do(r.Context(), "rfid_write", map[string]any{
		"tech": tag["tech"], "uid": tag["uid"], "picture": max(0, min(picture, 255)), "app": kind.App, "data": kind.Data, "timeout": 20,
	}, 30*time.Second)
	if err == nil {
		_, err = a.store.Update(func(s *config.Settings) error {
			if association != nil {
				s.Tags[str(tag, "uid")] = *association
			} else {
				delete(s.Tags, str(tag, "uid"))
			}
			return nil
		})
	}
	back(w, r, "/tags", err, "Étiquette écrite")
}

// User sounds live in <data>/media/sounds/user and play as "user/<name>".

func (a *App) userSoundDir() string { return filepath.Join(a.env.DataDir, "media", "sounds", "user") }

var soundNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}\.(mp3|wav)$`)

func soundName(name string) (string, error) {
	if !soundNameRe.MatchString(name) {
		return "", errors.New("nom de fichier invalide (lettres, chiffres, - et _, .mp3 ou .wav)")
	}
	return name, nil
}

func (a *App) userSounds() []string {
	entries, _ := os.ReadDir(a.userSoundDir())
	var out []string
	for _, e := range entries {
		if soundNameRe.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func sniffAudio(head []byte, ext string) bool {
	if ext == ".wav" {
		return len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WAVE"
	}
	return len(head) >= 3 && (string(head[:3]) == "ID3" || head[0] == 0xFF && head[1]&0xE0 == 0xE0)
}

func (a *App) uploadSound(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
	file, hdr, err := r.FormFile("file")
	if err != nil {
		back(w, r, "/sounds", errors.New("fichier manquant ou trop gros (10 Mo max)"), "")
		return
	}
	defer file.Close()
	name, err := soundName(strings.ReplaceAll(filepath.Base(hdr.Filename), " ", "_"))
	if err != nil {
		back(w, r, "/sounds", err, "")
		return
	}
	data, err := io.ReadAll(file)
	if err != nil || !sniffAudio(data, filepath.Ext(name)) {
		back(w, r, "/sounds", errors.New("ce fichier n'est pas un MP3 ou un WAV"), "")
		return
	}
	dir := a.userSoundDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		back(w, r, "/sounds", err, "")
		return
	}
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err == nil {
		_, err = tmp.Write(data)
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Rename(tmp.Name(), filepath.Join(dir, name))
		}
		os.Remove(tmp.Name())
	}
	back(w, r, "/sounds", err, "Son ajouté")
}
