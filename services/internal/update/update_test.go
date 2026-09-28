package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeGitHub struct {
	srv               *httptest.Server
	asset             string
	mu                sync.Mutex
	releases          []map[string]any // GitHub API objects
	bundles, sums     map[string]string
	failPage          string
	cutOnce, badRange atomic.Bool
	redirectElsewhere bool
	ranges, installed []string
}

func newFake(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{asset: "nabos-zero2-arm64.raucb", bundles: map[string]string{}, sums: map[string]string{}}
	mux := http.NewServeMux()
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	mux.HandleFunc("GET /repos/guilhem/nabos/releases", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		q := r.URL.Query()
		page, _ := strconv.Atoi(q.Get("page"))
		if q.Get("per_page") != "100" || page < 1 || q.Get("page") == f.failPage {
			http.Error(w, "unavailable", http.StatusBadGateway)
			return
		}
		json.NewEncoder(w).Encode(f.releases[min((page-1)*100, len(f.releases)):min(page*100, len(f.releases))])
	})
	mux.HandleFunc("GET /repos/guilhem/nabos/releases/tags/{tag}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, rel := range f.releases {
			if rel["tag_name"] == r.PathValue("tag") {
				json.NewEncoder(w).Encode(rel)
				return
			}
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("GET /guilhem/nabos/releases/download/{tag}/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		bundle, sums := f.bundles[r.PathValue("tag")], f.sums[r.PathValue("tag")]
		f.mu.Unlock()
		switch r.PathValue("name") {
		case SumsAsset:
			w.Write([]byte(sums))
		case f.asset:
			switch {
			case f.redirectElsewhere:
				http.Redirect(w, r, "https://evil.example.org/bundle", http.StatusFound)
			case f.cutOnce.CompareAndSwap(true, false):
				// Announce the full body, send half of it, drop the connection.
				w.Header().Set("Content-Length", fmt.Sprint(len(bundle)))
				w.Write([]byte(bundle[:len(bundle)/2]))
				w.(http.Flusher).Flush()
				panic(http.ErrAbortHandler)
			case r.Header.Get("Range") != "" && f.badRange.CompareAndSwap(true, false):
				w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(bundle)-1, len(bundle)))
				w.WriteHeader(http.StatusPartialContent)
				w.Write([]byte(bundle))
			default:
				f.mu.Lock()
				f.ranges = append(f.ranges, r.Header.Get("Range"))
				f.mu.Unlock()
				http.ServeContent(w, r, f.asset, time.Time{}, strings.NewReader(bundle))
			}
		}
	})
	f.add("v1.2.0", false)
	return f
}

// add publishes a ready release and returns its API object for mutation.
func (f *fakeGitHub) add(tag string, prerelease bool) map[string]any {
	bundle := strings.Repeat("signed-bundle-"+tag, 500)
	sum := sha256.Sum256([]byte(bundle))
	f.bundles[tag] = bundle
	f.sums[tag] = hex.EncodeToString(sum[:]) + "  " + f.asset + "\n"
	base := f.srv.URL + "/guilhem/nabos/releases/download/" + tag + "/"
	rel := map[string]any{"tag_name": tag, "prerelease": prerelease, "published_at": "2026-09-01T10:00:00Z", "body": "notes " + tag,
		"assets": []map[string]any{
			{"name": f.asset, "size": len(bundle), "browser_download_url": base + f.asset},
			{"name": SumsAsset, "size": 100, "browser_download_url": base + SumsAsset},
			{"name": "nabos-zero-armv6.raucb", "size": 1, "browser_download_url": "https://elsewhere.example.org/other-board"},
		}}
	f.releases = append(f.releases, rel)
	return rel
}

func asset(rel map[string]any, i int) map[string]any { return rel["assets"].([]map[string]any)[i] }

var good = BootState{BootID: "boot-1", Slot: "A", Health: "good", Operation: "idle"}

