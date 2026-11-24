package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
)

func TestFrenchVoiceSettingsInstall(t *testing.T) {
	a := testApp(t)
	a.ctx = t.Context()
	cookie := serviceSession(t, a)
	h := a.routes()
	const path = "/settings/voice/install"
	if w := serviceRequest(h, http.MethodPost, path, url.Values{}, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated install: HTTP %d", w.Code)
	}
	if w := serviceRequest(h, http.MethodPost, path, nil, cookie); w.Code != http.StatusForbidden {
		t.Fatalf("install without request origin: HTTP %d", w.Code)
	}
	settings := serviceRequest(h, http.MethodGet, "/settings", nil, cookie)
	if !strings.Contains(settings.Body.String(), path) || !strings.Contains(settings.Body.String(), "Installer la voix") {
		t.Fatal("voice installation is missing from Settings")
	}
	if err := os.WriteFile(filepath.Join(a.env.DataDir, ".volatile"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if w := serviceRequest(h, http.MethodPost, path, url.Values{}, cookie); w.Code != http.StatusSeeOther {
		t.Fatalf("install did not return to Settings: HTTP %d", w.Code)
	}
	for deadline := time.Now().Add(time.Second); a.frenchVoiceStatus().Fetching; {
		if time.Now().After(deadline) {
			t.Fatal("voice installation did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	settings = serviceRequest(h, http.MethodGet, "/settings", nil, cookie)
	if !strings.Contains(settings.Body.String(), "le stockage de données est volatile") {
		t.Fatal("Settings did not report volatile storage")
	}
}

func installPreviewModel(t *testing.T, a *App) {
	t.Helper()
	model := a.frenchVoice.ModelPath()
	dir := filepath.Dir(model)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	m := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest,
		ArtifactType: "application/vnd.nabos.voice.v1", Config: ocispec.DescriptorEmptyJSON,
	}
	for _, file := range []struct{ path, mediaType string }{
		{model, "application/vnd.nabos.voice.model.v1+onnx"},
		{model + ".json", "application/vnd.nabos.voice.config.v1+json"},
	} {
		data := []byte("fixture")
		if err := os.WriteFile(file.path, data, 0o640); err != nil {
			t.Fatal(err)
		}
		layer := content.NewDescriptorFromBytes(file.mediaType, data)
		layer.Annotations = map[string]string{ocispec.AnnotationTitle: filepath.Base(file.path)}
		m.Layers = append(m.Layers, layer)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	a.frenchVoice.Digest = digest.FromBytes(raw)
	if err := os.WriteFile(filepath.Join(dir, ".manifest.json"), raw, 0o640); err != nil {
		t.Fatal(err)
	}
}

const previewPath = "/settings/voice/preview"

func TestFrenchVoicePreviewAdmission(t *testing.T) {
	a := testApp(t)
	cookie := serviceSession(t, a)
	h := a.routes()
	form := url.Values{"text": {"Bonjour !"}}
	if w := serviceRequest(h, "POST", previewPath, form, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated preview: HTTP %d", w.Code)
	}
	if w := serviceRequest(h, "POST", previewPath, nil, cookie); w.Code != http.StatusForbidden {
		t.Fatalf("missing origin: HTTP %d", w.Code)
	}
	if w := serviceRequest(h, "GET", previewPath, nil, cookie); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET preview: HTTP %d", w.Code)
	}
	if w := serviceRequest(h, "POST", previewPath, form, cookie); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing local engine: HTTP %d", w.Code)
	}
	settings := serviceRequest(h, "GET", "/settings", nil, cookie).Body.String()
	if strings.Contains(settings, previewPath) || !strings.Contains(settings, "Installer la voix") || !strings.Contains(settings, "Pi Zero 2 W") {
		t.Fatal("Settings should offer installation and explain board support on Pi 1")
	}
	a.env.Piper, a.env.SpeechDir = "/unused/piper", t.TempDir()
	a.env.SoundsDirs = append(a.env.SoundsDirs, a.env.SpeechDir)
	installPreviewModel(t, a)
	if err := os.Remove(a.frenchVoice.ModelPath() + ".json"); err != nil {
		t.Fatal(err)
	}
	w := serviceRequest(h, "POST", previewPath, form, cookie)
	u, _ := w.Result().Location()
	if u == nil || !strings.Contains(u.Query().Get("err"), "Installez") {
		t.Fatal("accepted model without paired configuration")
	}
	for _, value := range []string{"  ", "mot\x00", "deux\nlignes", "deux\u2028lignes", "abc\xff", strings.Repeat("é", 161)} {
		w := serviceRequest(h, "POST", previewPath, url.Values{"text": {value}}, cookie)
		u, _ := w.Result().Location()
		if u == nil || !strings.Contains(u.Query().Get("err"), "160 caractères") {
			t.Errorf("accepted invalid phrase %q", value)
		}
	}
	for _, test := range []struct {
		path string
		form url.Values
	}{
		{previewPath + "?text=secret", url.Values{}}, {previewPath, url.Values{"text": {"a", "b"}}},
		{previewPath, url.Values{"text": {strings.Repeat("a", 5<<10)}}},
	} {
		if w := serviceRequest(h, "POST", test.path, test.form, cookie); w.Code != http.StatusBadRequest {
			t.Errorf("invalid form accepted: HTTP %d", w.Code)
		}
	}
}

func TestFrenchVoicePreviewPlaysOnceAndCleansUp(t *testing.T) {
	a := testApp(t)
	f := startNative(t, a)
	f.mu.Lock()
	f.hold = func(source string) bool { return strings.HasPrefix(filepath.Base(source), ".nabos-voice-") }
	f.mu.Unlock()
	a.env.SpeechDir = a.env.SoundsDirs[0]
	a.env.Piper = filepath.Join(t.TempDir(), "piper")
	// The fixture copies stdin after a sample WAV prefix for inspection while playback is held.
	const riff = "RIFF1234WAVE" + "00000000000000000000000000000000"
	script := "#!/bin/sh\nset -eu\n[ \"$#\" = 2 ] && [ -s \"$1\" ] && [ -s \"$1.json\" ] || exit 1\nprintf %s " + riff + " > \"$2\"\ncat >> \"$2\"\n"
	if err := os.WriteFile(a.env.Piper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	installPreviewModel(t, a)
	cookie := serviceSession(t, a)
	h := a.routes()
	settings := func() string { return serviceRequest(h, "GET", "/settings", nil, cookie).Body.String() }
	if !strings.Contains(settings(), "Faire parler le lapin") {
		t.Fatal("installed model missing from preview form")
	}
	phrase := strings.Repeat("é", 160)
	post := func() {
		t.Helper()
		w := serviceRequest(h, "POST", previewPath, url.Values{"text": {"  " + phrase + "  "}}, cookie)
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/settings#voix-francaise" {
			t.Fatalf("preview did not return directly to Settings: %d %s", w.Code, w.Header().Get("Location"))
		}
	}
	post()
	index, id := f.play(t, 0, ".wav")
	t.Cleanup(func() { f.finish(id) })
	f.mu.Lock()
	path := f.plays[index].Source
	f.mu.Unlock()
	if filepath.Dir(path) != a.env.SpeechDir || !strings.HasPrefix(filepath.Base(path), ".nabos-voice-") {
		t.Fatalf("unexpected audio source %s", path)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("wrong device-core file permissions: %v, %v", info, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != riff+phrase {
		t.Fatalf("Piper stdin did not preserve the trimmed UTF-8 phrase: %v", err)
	}
	post()
	if page := settings(); !strings.Contains(page, "Synthèse et lecture en cours") || strings.Contains(page, phrase) || strings.Contains(page, previewPath) {
		t.Fatal("busy settings exposed the phrase or invited an overlapping preview")
	}
	f.finish(id)
	pynabWait(t, "preview cleanup", time.Second, func() bool { return !a.frenchVoiceStatus().PreviewBusy })
	if f.length() != index+1 || !strings.Contains(settings(), "Lecture terminée.") {
		t.Fatal("preview did not finish once")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("generated speech retained after playback: %v", err)
	}
	// A partial output from a failed subprocess must never reach the audio player.
	if err := os.WriteFile(a.env.Piper, []byte("#!/bin/sh\nprintf partial > \"$2\"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	post()
	pynabWait(t, "failed preview", time.Second, func() bool { return !a.frenchVoiceStatus().PreviewBusy })
	files, err := filepath.Glob(filepath.Join(a.env.SpeechDir, ".nabos-voice-*"))
	if err != nil || len(files) != 0 || f.length() != index+1 || !strings.Contains(settings(), "n’a pas pu être lue") {
		t.Fatal("failed preview leaked a file, played or reported success")
	}
}

func TestFrenchVoicePreviewStopsWithApp(t *testing.T) {
	a := testApp(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	a.ctx = ctx
	a.env.SpeechDir = t.TempDir()
	a.env.SoundsDirs = []string{a.env.SpeechDir}
	a.env.Piper = filepath.Join(t.TempDir(), "piper")
	if err := os.WriteFile(a.env.Piper, []byte("#!/bin/sh\nprintf partial > \"$2\"\nexec sleep 60\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	installPreviewModel(t, a)
	serviceRequest(a.routes(), "POST", previewPath, url.Values{"text": {"Bonjour !"}}, serviceSession(t, a))
	pynabWait(t, "Piper to start writing", 3*time.Second, func() bool {
		files, _ := os.ReadDir(a.env.SpeechDir)
		if len(files) != 1 {
			return false
		}
		info, err := files[0].Info()
		return err == nil && info.Size() > 0 && info.Mode().Perm() == 0o600
	})
	cancel()
	pynabWait(t, "Piper to exit", time.Second, func() bool { return !a.frenchVoiceStatus().PreviewBusy })
	if files, err := os.ReadDir(a.env.SpeechDir); err != nil || len(files) != 0 {
		t.Fatal("canceled synthesis retained private speech")
	}
}
