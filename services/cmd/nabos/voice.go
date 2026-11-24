package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/guilhem/nabos/services/internal/rabbit"
	"github.com/guilhem/nabos/services/internal/voice"
	"github.com/guilhem/nabos/services/internal/web"
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
	Fetching         bool
	Installed        bool
	Error            string
	PreviewSupported bool
	PreviewBusy      bool
	PreviewMessage   string
}

func (a *App) frenchVoiceStatus() *voiceInstallation {
	if a.frenchVoice == nil {
		return nil
	}
	installed := a.frenchVoice.Installed()
	a.mu.Lock()
	defer a.mu.Unlock()
	return &voiceInstallation{
		Fetching:         a.voiceFetching,
		Installed:        installed,
		Error:            a.voiceError,
		PreviewSupported: a.voicePreviewSupported(),
		PreviewBusy:      a.voicePreviewDone != nil,
		PreviewMessage:   a.voicePreviewMessage,
	}
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

func (a *App) voicePreviewSupported() bool {
	return filepath.IsAbs(a.env.Piper) && filepath.IsAbs(a.env.SpeechDir) && slices.Contains(a.env.SoundsDirs, a.env.SpeechDir)
}

func (a *App) previewFrenchVoice(w http.ResponseWriter, r *http.Request) {
	if !a.voicePreviewSupported() || a.frenchVoice == nil {
		http.Error(w, "La synthèse locale est disponible sur Raspberry Pi Zero 2 W.", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if r.URL.RawQuery != "" || r.ParseForm() != nil || len(r.PostForm) != 1 || len(r.PostForm["text"]) != 1 {
		http.Error(w, "Formulaire invalide.", http.StatusBadRequest)
		return
	}
	raw := r.PostForm.Get("text")
	text := strings.TrimSpace(raw)
	multilineOrControl := strings.ContainsAny(raw, "\u2028\u2029") || strings.IndexFunc(raw, unicode.IsControl) >= 0
	if text == "" || !utf8.ValidString(raw) || utf8.RuneCountInString(text) > 160 || multilineOrControl {
		back(w, r, "/settings", errors.New("Saisissez une phrase de 1 à 160 caractères sur une seule ligne."), "")
		return
	}
	if !a.frenchVoice.Installed() {
		back(w, r, "/settings", errors.New("Installez d’abord la voix française du Nabaztag."), "")
		return
	}
	a.mu.Lock()
	if a.voicePreviewDone == nil && a.ctx.Err() == nil {
		a.voicePreviewDone = make(chan struct{})
		a.voicePreviewMessage = ""
		go a.runVoicePreview(text)
	}
	a.mu.Unlock()
	http.Redirect(w, r, web.Path(r, "/settings#voix-francaise"), http.StatusSeeOther)
}

func (a *App) runVoicePreview(text string) {
	ctx, cancel := context.WithTimeout(a.ctx, 3*time.Minute)
	defer cancel()
	message := "Lecture terminée."
	if err := a.speakFrench(ctx, text); err != nil {
		slog.Warn("French voice preview failed", "err", err)
		message = "La phrase n’a pas pu être lue. Vérifiez que le lapin est éveillé, puis réessayez."
	}
	a.mu.Lock()
	a.voicePreviewMessage = message
	close(a.voicePreviewDone)
	a.voicePreviewDone = nil
	a.mu.Unlock()
}

func (a *App) speakFrench(ctx context.Context, text string) (err error) {
	// A hidden basename stays out of random sound selections and resolves as an exact resource.
	wav, err := os.CreateTemp(a.env.SpeechDir, ".nabos-voice-*.wav")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, wav.Close(), os.Remove(wav.Name())) }()
	cmd := exec.CommandContext(ctx, a.env.Piper, a.frenchVoice.ModelPath(), wav.Name())
	cmd.Stdin = strings.NewReader(text)
	if err := cmd.Run(); err != nil {
		return err
	}
	// Python's wave writer emits a 44-byte PCM header, followed by the samples.
	var header [45]byte
	if _, err := wav.ReadAt(header[:], 0); err != nil || !sniffAudio(header[:], ".wav") {
		return errors.New("Piper produced an empty or invalid WAV")
	}
	// The setgid speech directory supplies device-core's read group.
	if err := wav.Chmod(0o640); err != nil {
		return err
	}
	return a.do(ctx, rabbit.Command{Action: rabbit.Play, Cancelable: true,
		Sequence: []rabbit.Item{{Audio: []string{filepath.Base(wav.Name())}}},
	}, time.Minute)
}
