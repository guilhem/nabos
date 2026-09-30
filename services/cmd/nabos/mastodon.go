package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/guilhem/nabos/services/internal/config"
	"github.com/guilhem/nabos/services/internal/mastodon"
	"github.com/guilhem/nabos/services/internal/rabbit"
)

type oauthLogin struct {
	nonce, instance, id, secret, redirect string
	expires                               time.Time
}

// Serializes pairing transitions without holding the global settings lock
// while Mastodon is unavailable. Other settings remain usable during a request.
func (a *App) updateMastodon(fn func(*mastodon.State) error) error {
	a.mastodonMu.Lock()
	defer a.mastodonMu.Unlock()
	next := a.store.Get().Mastodon
	if err := fn(&next); err != nil {
		return err
	}
	_, err := a.store.Update(func(s *config.Settings) error { s.Mastodon = next; return nil })
	return err
}

func (a *App) mastodonLoop(ctx context.Context) {
	for {
		s := a.store.Get().Mastodon
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		c, err := mastodon.NewClient(s.Instance, nil)
		if s.Instance != "" && err != nil {
			a.serviceError("mastodon", err)
		}
		if err == nil && s.AccessToken != "" {
			go func() {
				defer close(done)
				c.Run(runCtx,
					func() mastodon.State { return a.store.Get().Mastodon }, a.updateMastodon,
					func(l, r int) {
						_, err := a.store.Update(func(s *config.Settings) error { s.Ears = [2]int{l, r}; return nil })
						if err != nil {
							a.serviceError("mastodon", err)
							return
						}
						a.send(earsCommand(l, r))
					},
					a.mastodonSound,
					func(err error) { a.serviceError("mastodon", err) })
			}()
		} else {
			close(done)
		}
		select {
		case <-ctx.Done():
			cancel()
			<-done
			return
		case <-a.mastodonKick:
			cancel()
			<-done
		}
	}
}

func (a *App) mastodonSound(sound string) {
	command := sequence(sound, "")
	if sound != "mastodon/communion.wav" {
		command = rabbit.Command{Action: rabbit.Message, Cancelable: true, Signature: &rabbit.Item{Audio: []string{"mastodon/respirations/*.mp3"}}, Body: []rabbit.Item{{Audio: []string{sound + ";fr_FR/" + sound}}}}
	}
	go func() { a.serviceError("mastodon", a.media(a.ctx, command, time.Minute)) }()
}

func (a *App) mastodonEars(left, right *uint8) {
	if left == nil || right == nil || a.store.Get().Mastodon.PairingState != "married" {
		return
	}
	l, r := int(*left), int(*right)
	go func() {
		ctx, cancel := context.WithTimeout(a.ctx, 15*time.Second)
		defer cancel()
		err := a.updateMastodon(func(s *mastodon.State) error {
			if s.PairingState != "married" {
				return nil
			}
			c, err := mastodon.NewClient(s.Instance, nil)
			if err != nil {
				return err
			}
			return c.SendEars(ctx, s, l, r)
		})
		a.serviceError("mastodon", err)
		if err == nil {
			a.mastodonSound("mastodon/communion.wav")
		}
	}()
}

func (a *App) mastodonRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /services/mastodon/connect", a.connectMastodon)
	m.HandleFunc("GET /services/mastodon/callback", a.mastodonCallback)
	m.HandleFunc("POST /services/mastodon/action", a.mastodonAction)
}

func (a *App) connectMastodon(w http.ResponseWriter, r *http.Request) {
	instance := strings.TrimSpace(r.FormValue("instance"))
	c, err := mastodon.NewClient(instance, nil)
	if err != nil {
		back(w, r, "/services", err, "")
		return
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	redirect := (&url.URL{Scheme: scheme, Host: r.Host, Path: "/services/mastodon/callback"}).String()
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	id, secret, err := c.RegisterApp(ctx, redirect)
	if err != nil {
		back(w, r, "/services", err, "")
		return
	}
	nonce := rand.Text()
	u, err := c.AuthorizeURL(id, redirect, nonce)
	if err != nil {
		back(w, r, "/services", err, "")
		return
	}
	a.mu.Lock()
	a.oauth = &oauthLogin{nonce, instance, id, secret, redirect, time.Now().Add(10 * time.Minute)}
	a.mu.Unlock()
	http.Redirect(w, r, u, http.StatusSeeOther)
}

func (a *App) mastodonCallback(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	p := a.oauth
	valid := p != nil && time.Now().Before(p.expires) && subtle.ConstantTimeCompare([]byte(p.nonce), []byte(r.URL.Query().Get("state"))) == 1
	if valid {
		a.oauth = nil
	}
	a.mu.Unlock()
	if !valid {
		http.Error(w, "Connexion expirée ou état OAuth invalide", http.StatusBadRequest)
		return
	}
	u, err := url.Parse(p.redirect)
	if err != nil || u.Host != r.Host || r.URL.Query().Get("code") == "" {
		http.Error(w, "Réponse OAuth invalide", http.StatusBadRequest)
		return
	}
	c, err := mastodon.NewClient(p.instance, nil)
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err == nil {
		var token string
		token, err = c.ExchangeCode(ctx, p.id, p.secret, p.redirect, r.URL.Query().Get("code"))
		if err == nil {
			var account mastodon.Account
			account, err = c.VerifyAccount(ctx, token)
			if err == nil {
				err = a.updateMastodon(func(s *mastodon.State) error {
					*s = mastodon.State{Instance: p.instance, ClientID: p.id, ClientSecret: p.secret, RedirectURI: p.redirect}
					return s.SetAccount(token, account)
				})
			}
		}
	}
	if err == nil {
		kick(a.mastodonKick)
		a.mastodonSound("mastodon/setup.mp3")
	}
	back(w, r, "/services", err, "Compte Mastodon connecté")
}

func (a *App) mastodonAction(w http.ResponseWriter, r *http.Request) {
	action := r.FormValue("action")
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	err := a.updateMastodon(func(s *mastodon.State) error {
		if action == "disconnect" {
			s.ClearAccount()
			return nil
		}
		c, err := mastodon.NewClient(s.Instance, nil)
		if err != nil {
			return err
		}
		switch action {
		case "propose":
			return c.Propose(ctx, s, r.FormValue("spouse"))
		case "accept":
			return c.Accept(ctx, s)
		case "reject":
			return c.Reject(ctx, s)
		case "divorce":
			return c.Divorce(ctx, s)
		default:
			return errors.New("action Mastodon inconnue")
		}
	})
	if err == nil {
		kick(a.mastodonKick)
	}
	back(w, r, "/services", err, "Jumelage mis à jour")
}
