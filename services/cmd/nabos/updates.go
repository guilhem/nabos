package main

import (
	"errors"
	"net/http"

	"github.com/guilhem/nabos/services/internal/device"
)

func (a *App) proposedUpdate() string {
	settings, err := a.systemSettings(a.ctx)
	if err != nil {
		return ""
	}
	releases, err := a.device.Releases(a.ctx, settings.Updates.Channel)
	if err != nil {
		return ""
	}
	for _, rel := range releases {
		if rel.Ready && rel.Blocked == "" {
			return rel.Tag
		}
	}
	return ""
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
		status, err := a.device.UpdateStatus(r.Context())
		settings, configErr := a.systemSettings(r.Context())
		configured, configuredErr := a.device.UpdatesConfigured(r.Context())
		var releases []device.Release
		if err == nil {
			err = configErr
		}
		if err == nil {
			err = configuredErr
		}
		if err == nil && configured {
			releases, err = a.device.Releases(r.Context(), settings.Updates.Channel)
		}
		busy := status.State == "installing" || status.State == "downloading" || status.State == "reboot" || status.State == "confirming" || err != nil
		a.render(w, r, "updates", "Mises à jour", map[string]any{
			"Status": status, "StateLabel": updateStateLabel(status.State), "Configured": configured,
			"Releases": releases, "Busy": busy, "LastError": err,
		})
	})
	m.HandleFunc("POST /updates/settings", a.saveUpdateSettings)
	m.HandleFunc("POST /updates/check", func(w http.ResponseWriter, r *http.Request) {
		_, err := a.device.CheckUpdates(r.Context())
		back(w, r, "/updates", err, "Vérification terminée")
	})
	m.HandleFunc("POST /updates/install", func(w http.ResponseWriter, r *http.Request) {
		settings, err := a.systemSettings(r.Context())
		if err == nil {
			_, err = a.device.InstallUpdate(r.Context(), r.FormValue("tag"), settings.Updates.Channel, false, r.FormValue("retry") == "true")
		}
		back(w, r, "/updates", err, "Installation demandée ; le redémarrage restera manuel")
	})
}

func (a *App) saveUpdateSettings(w http.ResponseWriter, r *http.Request) {
	revision, settings, err := a.device.ReadConfig(r.Context())
	if err == nil && revision != r.FormValue("revision") {
		err = errors.New("réglages système modifiés, rechargez la page")
	}
	if err == nil {
		switch r.FormValue("mode") {
		case "manual":
			settings.AutoCheck, settings.Updates.Automatic = false, false
		case "notify":
			settings.AutoCheck, settings.Updates.Automatic = true, false
		case "auto":
			settings.AutoCheck, settings.Updates.Automatic = true, true
		default:
			err = errors.New("mode de mise à jour invalide")
		}
	}
	if err == nil {
		settings.Updates.Channel = r.FormValue("channel")
		settings.Updates.Start, err = parseSystemHM(r.FormValue("start"))
	}
	if err == nil {
		settings.Updates.End, err = parseSystemHM(r.FormValue("end"))
	}
	if err == nil {
		_, err = a.device.UpdateConfig(r.Context(), revision, settings)
	}
	back(w, r, "/updates", err, "Réglages des mises à jour enregistrés")
}
func parseSystemHM(raw string) (device.HM, error) {
	h, err := parseHM(raw)
	return device.HM{Hour: uint32(h.Hour), Min: uint32(h.Min)}, err
}