// updater installs from f; boot is what RAUC and the health check report.
func (f *fakeGitHub) updater(t *testing.T, dir string, boot *BootState) *Updater {
	u := New("guilhem/nabos", f.asset, "v1.1.0", dir)
	u.APIBase, u.DownloadBase = f.srv.URL, f.srv.URL
	u.HTTP = f.srv.Client()
	host, _ := url.Parse(f.srv.URL)
	u.HTTP.CheckRedirect = AllowRedirects(host.Hostname())
	u.Probe = func() (BootState, error) { return *boot, nil }
	u.Install = func(_ context.Context, path string, _ func(int)) error {
		b, _ := os.ReadFile(path)
		for tag, bundle := range f.bundles {
			if string(b) == bundle {
				f.installed = append(f.installed, tag)
				return nil
			}
		}
		return errors.New("corrupt bundle handed to RAUC")
	}
	return u
}

var (
	manual = InstallOptions{Channel: "stable"}
	retry  = InstallOptions{Channel: "stable", Retry: true}
	auto   = InstallOptions{Channel: "stable", Automatic: true}
)

func tags(rels []Release) []string {
	var s []string
	for _, r := range rels {
		s = append(s, r.Tag)
	}
	return s
}

func TestCatalogue(t *testing.T) {
	f := newFake(t)
	for i := range 200 { // three pages
		f.add(fmt.Sprintf("v0.9.%d", i), false)
	}
	for _, tag := range []string{"v1.3.0-rc.2", "v1.3.0", "v1.3.0-rc.10", "v1.3.0-beta", "v1.1.0+rebuild", "v1.2.0-alpha", "v1.5.0-preview"} {
		f.add(tag, false)
	}
	f.add("v1.2.5", true) // flagged preview without suffix
	f.add("v9.0.0", false)["draft"] = true
	for _, bad := range []string{"v1.4", "latest", "v01.5.0", "v1.5.0-01", "v1.5.0;reboot"} {
		f.add(bad, false)
	}
	f.add("v1.2.6", false)["assets"] = []map[string]any{}
	asset(f.add("v1.2.7", false), 0)["browser_download_url"] = "https://evil.example.org/bundle"
	asset(f.add("v1.2.8", false), 1)["size"] = 1 << 20
	u := f.updater(t, t.TempDir(), &good)
	all, err := u.Check(context.Background())
	if err != nil || len(all) != 212 {
		t.Fatalf("check: %v, %d releases", err, len(all))
	}
	want := []string{"v1.5.0-preview", "v1.3.0", "v1.3.0-rc.10", "v1.3.0-rc.2", "v1.3.0-beta", "v1.2.8", "v1.2.7", "v1.2.6", "v1.2.5", "v1.2.0", "v1.2.0-alpha"}
	if got := tags(u.Releases("test")); !slices.Equal(got, want) {
		t.Errorf("test channel %q", got)
	}
	if got := tags(u.Releases("stable")); !slices.Equal(got, []string{"v1.3.0", "v1.2.8", "v1.2.7", "v1.2.6", "v1.2.0"}) {
		t.Errorf("stable channel %q", got)
	}
	if got := u.Releases("nightly"); got != nil {
		t.Errorf("unknown channel %q", tags(got))
	}
	for _, r := range u.Releases("test") {
		broken := slices.Contains([]string{"v1.2.6", "v1.2.7", "v1.2.8"}, r.Tag)
		if r.Ready == broken || (r.Problem == "") != r.Ready || r.Published.IsZero() || r.Notes != "notes "+r.Tag {
			t.Errorf("%s: ready %v problem %q", r.Tag, r.Ready, r.Problem)
		}
	}
	if s := u.Status(); s.Checked.IsZero() || s.Checking || s.CheckError != "" || s.State != "idle" {
		t.Errorf("status %+v", s)
	}
	if !Newer("v1.3.0-rc.10", "v1.3.0-rc.2") || !Newer("v1.3.0", "v1.3.0-rc.10") || !Newer("v1.3.0-rc", "v1.3.0-beta.9") ||
		!Newer("v1.3.0-rc.1", "v1.3.0-rc") || Newer("v1.1.0+rebuild", "v1.1.0") || !Newer("v1.10.0", "v1.9.9") ||
		Newer("1.3.0", "v1.0.0") || !Newer("v0.0.1", "dev") || !Newer("v1.0.0-beta", "v1.0.0-9") ||
		!Newer("v1.0.0-99999999999999999999", "v1.0.0-9999999999999999999") {
		t.Error("version comparison")
	}
}

