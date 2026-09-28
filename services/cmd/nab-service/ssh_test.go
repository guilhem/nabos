package main

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSSHSettings(t *testing.T) {
	a := testApp(t)
	cookie := serviceSession(t, a)
	h := a.routes()
	dir := t.TempDir()
	key := filepath.Join(dir, "client")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "test<&+", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v %s", err, out)
	}
	pub, _ := os.ReadFile(key + ".pub")
	private, _ := os.ReadFile(key)
	// Only the job runner is faked; key validation uses the installed OpenSSH.
	control := `#!/bin/sh
[ "$3" = ssh.service ] || exit 2
if [ "$1" = is-active ]; then test -s "$SSH_TEST_KEYS"; exit; fi
[ "$1" = --no-ask-password ] || exit 2
echo "$2" >> "$SSH_TEST_LOG"
[ ! -f "$SSH_TEST_FAIL" ] || exit 1
case "$2" in
start) test -s "$SSH_TEST_KEYS" ;;
stop) test ! -s "$SSH_TEST_KEYS" ;;
*) exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(control), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("SSH_TEST_KEYS", a.sshKeysFile())
	t.Setenv("SSH_TEST_LOG", filepath.Join(dir, "jobs"))
	t.Setenv("SSH_TEST_FAIL", filepath.Join(dir, "fail"))
	form := url.Values{"authorized_keys": {string(pub)}}
	if w := serviceRequest(h, "POST", "/settings/ssh", form, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated POST: %d", w.Code)
	}
	r := httptest.NewRequest("POST", "/settings/ssh", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://elsewhere.example")
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST: %d", w.Code)
	}
	if keys, err := a.readSSHKeys(); keys != "" || err != nil {
		t.Fatalf("SSH enabled before authorized save: %v", err)
	}
	save := func(raw string, wantError bool) {
		t.Helper()
		form.Set("authorized_keys", raw)
		w := serviceRequest(h, "POST", "/settings/ssh", form, cookie)
		if w.Code != http.StatusSeeOther || strings.Contains(w.Header().Get("Location"), "err=") != wantError {
			t.Fatalf("save: %d %s", w.Code, w.Header().Get("Location"))
		}
	}
	save(string(pub), false)
	fi, err := os.Stat(a.sshKeysFile())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("private permissions: %v %v", fi, err)
	}
	for _, raw := range []string{"not a key", string(private), string(pub) + "broken\n", strings.Repeat("x", maxSSHKeys+1), string(pub) + "\x00"} {
		save(raw, true)
		if stored, _ := a.readSSHKeys(); stored != string(pub) {
			t.Fatal("invalid input replaced the working keys")
		}
	}
	keys := "# My devices\r\n" + strings.TrimSpace(string(pub)) + "\r\nrestrict " + strings.TrimSpace(string(pub)) + "\r\n"
	save(keys, false)
	keys = strings.ReplaceAll(keys, "\r\n", "\n")
	page := serviceRequest(h, "GET", "/settings", nil, cookie)
	if page.Code != http.StatusOK || !strings.Contains(html.UnescapeString(page.Body.String()), strings.TrimSpace(string(pub))) {
		t.Fatal("saved keys missing from settings")
	}
	afterRestart, err := NewApp(a.env)
	if err != nil {
		t.Fatal(err)
	}
	if stored, _ := afterRestart.readSSHKeys(); stored != keys {
		t.Fatal("keys lost after application restart")
	}
	if err := os.WriteFile(filepath.Join(dir, "fail"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	save(string(pub), true)
	if stored, _ := a.readSSHKeys(); stored != string(pub) {
		t.Fatal("startup failure discarded saved keys")
	}
	os.Remove(filepath.Join(dir, "fail"))
	save("# no keys left\n", false)
	if stored, _ := a.readSSHKeys(); stored != "" {
		t.Fatal("removing the last key did not disable SSH")
	}
	jobs, _ := os.ReadFile(filepath.Join(dir, "jobs"))
	if string(jobs) != "start\nstart\nstart\nstop\n" {
		t.Fatalf("unexpected systemd jobs: %q", jobs)
	}
}
