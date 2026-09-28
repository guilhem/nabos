package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeGitHub struct {
	srv               *httptest.Server
	tag, asset, sums  string
	bundle            []byte
	bundleURL         string
	cutOnce           atomic.Bool
	redirectElsewhere bool
	ranges            []string
}

func newFake(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{tag: "v1.2.0", asset: "nabos-zero2-arm64.raucb", bundle: []byte(strings.Repeat("signed-bundle-", 5000))}
	sum := sha256.Sum256(f.bundle)
	f.sums = hex.EncodeToString(sum[:]) + "  " + f.asset + "\n"
	mux := http.NewServeMux()
	f.srv = httptest.NewTLSServer(mux)
	base := f.srv.URL + "/guilhem/nabos/releases/download/" + f.tag + "/"
	f.bundleURL = base + f.asset
	mux.HandleFunc("/repos/guilhem/nabos/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":%q,"assets":[{"name":%q,"size":%d,"browser_download_url":%q},{"name":"SHA256SUMS","size":100,"browser_download_url":%q}]}`,
			f.tag, f.asset, len(f.bundle), f.bundleURL, base+"SHA256SUMS")
	})
	mux.HandleFunc("/guilhem/nabos/releases/download/", func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "SHA256SUMS":
			w.Write([]byte(f.sums))
		case f.asset:
			if f.redirectElsewhere {
				http.Redirect(w, r, "https://evil.example.org/bundle", http.StatusFound)
				return
			}
			if f.cutOnce.CompareAndSwap(true, false) {
				// Announce the full body, send half of it, drop the connection.
				w.Header().Set("Content-Length", fmt.Sprint(len(f.bundle)))
				w.Write(f.bundle[:len(f.bundle)/2])
				w.(http.Flusher).Flush()
				panic(http.ErrAbortHandler)
			}
			f.ranges = append(f.ranges, r.Header.Get("Range"))
			http.ServeContent(w, r, f.asset, time.Time{}, strings.NewReader(string(f.bundle)))
		}
	})
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) updater(t *testing.T, installed *string) *Updater {
	u := New("guilhem/nabos", f.asset, "v1.1.0", t.TempDir())
	u.APIBase, u.DownloadBase = f.srv.URL, f.srv.URL
	u.HTTP = f.srv.Client()
	host := strings.TrimPrefix(f.srv.URL, "https://")
	u.HTTP.CheckRedirect = AllowRedirects(strings.Split(host, ":")[0])
	u.Install = func(_ context.Context, path string, _ func(int)) error {
		b, _ := os.ReadFile(path)
		if string(b) != string(f.bundle) {
			return fmt.Errorf("corrupt bundle handed to RAUC")
		}
		*installed = path
		return nil
	}
	return u
}

func TestInstallVerifiedBundle(t *testing.T) {
	// GNU sha256sum preserves ./ when the image builder uses ./*.raucb.
	for _, prefix := range []string{"", "./"} {
		t.Run("checksum-prefix="+prefix, func(t *testing.T) {
			f := newFake(t)
			f.sums = strings.Replace(f.sums, f.asset, prefix+f.asset, 1)
			var installed string
			u := f.updater(t, &installed)
			if _, err := u.Check(context.Background()); err != nil || !u.Status().Available {
				t.Fatalf("check: %v %+v", err, u.Status())
			}
			if err := u.InstallLatest(context.Background()); err != nil {
				t.Fatalf("install: %v", err)
			}
			if installed == "" || u.Status().State != "reboot" {
				t.Fatalf("not installed: %+v", u.Status())
			}
		})
	}
}

func TestRefusesUntrustedReleases(t *testing.T) {
	cases := map[string]func(f *fakeGitHub){
		"bad tag":      func(f *fakeGitHub) { f.tag = "v1.2.0;reboot" },
		"foreign url":  func(f *fakeGitHub) { f.bundleURL = "https://evil.example.org/" + f.asset },
		"bad checksum": func(f *fakeGitHub) { f.sums = strings.Repeat("0", 64) + "  " + f.asset + "\n" },
		"unlisted":     func(f *fakeGitHub) { f.sums = "" },
		"checksum traversal": func(f *fakeGitHub) {
			f.sums = strings.Replace(f.sums, f.asset, "../"+f.asset, 1)
		},
		"evil redirect": func(f *fakeGitHub) { f.redirectElsewhere = true },
	}
	for name, mutate := range cases {
		f := newFake(t)
		mutate(f)
		var installed string
		u := f.updater(t, &installed)
		_, err := u.Check(context.Background())
		if err == nil {
			err = u.InstallLatest(context.Background())
		}
		if err == nil || installed != "" {
			t.Fatalf("%s: accepted (err=%v)", name, err)
		}
		if entries, _ := os.ReadDir(u.Dir); name == "bad checksum" && len(entries) != 0 {
			t.Fatalf("%s: corrupt download kept", name)
		}
	}
}

func TestInterruptedDownloadResumes(t *testing.T) {
	f := newFake(t)
	f.cutOnce.Store(true)
	var installed string
	u := f.updater(t, &installed)
	if _, err := u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A power cut during installation can leave a completed bundle. Retaining
	// it alongside the new download can exhaust a 16 GB card's data partition.
	if err := os.WriteFile(filepath.Join(u.Dir, f.asset), []byte("previous bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := u.InstallLatest(context.Background()); err == nil || installed != "" {
		t.Fatalf("cut download must fail without installing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(u.Dir, f.asset)); !os.IsNotExist(err) {
		t.Fatal("stale completed bundle occupies space alongside the partial download")
	}
	fi, err := os.Stat(filepath.Join(u.Dir, f.asset+".part"))
	if err != nil || fi.Size() == 0 || fi.Size() >= int64(len(f.bundle)) {
		t.Fatalf("partial download not kept: %v", err)
	}
	if err := u.InstallLatest(context.Background()); err != nil || installed == "" {
		t.Fatalf("resume: %v", err)
	}
	if len(f.ranges) != 1 || f.ranges[0] != fmt.Sprintf("bytes=%d-", fi.Size()) {
		t.Fatalf("expected one ranged request, got %q", f.ranges)
	}
	if !Newer("v1.10.0", "v1.9.9") || Newer("v1.2.0", "v1.2.0") || Newer("1.3.0", "v1.0.0") {
		t.Fatal("version comparison")
	}
}

func TestUpdateOperationsDoNotOverlap(t *testing.T) {
	f := newFake(t)
	var installed string
	u := f.updater(t, &installed)
	if _, err := u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	entered, finish := make(chan struct{}), make(chan struct{})
	u.Install = func(context.Context, string, func(int)) error {
		close(entered)
		<-finish
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- u.InstallLatest(context.Background()) }()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("installation did not start: %v", err)
	case <-time.After(5 * time.Second):
		close(finish)
		t.Fatal("installation did not start")
	}
	defer func() {
		close(finish)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	if err := u.InstallLatest(context.Background()); err == nil {
		t.Error("accepted overlapping installation")
	}
	if _, err := u.Check(context.Background()); err == nil {
		t.Error("accepted a release check during installation")
	}
	if state := u.Status().State; state != "installing" {
		t.Errorf("lost active installation state: %s", state)
	}
}