func TestIncompleteCatalogueKeepsThePreviousOne(t *testing.T) {
	f := newFake(t)
	for i := range 150 {
		f.add(fmt.Sprintf("v0.9.%d", i), false)
	}
	u := f.updater(t, t.TempDir(), &good)
	if _, err := u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	checked := u.Status().Checked
	f.failPage = "2"
	if _, err := u.Check(context.Background()); err == nil {
		t.Fatal("accepted a catalogue with a missing page")
	}
	f.failPage = ""
	for i := range 900 {
		f.add(fmt.Sprintf("v0.8.%d", i), false)
	}
	if _, err := u.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("accepted a truncated catalogue: %v", err)
	}
	if s := u.Status(); s.CheckError == "" || !s.Checked.Equal(checked) || !slices.Equal(tags(u.Releases("stable")), []string{"v1.2.0"}) {
		t.Errorf("previous catalogue lost: %+v", s)
	}
}

func TestInstallVerifiedBundle(t *testing.T) {
	// GNU sha256sum preserves ./ when the image builder uses ./*.raucb.
	for _, prefix := range []string{"", "./"} {
		t.Run("checksum-prefix="+prefix, func(t *testing.T) {
			f := newFake(t)
			f.sums["v1.2.0"] = strings.Replace(f.sums["v1.2.0"], f.asset, prefix+f.asset, 1)
			dir := t.TempDir()
			u := f.updater(t, dir, &good)
			var calls []int
			opts := manual
			opts.BeforeInstall = func() error {
				bundles, _ := filepath.Glob(filepath.Join(dir, "*.raucb"))
				calls = append(calls, len(bundles)+10*len(f.installed))
				return nil
			}
			if err := u.InstallRelease(context.Background(), "v1.2.0", opts); err != nil {
				t.Fatalf("install: %v", err)
			}
			// Before the download, then with the verified bundle before RAUC.
			if !slices.Equal(calls, []int{0, 1}) || !slices.Equal(f.installed, []string{"v1.2.0"}) {
				t.Fatalf("BeforeInstall calls %v, installed %v", calls, f.installed)
			}
			if bundles, _ := filepath.Glob(filepath.Join(dir, "*.raucb")); len(bundles) != 0 {
				t.Error("installed bundle kept")
			}
			s := u.Status()
			if s.State != "reboot" || s.Target != "v1.2.0" || s.PendingChannel != "stable" || s.PendingAuto || s.Suspended {
				t.Fatalf("status %+v", s)
			}
			// The journal survives a restart of nab-service.
			again := f.updater(t, dir, &good)
			if err := again.Reconcile(); err != nil || again.Status().State != "reboot" {
				t.Fatalf("after restart: %v %+v", err, again.Status())
			}
			if err := again.InstallRelease(context.Background(), "v1.2.0", retry); err == nil {
				t.Error("installed again before the restart")
			}
		})
	}
}

