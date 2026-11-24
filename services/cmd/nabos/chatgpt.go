package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/guilhem/nabos/services/internal/chatgpt"
	"github.com/guilhem/nabos/services/internal/web"
)

type chatGPTAccount interface {
	Start(context.Context, string) (string, error)
	Complete(context.Context, string, string) error
	Status(string) (chatgpt.Status, error)
	Disconnect(context.Context) error
}

func (a *App) changeChatGPT(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "same-origin")
	if web.Scheme(r) != "https" {
		http.Error(w, "Ouvrez les réglages en HTTPS pour gérer le compte ChatGPT.", http.StatusForbidden)
		return
	}
	cookie, err := r.Cookie(web.CookieName)
	if err != nil {
		http.Error(w, "Connexion à NabOS requise.", http.StatusUnauthorized)
		return
	}
	// Only a form body may carry the one-time callback. Never reflect it in redirects or logs.
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if r.URL.RawQuery != "" || r.ParseForm() != nil {
		http.Error(w, "Formulaire invalide.", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var message string
	switch r.PathValue("action") {
	case "start":
		_, err = a.chatGPT.Start(ctx, cookie.Value)
		message = "Impossible de démarrer la connexion à ChatGPT. Vérifiez la connexion Internet du lapin et réessayez."
	case "complete":
		values := r.PostForm["callback"]
		if len(r.PostForm) != 1 || len(values) != 1 || strings.TrimSpace(values[0]) == "" {
			http.Error(w, "Collez l’adresse de retour complète.", http.StatusBadRequest)
			return
		}
		err = a.chatGPT.Complete(ctx, cookie.Value, strings.TrimSpace(values[0]))
		if errors.Is(err, chatgpt.ErrConsentRequired) {
			err = nil // Settings shows the action for granting plan access.
		}
		message = "Connexion à ChatGPT inachevée. Vérifiez l’adresse de retour ou recommencez la connexion depuis ces réglages."
	case "disconnect":
		err = a.chatGPT.Disconnect(ctx)
		if errors.Is(err, chatgpt.ErrRevocationUnconfirmed) {
			back(w, r, "/settings", nil, "Le lapin est déconnecté. Vérifiez l’accès accordé à NabOS dans les réglages de ChatGPT.")
			return
		}
		message = "Impossible de déconnecter le compte ChatGPT. Réessayez depuis les réglages."
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		back(w, r, "/settings", errors.New(message), "")
		return
	}
	http.Redirect(w, r, web.Path(r, "/settings#chatgpt"), http.StatusSeeOther)
}
