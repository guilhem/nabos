package main

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSSHSettingsUseRemoteAPI(t *testing.T) {
	a := testApp(t)
	cookie := serviceSession(t, a)
	h := a.routes()
	f := appFixture(t, a)
	sshRevision, _, err := a.device.SSHKeys(a.ctx)
	if err != nil {
		t.Fatal(err)
	}
	page := serviceRequest(h, "GET", "/settings", nil, cookie)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `name="ssh_revision" value="`+sshRevision+`"`) {
		t.Fatal("SSH snapshot revision missing from form", page.Code, page.Body.String())
	}
	form := url.Values{"ssh_revision": {sshRevision}, "authorized_keys": {"ssh-ed25519 test remote"}}
	if w := serviceRequest(h, "POST", "/settings/ssh", form, nil); w.Code != http.StatusUnauthorized {
		t.Fatal(w.Code)
	}
	w := serviceRequest(h, "POST", "/settings/ssh", form, cookie)
	if strings.Contains(w.Header().Get("Location"), "err=") {
		t.Fatal(w.Header())
	}
	next, keys, err := a.device.SSHKeys(a.ctx)
	if err != nil || keys != form.Get("authorized_keys") || next == sshRevision {
		t.Fatal(next, keys, err)
	}
	if _, err := os.Stat(filepath.Join(a.env.DataDir, "ssh/authorized_keys")); !os.IsNotExist(err) {
		t.Fatal("application wrote local SSH keys", err)
	}
	f.Mu.Lock()
	f.FailSSH = true
	f.Mu.Unlock()
	form.Set("authorized_keys", "invalid")
	form.Set("ssh_revision", next)
	w = serviceRequest(h, "POST", "/settings/ssh", form, cookie)
	if !strings.Contains(w.Header().Get("Location"), "err=") || strings.Contains(w.Header().Get("Location"), "secret") {
		t.Fatal("remote refusal not handled safely", w.Header())
	}
	if revision, keys, _ := a.device.SSHKeys(a.ctx); keys != "ssh-ed25519 test remote" || revision != next {
		t.Fatal("rejection changed SSH snapshot", revision, keys)
	}
}

func TestSSHSettingsRefuseMissingAndStaleRevisions(t *testing.T) {
	a := testApp(t)
	cookie := serviceSession(t, a)
	h := a.routes()
	f := appFixture(t, a)
	oldRevision, _, err := a.device.SSHKeys(a.ctx)
	if err != nil {
		t.Fatal(err)
	}
	current, err := a.device.SetSSHKeys(a.ctx, oldRevision, "current remote keys")
	if err != nil {
		t.Fatal(err)
	}
	page := serviceRequest(h, "GET", "/settings", nil, cookie)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `name="ssh_revision" value="`+current+`"`) || !strings.Contains(page.Body.String(), "current remote keys") {
		t.Fatal("SSH page did not pair current revision and keys", page.Code, page.Body.String())
	}
	configRevision, _, err := a.device.ReadConfig(a.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, revision := range []string{"", oldRevision, configRevision} {
		form := url.Values{"ssh_revision": {revision}, "authorized_keys": {"must not replace keys"}}
		if revision == "" {
			form.Del("ssh_revision") // the old form had no SSH revision
		}
		w := serviceRequest(h, "POST", "/settings/ssh", form, cookie)
		if !strings.Contains(w.Header().Get("Location"), "err=") {
			t.Fatal("missing/stale SSH form accepted", revision, w.Code, w.Header())
		}
		if saved, keys, err := a.device.SSHKeys(a.ctx); err != nil || saved != current || keys != "current remote keys" {
			t.Fatal("refused SSH form changed snapshot", saved, keys, err)
		}
	}
	f.Mu.Lock()
	f.SSHRevision = "ssh-restarted:1"
	f.Mu.Unlock()
	form := url.Values{"ssh_revision": {current}, "authorized_keys": {"stale daemon keys"}}
	w := serviceRequest(h, "POST", "/settings/ssh", form, cookie)
	if !strings.Contains(w.Header().Get("Location"), "err=") {
		t.Fatal("SSH form survived daemon incarnation change", w.Header())
	}
	if revision, keys, err := a.device.SSHKeys(a.ctx); err != nil || revision != "ssh-restarted:1" || keys != "current remote keys" {
		t.Fatal("old daemon form changed SSH snapshot", revision, keys, err)
	}
}
