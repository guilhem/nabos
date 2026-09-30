package main

import (
	"errors"
	"net/http"
)

const maxSSHKeys = 64 << 10

func (a *App) readSSHKeys() (string, string, error) { return a.device.SSHKeys(a.ctx) }
func (a *App) saveSSHKeys(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 3*maxSSHKeys)
	if err := r.ParseForm(); err != nil || !r.PostForm.Has("authorized_keys") || r.PostForm.Get("ssh_revision") == "" {
		back(w, r, "/settings", errors.New("formulaire SSH invalide ou trop volumineux"), "")
		return
	}
	_, err := a.device.SetSSHKeys(r.Context(), r.PostForm.Get("ssh_revision"), r.PostForm.Get("authorized_keys"))
	back(w, r, "/settings", err, "Clés SSH enregistrées")
}
