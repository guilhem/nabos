package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/guilhem/nabos/services/internal/config"
)

const maxSSHKeys = 64 << 10

func (a *App) sshKeysFile() string {
	return filepath.Join(a.env.DataDir, "ssh", "authorized_keys")
}

func (a *App) readSSHKeys() (string, error) {
	b, err := os.ReadFile(a.sshKeysFile())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return string(b), err
}

// Let OpenSSH parse its own key format. Checking each line also rejects mixed
// valid/invalid input, which ssh-keygen silently skips when given a whole file.
func validateSSHKeys(ctx context.Context, raw string) (string, error) {
	if len(raw) > maxSSHKeys || strings.ContainsRune(raw, 0) {
		return "", errors.New("clés SSH invalides ou trop volumineuses (64 Kio maximum)")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	raw = strings.TrimSpace(strings.ReplaceAll(raw, "\r\n", "\n"))
	keys := 0
	for i, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cmd := exec.CommandContext(ctx, "ssh-keygen", "-l", "-f", "-")
		cmd.Stdin = strings.NewReader(line + "\n")
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("clé publique SSH invalide à la ligne %d : %w", i+1, err)
		}
		keys++
	}
	if keys == 0 {
		return "", nil
	}
	return raw + "\n", nil
}

func (a *App) saveSSHKeys(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 3*maxSSHKeys)
	if err := r.ParseForm(); err != nil || !r.PostForm.Has("authorized_keys") {
		back(w, r, "/settings", errors.New("formulaire SSH invalide ou trop volumineux"), "")
		return
	}
	keys, err := validateSSHKeys(r.Context(), r.PostForm.Get("authorized_keys"))
	if err != nil {
		back(w, r, "/settings", err, "")
		return
	}
	// Serialize persistence and the corresponding start/stop job across requests.
	a.sshMu.Lock()
	defer a.sshMu.Unlock()
	if err := config.WriteFile(a.sshKeysFile(), []byte(keys)); err != nil {
		back(w, r, "/settings", err, "")
		return
	}
	action, message := "start", "Clés enregistrées, SSH activé"
	if keys == "" {
		action, message = "stop", "Nouvelles connexions SSH désactivées"
	}
	// systemctl waits for the job, so the UI reports actual startup failures.
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "systemctl", "--no-ask-password", action, "ssh.service").CombinedOutput(); err != nil {
		back(w, r, "/settings", fmt.Errorf("clés enregistrées, mais SSH : %v %s", err, strings.TrimSpace(string(out))), "")
		return
	}
	if action == "start" && exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", "ssh.service").Run() != nil {
		back(w, r, "/settings", errors.New("clés enregistrées, mais le service SSH n'est pas actif"), "")
		return
	}
	back(w, r, "/settings", nil, message)
}
