package main

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

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
		status, statusErr := a.device.UpdateStatus(r.Context())
		settings, configErr := a.systemSettings(r.Context())
		configured, configuredErr := a.device.UpdatesConfigured(r.Context())
		var releases []device.Release
		catalogErr := errors.Join(configErr, configuredErr)
		if catalogErr == nil && configured {
			releases, catalogErr = a.device.Releases(r.Context(), settings.Updates.Channel)
		}
		busy := statusErr != nil || updateBusy(status)
		a.render(w, r, "updates", "Mises à jour", map[string]any{
			"Status": status, "StateLabel": updateStateLabel(status.State), "Configured": configured,
			"Releases": releases, "Busy": busy, "LastError": errors.Join(statusErr, catalogErr),
		})
	})
	m.HandleFunc("POST /updates/upload", a.uploadUpdate)
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

func updateBusy(status device.UpdateStatus) bool {
	return status.State == "installing" || status.State == "downloading" || status.State == "reboot" || status.State == "confirming"
}

const maxUpdateBundle = 2 << 30
const maxUpdateRequest = maxUpdateBundle + (64 << 10)

func (a *App) uploadUpdate(w http.ResponseWriter, r *http.Request) {
	err := a.receiveUpdate(w, r)
	back(w, r, "/updates", err, "Installation demandée ; le redémarrage restera manuel")
}

func (a *App) receiveUpdate(w http.ResponseWriter, r *http.Request) error {
	dir, err := filepath.Abs(a.env.DataDir)
	if err != nil {
		return errors.New("impossible de vérifier le stockage de la mise à jour")
	}
	// boot-init marks the shared data mount when it falls back to its 64 MiB tmpfs.
	for ; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, ".volatile")); err == nil {
			return errors.New("l’import est indisponible en mode de secours : le stockage de données est volatile")
		} else if !os.IsNotExist(err) {
			return errors.New("impossible de vérifier le stockage de la mise à jour")
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	// Reserve staging before RAUC knows about this upload.
	if !a.updateUploadMu.TryLock() {
		return errors.New("un import de mise à jour est déjà en cours ; réessayez après sa fin")
	}
	defer a.updateUploadMu.Unlock()
	if r.ContentLength > maxUpdateRequest {
		return errors.New("le fichier de mise à jour dépasse la limite de 2 Gio")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpdateRequest)
	parts, err := r.MultipartReader()
	if err != nil {
		return errors.New("formulaire d’import de mise à jour invalide")
	}
	status, err := a.device.UpdateStatus(r.Context())
	if err != nil {
		return err
	}
	if updateBusy(status) {
		return errors.New("une mise à jour est déjà en cours ; attendez sa fin et le redémarrage")
	}
	var bundle *os.File
	defer func() {
		if bundle != nil {
			bundle.Close()
			os.Remove(bundle.Name())
		}
	}()
	flags := map[string]bool{}
	for {
		part, err := parts.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return updateUploadReadError(err)
		}
		switch part.FormName() {
		case "bundle":
			ext := strings.ToLower(filepath.Ext(part.FileName()))
			if bundle != nil || (ext != ".rauc" && ext != ".raucb") {
				return errors.New("sélectionnez un seul fichier de mise à jour .rauc ou .raucb")
			}
			dir := filepath.Join(a.env.DataDir, "updates")
			if err := os.MkdirAll(dir, 0700); err != nil {
				return errors.New("impossible de préparer le stockage de la mise à jour")
			}
			bundle, err = os.CreateTemp(dir, "upload-*")
			if err != nil {
				return errors.New("impossible de stocker la mise à jour")
			}
			n, err := io.Copy(bundle, io.LimitReader(part, maxUpdateBundle+1))
			if n > maxUpdateBundle {
				return errors.New("le fichier de mise à jour dépasse la limite de 2 Gio")
			}
			if err != nil {
				return updateUploadReadError(err)
			}
			if n == 0 {
				return errors.New("le fichier de mise à jour est vide")
			}
		case "ignore_certificate", "retry":
			name := part.FormName()
			_, duplicate := flags[name]
			value, err := io.ReadAll(io.LimitReader(part, 6))
			if err != nil {
				return updateUploadReadError(err)
			}
			if duplicate || part.FileName() != "" || (string(value) != "true" && string(value) != "false") {
				return errors.New("option d’import de mise à jour invalide ou répétée")
			}
			flags[name] = string(value) == "true"
		default:
			return errors.New("champ d’import de mise à jour invalide")
		}
	}
	if bundle == nil {
		return errors.New("sélectionnez un fichier de mise à jour .rauc ou .raucb")
	}
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		return updateUploadReadError(err)
	}
	if _, err := bundle.Seek(0, io.SeekStart); err != nil {
		return errors.New("impossible de lire le fichier de mise à jour")
	}
	_, err = a.device.InstallBundle(r.Context(), bundle, flags["ignore_certificate"], flags["retry"])
	return err
}

func updateUploadReadError(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return errors.New("le formulaire dépasse la limite d’import de 2 Gio")
	}
	return errors.New("impossible de recevoir la mise à jour : fichier ou formulaire incomplet, ou stockage insuffisant")
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