func TestRefusesUntrustedReleases(t *testing.T) {
	cases := map[string]struct {
		tag    string
		opts   InstallOptions
		mutate func(f *fakeGitHub)
	}{
		"bad tag":          {tag: "v1.2.0;reboot"},
		"not newer":        {tag: "v1.1.0"},
		"unknown channel":  {opts: InstallOptions{Channel: "nightly"}},
		"unpublished":      {tag: "v1.2.1"},
		"draft":            {mutate: func(f *fakeGitHub) { f.releases[0]["draft"] = true }},
		"preview":          {mutate: func(f *fakeGitHub) { f.releases[0]["prerelease"] = true }},
		"foreign url":      {mutate: func(f *fakeGitHub) { asset(f.releases[0], 0)["browser_download_url"] = "https://evil.example.org/b" }},
		"oversized sums":   {mutate: func(f *fakeGitHub) { asset(f.releases[0], 1)["size"] = 1 << 20 }},
		"bad checksum":     {mutate: func(f *fakeGitHub) { f.sums["v1.2.0"] = strings.Repeat("0", 64) + "  " + f.asset + "\n" }},
		"unlisted":         {mutate: func(f *fakeGitHub) { f.sums["v1.2.0"] = "" }},
		"traversal":        {mutate: func(f *fakeGitHub) { f.sums["v1.2.0"] = strings.Replace(f.sums["v1.2.0"], f.asset, "../"+f.asset, 1) }},
		"conflicting sums": {mutate: func(f *fakeGitHub) { f.sums["v1.2.0"] += strings.Repeat("0", 64) + "  " + f.asset + "\n" }},
		"evil redirect":    {mutate: func(f *fakeGitHub) { f.redirectElsewhere = true }},
		"window closed": {opts: InstallOptions{Channel: "stable", BeforeInstall: func() error {
			return errors.New("outside the update window")
		}}},
	}
	for name, c := range cases {
		f := newFake(t)
		if c.mutate != nil {
			c.mutate(f)
		}
		dir := t.TempDir()
		u := f.updater(t, dir, &good)
		if c.opts.Channel == "" {
			c.opts = manual
		}
		err := u.InstallRelease(context.Background(), cmpOr(c.tag, "v1.2.0"), c.opts)
		if err == nil || len(f.installed) != 0 || u.Status().PendingChannel != "" {
			t.Errorf("%s: accepted (err=%v)", name, err)
		}
		if parts, _ := filepath.Glob(filepath.Join(dir, "*.*")); name == "bad checksum" && len(parts) != 0 {
			t.Errorf("%s: corrupt download kept: %v", name, parts)
		}
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func TestInterruptedDownloadResumes(t *testing.T) {
	f := newFake(t)
	f.cutOnce.Store(true)
	dir := t.TempDir()
	u := f.updater(t, dir, &good)
	sum := strings.Fields(f.sums["v1.2.0"])[0]
	part := filepath.Join(dir, "v1.2.0-"+sum+".part")
	// Leftovers of a power cut: a completed bundle and downloads of another
	// version or of a re-published v1.2.0. Keeping them next to the new
	// download could exhaust a 16 GB card's data partition.
	stale := []string{f.asset, "v1.1.9-" + sum + ".part", "v1.2.0-" + strings.Repeat("0", 64) + ".part"}
	for _, name := range stale {
		os.WriteFile(filepath.Join(dir, name), []byte("previous bundle"), 0o600)
	}
	if err := u.InstallRelease(context.Background(), "v1.2.0", manual); err == nil || len(f.installed) != 0 {
		t.Fatalf("cut download must fail without installing: %v", err)
	}
	for _, name := range stale {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("stale %s kept alongside the partial download", name)
		}
	}
	fi, err := os.Stat(part)
	if err != nil || fi.Size() == 0 || fi.Size() >= int64(len(f.bundles["v1.2.0"])) {
		t.Fatalf("partial download not kept: %v", err)
	}
	if err := u.InstallRelease(context.Background(), "v1.2.0", manual); err != nil || len(f.installed) != 1 {
		t.Fatalf("resume: %v", err)
	}
	if !slices.Equal(f.ranges, []string{fmt.Sprintf("bytes=%d-", fi.Size())}) {
		t.Fatalf("expected one ranged request, got %q", f.ranges)
	}
}

func TestMisplacedRangeRestartsTheDownload(t *testing.T) {
	f := newFake(t)
	f.cutOnce.Store(true)
	f.badRange.Store(true)
	u := f.updater(t, t.TempDir(), &good)
	for range 2 { // cut, then a 206 answer for the wrong range
		if err := u.InstallRelease(context.Background(), "v1.2.0", manual); err == nil {
			t.Fatal("accepted a broken download")
		}
	}
	if parts, _ := filepath.Glob(filepath.Join(u.Dir, "*.part")); len(parts) != 0 {
		t.Fatal("kept a download misplaced by a wrong range")
	}
	if err := u.InstallRelease(context.Background(), "v1.2.0", manual); err != nil || !slices.Equal(f.ranges, []string{""}) {
		t.Fatalf("restart: %v %q", err, f.ranges)
	}
}

func TestUpdateOperationsDoNotOverlap(t *testing.T) {
	f := newFake(t)
	u := f.updater(t, t.TempDir(), &good)
	entered, finish := make(chan struct{}), make(chan struct{})
	u.Install = func(context.Context, string, func(int)) error {
		close(entered)
		<-finish
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- u.InstallRelease(context.Background(), "v1.2.0", manual) }()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("installation did not start: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("installation did not start")
	}
	if err := u.InstallRelease(context.Background(), "v1.2.0", manual); err == nil {
		t.Error("accepted overlapping installation")
	}
	if u.Reconcile() != nil || u.Status().State != "installing" {
		t.Error("reconcile disturbed the running installation")
	}
	// A check runs alongside and leaves the installation state alone.
	if _, err := u.Check(context.Background()); err != nil || u.Status().State != "installing" {
		t.Errorf("check during installation: %v %+v", err, u.Status())
	}
	close(finish)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestServiceRestartDuringRaucThenExplicitRetry(t *testing.T) {
	f := newFake(t)
	dir, boot := t.TempDir(), good
	u := f.updater(t, dir, &boot)
	ctx, cancel := context.WithCancel(context.Background())
	u.Install = func(ctx context.Context, _ string, _ func(int)) error {
		boot.Operation = "installing" // nab-service stops while RAUC works
		cancel()
		return ctx.Err()
	}
	if err := u.InstallRelease(ctx, "v1.2.0", manual); err == nil {
		t.Fatal("interrupted installation reported success")
	}
	bundles := func() int { b, _ := filepath.Glob(filepath.Join(dir, "*.raucb")); return len(b) }
	u = f.updater(t, dir, &boot)
	if err := u.Reconcile(); err != nil || u.Status().State != "installing" {
		t.Fatalf("restart during RAUC: %v %+v", err, u.Status())
	}
	if err := u.InstallRelease(context.Background(), "v1.2.0", retry); err == nil || bundles() != 1 {
		t.Fatalf("started over a running RAUC installation or removed its bundle: %v", err)
	}
	boot.Operation = "idle" // RAUC finished, its result is lost
	if err := u.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if s := u.Status(); s.State != "uncertain" || !s.Suspended || s.Target != "v1.2.0" {
		t.Fatalf("lost result: %+v", s)
	}
	u = f.updater(t, dir, &boot)
	if !u.Status().Suspended {
		t.Fatal("suspension not persisted")
	}
	for _, opts := range []InstallOptions{auto, manual} {
		if err := u.InstallRelease(context.Background(), "v1.2.0", opts); err == nil {
			t.Fatalf("%+v resumed an uncertain installation", opts)
		}
	}
	if err := u.InstallRelease(context.Background(), "v1.2.0", retry); err != nil || u.Status().State != "reboot" {
		t.Fatalf("explicit retry: %v %+v", err, u.Status())
	}
	// Restart into v1.2.0: confirmed only once the health check marks it good.
	boot = BootState{BootID: "boot-2", Slot: "B", Operation: "idle"}
	u = f.updater(t, dir, &boot)
	u.Current = "v1.2.0"
	f.add("v1.3.0", false)
	if err := u.Reconcile(); err != nil || u.Status().State != "confirming" || !u.Status().Suspended {
		t.Fatalf("unconfirmed boot: %v %+v", err, u.Status())
	}
	if err := u.InstallRelease(context.Background(), "v1.3.0", manual); err == nil {
		t.Fatal("installed over an unconfirmed boot")
	}
	boot.Health = "good"
	if err := u.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if s := u.Status(); s.State != "idle" || s.Suspended || s.PendingChannel != "" || s.Target != "v1.2.0" || !strings.Contains(s.LastResult, "updated from v1.1.0 to v1.2.0") {
		t.Fatalf("success: %+v", s)
	}
}

func TestRollbackBlocksAutomaticInstallation(t *testing.T) {
	f := newFake(t)
	dir, boot := t.TempDir(), good
	u := f.updater(t, dir, &boot)
	if err := u.InstallRelease(context.Background(), "v1.2.0", auto); err != nil || !u.Status().PendingAuto {
		t.Fatalf("automatic install: %v %+v", err, u.Status())
	}
	if _, err := u.Check(context.Background()); err != nil || u.Status().State != "reboot" {
		t.Fatal("check changed the installation state")
	}
	// U-Boot fell back to the previous slot.
	boot = BootState{BootID: "boot-2", Slot: "A", Health: "good", Operation: "idle"}
	u = f.updater(t, dir, &boot)
	u.Check(context.Background())
	if err := u.Reconcile(); err != nil {
		t.Fatal(err)
	}
	s, rels := u.Status(), u.Releases("stable")
	if s.State != "idle" || s.Suspended || s.PendingAuto || len(rels) != 1 || rels[0].Blocked == "" || !strings.Contains(s.LastResult, "back to v1.1.0") {
		t.Fatalf("rollback: %+v %+v", s, rels)
	}
	for _, opts := range []InstallOptions{auto, manual} {
		if err := u.InstallRelease(context.Background(), "v1.2.0", opts); err == nil {
			t.Fatalf("%+v reinstalled a rolled back release", opts)
		}
	}
	if err := u.InstallRelease(context.Background(), "v1.2.0", retry); err != nil {
		t.Fatalf("explicit retry: %v", err)
	}
}

func TestPowerCutDuringInstallationSuspendsAutomaticUpdates(t *testing.T) {
	f := newFake(t)
	dir, boot := t.TempDir(), good
	u := f.updater(t, dir, &boot)
	u.Install = func(context.Context, string, func(int)) error { select {} }
	go u.InstallRelease(context.Background(), "v1.2.0", auto)
	for u.Status().State != "installing" {
		time.Sleep(time.Millisecond)
	}
	// Power cut: the journal still says installing; the old slot boots.
	boot.BootID = "boot-2"
	u = f.updater(t, dir, &boot)
	if err := u.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if s := u.Status(); s.State != "idle" || !s.Suspended || s.PendingAuto || !strings.Contains(s.LastResult, "interrupted") {
		t.Fatalf("after power cut: %+v", s)
	}
	if err := u.InstallRelease(context.Background(), "v1.2.0", auto); err == nil {
		t.Fatal("automatic installation despite the suspension")
	}
	if err := u.InstallRelease(context.Background(), "v1.2.0", manual); err == nil {
		t.Fatal("manual installation did not require explicit retry")
	}
	if err := u.InstallRelease(context.Background(), "v1.2.0", retry); err != nil {
		t.Fatalf("manual retry: %v", err)
	}
}

func TestRaucRefusalBlocksTheRelease(t *testing.T) {
	f := newFake(t)
	u := f.updater(t, t.TempDir(), &good)
	u.Install = func(context.Context, string, func(int)) error { return errors.New("signature verification failed") }
	if err := u.InstallRelease(context.Background(), "v1.2.0", auto); err == nil {
		t.Fatal("refusal reported as success")
	}
	u.Check(context.Background())
	if s := u.Status(); s.State != "error" || s.PendingAuto || u.Releases("stable")[0].Blocked == "" {
		t.Fatalf("refusal: %+v", s)
	}
	if err := u.InstallRelease(context.Background(), "v1.2.0", auto); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("automatic retry of a refused release: %v", err)
	}
	u = f.updater(t, u.Dir, &good)
	if err := u.InstallRelease(context.Background(), "v1.2.0", retry); err != nil {
		t.Fatalf("explicit retry: %v", err)
	}
}

func TestBootStateGatesInstallations(t *testing.T) {
	f := newFake(t)
	for _, c := range []struct {
		boot         BootState
		current      string
		auto, manual bool
	}{
		{good, "v1.1.0", true, true},
		{BootState{Slot: "A", Health: "good", Operation: "idle"}, "v1.1.0", false, false},
		{BootState{BootID: "b", Slot: "unknown", Health: "good", Operation: "idle"}, "v1.1.0", false, false},
		{BootState{BootID: "b", Slot: "A", Operation: "idle"}, "v1.1.0", false, false},
		{BootState{BootID: "b", Slot: "A", Health: "stranded", Operation: "idle"}, "v1.1.0", false, true},
		{BootState{BootID: "b", Slot: "A", Health: "good", Operation: "installing"}, "v1.1.0", false, false},
		{good, "dev", false, true},
	} {
		for _, opts := range []InstallOptions{auto, manual} {
			boot := c.boot
			u := f.updater(t, t.TempDir(), &boot)
			u.Current = c.current
			err := u.InstallRelease(context.Background(), "v1.2.0", opts)
			if (err == nil) != (opts.Automatic && c.auto || !opts.Automatic && c.manual) {
				t.Errorf("%+v %s automatic=%v: %v", c.boot, c.current, opts.Automatic, err)
			}
			if c.current == "dev" && !u.Status().Suspended {
				t.Error("automatic updates not suspended on a development build")
			}
		}
	}
	for marker, want := range map[string]string{"good A\n": "good", "stranded A": "stranded", "good B": "", "good": "", "bad A": "", "": ""} {
		if got := markerHealth(marker, "A"); got != want {
			t.Errorf("marker %q: %q", marker, got)
		}
	}
}

func TestBootChangesAreNotConfirmedOrOverwritten(t *testing.T) {
	for _, change := range []string{"wrong slot", "wrong version", "stranded", "health changed during download"} {
		t.Run(change, func(t *testing.T) {
			f := newFake(t)
			dir, boot := t.TempDir(), good
			u := f.updater(t, dir, &boot)
			opts := auto
			if change == "health changed during download" {
				calls := 0
				opts.BeforeInstall = func() error {
					calls++
					if calls == 2 {
						boot.Health = ""
					}
					return nil
				}
				if err := u.InstallRelease(context.Background(), "v1.2.0", opts); err == nil || len(f.installed) != 0 {
					t.Fatal("installed after the health verdict changed", err)
				}
				return
			}
			if err := u.InstallRelease(context.Background(), "v1.2.0", opts); err != nil {
				t.Fatal(err)
			}
			boot = BootState{BootID: "boot-2", Slot: "B", Health: "good", Operation: "idle"}
			u = f.updater(t, dir, &boot)
			u.Current = "v1.2.0"
			switch change {
			case "wrong slot":
				boot.Slot = "A"
			case "wrong version":
				u.Current = "v1.1.0"
			case "stranded":
				boot.Health = "stranded"
			}
			if err := u.Reconcile(); err != nil {
				t.Fatal(err)
			}
			if st := u.Status(); strings.Contains(st.LastResult, "updated from") || st.State == "confirming" {
				t.Fatal("unexpected confirmation or unrecoverable pending update", st)
			}
		})
	}
}

func TestUnreadableJournalSuspendsAutomaticUpdates(t *testing.T) {
	f := newFake(t)
	for name, content := range map[string]string{"garbage": "{", "unknown phase": `{"pending":{"tag":"v1.2.0","phase":"done"}}`} {
		dir := t.TempDir()
		path := filepath.Join(dir, "state.json")
		os.WriteFile(path, []byte(content), 0o600)
		u := f.updater(t, dir, &good)
		if !u.Status().Suspended {
			t.Fatalf("%s: unreadable journal ignored", name)
		}
		archived, _ := filepath.Glob(path + ".unreadable-*")
		if len(archived) != 1 || !bytes.Equal(must(os.ReadFile(archived[0])), []byte(content)) {
			t.Fatalf("%s: journal not archived: %v", name, archived)
		}
		if !f.updater(t, dir, &good).Status().Suspended {
			t.Fatalf("%s: suspension not persisted", name)
		}
		if err := u.InstallRelease(context.Background(), "v1.2.0", auto); err == nil {
			t.Fatalf("%s: automatic installation", name)
		}
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestClaimWindow(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	u := f.updater(t, dir, &good)
	for i, c := range []struct {
		window string
		want   bool
	}{{"2026-09-29", true}, {"2026-09-29", false}, {"", false}} {
		if got, err := u.ClaimWindow(c.window); got != c.want || err != nil {
			t.Errorf("claim %d: %v %v", i, got, err)
		}
	}
	u = f.updater(t, dir, &good)
	if got, _ := u.ClaimWindow("2026-09-29"); got || u.Status().LastWindow != "2026-09-29" {
		t.Error("claimed window forgotten after a restart")
	}
	if got, _ := u.ClaimWindow("2026-09-30"); !got {
		t.Error("next window refused")
	}
}
