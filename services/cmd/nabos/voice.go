package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

	"github.com/guilhem/nabos/services/internal/voice"
	"github.com/opencontainers/go-digest"
	"oras.land/oras-go/v2/registry/remote"
)

const frenchVoiceDigest = "sha256:14245e9dbe1c34983520f0766bc75c2927c0203a16e35ac7285739f72e70ce25"

func newFrenchVoiceStore(dataDir string) (*voice.Store, error) {
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	repo, err := remote.NewRepository("ghcr.io/guilhem/nabos-voices")
	if err != nil {
		return nil, err
	}
	return &voice.Store{DataDir: dataDir, Repository: repo, Digest: digest.Digest(frenchVoiceDigest)}, nil
}

type voiceInstallation struct {
	Fetching  bool
	Installed bool
	Error     string
}

func (a *App) frenchVoiceStatus() *voiceInstallation {
	if a.frenchVoice == nil {
		return nil
	}
	a.mu.Lock()
	fetching, message := a.voiceFetching, a.voiceError
	a.mu.Unlock()
	return &voiceInstallation{Fetching: fetching, Installed: a.frenchVoice.Installed(), Error: message}
}

func (a *App) installFrenchVoice(w http.ResponseWriter, r *http.Request) {
	if a.frenchVoice == nil {
		http.NotFound(w, r)
		return
	}
	a.mu.Lock()
	if a.voiceFetching || (a.voiceError == "" && a.frenchVoice.Installed()) {
		a.mu.Unlock()
		back(w, r, "/settings", nil, "")
		return
	}
	a.voiceFetching, a.voiceError = true, ""
	a.mu.Unlock()
	go a.fetchFrenchVoice()
	back(w, r, "/settings", nil, "")
}

func (a *App) fetchFrenchVoice() {
	ctx, cancel := context.WithTimeout(a.ctx, 15*time.Minute)
	defer cancel()
	err := a.frenchVoice.Install(ctx)
	message := ""
	switch {
	case errors.Is(err, voice.ErrVolatile):
		message = "L’installation est indisponible en mode de secours : le stockage de données est volatile."
	case err != nil:
		slog.Warn("install French voice", "err", err)
		message = "Impossible d’installer la voix. Vérifiez la connexion Internet et l’espace disponible, puis réessayez."
	}
	a.mu.Lock()
	a.voiceFetching, a.voiceError = false, message
	a.mu.Unlock()
}
