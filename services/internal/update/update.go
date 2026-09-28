// Package update lists the releases published on GitHub, downloads the RAUC
// bundle for this board and installs it with RAUC over D-Bus. RAUC verifies
// the bundle signature and compatibility; this package additionally refuses
// unexpected URLs, sizes and checksums before handing a local file to RAUC,
// and journals every installation (journal.go) so that a restart, a power cut
// or a rollback is recognised afterwards.
package update

import (
	"bufio"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	MaxBundleSize = 2 << 30 // GitHub release asset limit
	SumsAsset     = "SHA256SUMS"
	maxSumsSize   = 64 << 10
	// ponytail: cap the in-memory catalogue at 1000 releases; stream it if the repository outgrows this.
	maxPages     = 10
	stallTimeout = 2 * time.Minute // a download receiving nothing for this long is abandoned (and resumed later)
)

var (
	tagRe   = regexp.MustCompile(`^v(0|[1-9]\d{0,5})\.(0|[1-9]\d{0,5})\.(0|[1-9]\d{0,5})(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
	assetRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	repoRe  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)
	sumRe   = regexp.MustCompile(`^([0-9a-f]{64}) [ *](?:\./)?([A-Za-z0-9._-]{1,128})$`)

	errNotConfigured = errors.New("updates are not configured on this image")
	errBusy          = errors.New("an update operation is already in progress")
)

type Release struct {
	Tag        string
	BundleURL  string
	Size       int64
	SumsURL    string
	Notes      string
	Published  time.Time
	Prerelease bool   // flagged on GitHub or a SemVer prerelease tag
	Ready      bool   // bundle for this board and SHA256SUMS with the expected URLs and sizes
	Problem    string // why the release is not Ready
	Blocked    string // why automatic installation skips it; an explicit manual retry may install it
}

// Status of the updater. State is idle, downloading, installing, reboot
// (installed, restart to run it), confirming (running it, health not yet
// confirmed), uncertain (installation result unknown: manual retry) or error.
type Status struct {
	Current        string
	State          string
	Progress       int
	Error          string
	Checked        time.Time // last successful release check
	Checking       bool
	CheckError     string
	Target         string // release pending, being or last installed
	LastResult     string
	Suspended      bool // automatic installation suspended (manual installation still possible)
	RetryRequired  bool // explicitly acknowledge an uncertain previous installation
	PendingAuto    bool
	PendingChannel string
	LastWindow     string
}

type InstallOptions struct {
	Channel   string // "stable" or "test"
	Automatic bool
	// Retry explicitly resumes after an unknown result or a blocked release;
	// manual installations only.
	Retry bool
	// BeforeInstall, when set, runs before the download and again right
	// before RAUC; an error aborts the installation.
	BeforeInstall func() error
}

type Updater struct {
	Repo, Asset, Current string
	APIBase              string // https://api.github.com
	DownloadBase         string // https://github.com
	Dir                  string // downloads and the installation journal
	HTTP                 *http.Client
	// Install hands a verified local bundle to RAUC.
	Install func(ctx context.Context, path string, progress func(int)) error
	// Probe reads RAUC's operation, the booted slot and its health verdict.
	Probe func() (BootState, error)

	operation sync.Mutex // one installation (or reconciliation) at a time
	checking  sync.Mutex // one release check at a time
	mu        sync.Mutex // guards the fields below; Status stays readable
	status    Status
	catalog   []Release
	j         *journal
}

// AllowRedirects limits redirects to https on the given host suffixes.
func AllowRedirects(suffixes ...string) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "https" {
			return fmt.Errorf("refusing non-https redirect to %s", req.URL.Host)
		}
		h := req.URL.Hostname()
		for _, s := range suffixes {
			if h == s || strings.HasSuffix(h, "."+s) {
				return nil
			}
		}
		return fmt.Errorf("refusing redirect to %s", h)
	}
}

func New(repo, asset, current, dir string) *Updater {
	return &Updater{
		Repo: repo, Asset: asset, Current: current, Dir: dir,
		APIBase:      "https://api.github.com",
		DownloadBase: "https://github.com",
		HTTP:         &http.Client{Timeout: 0, CheckRedirect: AllowRedirects("github.com", "githubusercontent.com")},
		Install:      RaucInstall,
		Probe:        RaucProbe,
		status:       Status{State: "idle"},
	}
}

func (u *Updater) Configured() bool {
	return repoRe.MatchString(u.Repo) && assetRe.MatchString(u.Asset)
}

func (u *Updater) Status() Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	s, j := u.status, u.journal()
	s.Current, s.LastWindow, s.LastResult = u.Current, j.LastWindow, j.LastResult
	_, released := parseVersion(u.Current)
	s.Suspended = j.Suspended != "" || !released
	s.RetryRequired = j.Suspended != ""
	s.Target = cmp.Or(s.Target, j.Target)
	if p := j.Pending; p != nil {
		s.Target, s.PendingAuto, s.PendingChannel = p.Tag, p.Automatic, p.Channel
	}
	return s
}

func (u *Updater) set(fn func(*Status)) {
	u.mu.Lock()
	fn(&u.status)
	u.mu.Unlock()
}

func (u *Updater) fail(err error) error {
	u.set(func(s *Status) { s.State, s.Error = "error", err.Error() })
	return err
}

type semver struct {
	core [3]int
	pre  string
}

func parseVersion(tag string) (semver, bool) {
	m := tagRe.FindStringSubmatch(tag)
	if m == nil || len(tag) > 64 { // same bound as the image builder
		return semver{}, false
	}
	v := semver{pre: m[4]}
	for i := range v.core {
		v.core[i], _ = strconv.Atoi(m[i+1])
	}
	for _, part := range strings.Split(v.pre, ".") {
		if numeric(part) && len(part) > 1 && part[0] == '0' {
			return semver{}, false
		}
	}
	return v, true
}

// compare orders by SemVer precedence: 1.0.0-rc < 1.0.0-rc.2 < 1.0.0-rc.10 < 1.0.0.
func (a semver) compare(b semver) int {
	if c := slices.Compare(a.core[:], b.core[:]); c != 0 {
		return c
	}
	if a.pre == b.pre {
		return 0
	}
	if a.pre == "" {
		return 1
	}
	if b.pre == "" {
		return -1
	}
	x, y := strings.Split(a.pre, "."), strings.Split(b.pre, ".")
	for i := 0; i < min(len(x), len(y)); i++ {
		if x[i] == y[i] {
			continue
		}
		xn, yn := numeric(x[i]), numeric(y[i])
		if xn && !yn {
			return -1
		}
		if !xn && yn {
			return 1
		}
		if xn && len(x[i]) != len(y[i]) {
			return cmp.Compare(len(x[i]), len(y[i]))
		}
		return strings.Compare(x[i], y[i])
	}
	return cmp.Compare(len(x), len(y))
}

func numeric(s string) bool { return s != "" && strings.Trim(s, "0123456789") == "" }

// Newer reports whether tag is a newer release than current (an unparsable
// current version, e.g. a development build, accepts any release).
func Newer(tag, current string) bool {
	t, ok := parseVersion(tag)
	if !ok {
		return false
	}
	c, ok := parseVersion(current)
	return !ok || t.compare(c) > 0
}

func allowed(r Release, channel string) bool {
	return channel == "test" || channel == "stable" && !r.Prerelease
}

func (u *Updater) get(ctx context.Context, url string, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return u.HTTP.Do(req)
}

func (u *Updater) getJSON(ctx context.Context, url string, v any) error {
	resp, err := u.get(ctx, url, map[string]string{"Accept": "application/vnd.github+json"})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub API: HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(v)
}

type ghRelease struct {
	Tag        string    `json:"tag_name"`
	Body       string    `json:"body"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	Published  time.Time `json:"published_at"`
	Assets     []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// release validates GitHub's (untrusted) metadata; drafts and tags that are
// not vSemVer are no releases at all.
func (u *Updater) release(r ghRelease) (Release, bool) {
	v, ok := parseVersion(r.Tag)
	if !ok || r.Draft {
		return Release{}, false
	}
	rel := Release{Tag: r.Tag, Notes: r.Body, Published: r.Published, Prerelease: r.Prerelease || v.pre != ""}
	var problems []string
	for _, a := range r.Assets {
		if a.Name != u.Asset && a.Name != SumsAsset {
			continue
		}
		limit := int64(MaxBundleSize)
		if a.Name == SumsAsset {
			limit = maxSumsSize
		}
		switch {
		case a.URL != fmt.Sprintf("%s/%s/releases/download/%s/%s", u.DownloadBase, u.Repo, r.Tag, a.Name):
			problems = append(problems, "unexpected "+a.Name+" URL")
		case a.Size <= 0 || a.Size > limit:
			problems = append(problems, fmt.Sprintf("unexpected %s size %d", a.Name, a.Size))
		case a.Name == SumsAsset:
			rel.SumsURL = a.URL
		default:
			rel.BundleURL, rel.Size = a.URL, a.Size
		}
	}
	if len(problems) == 0 && rel.BundleURL == "" {
		problems = append(problems, "no "+u.Asset+" for this board")
	}
	if len(problems) == 0 && rel.SumsURL == "" {
		problems = append(problems, "no "+SumsAsset)
	}
	rel.Problem = strings.Join(problems, "; ")
	rel.Ready = rel.Problem == ""
	return rel, true
}

// Check loads every page of the release catalogue. On any error the previous
// catalogue is kept; the installation state is never changed.
func (u *Updater) Check(ctx context.Context) ([]Release, error) {
	if !u.Configured() {
		return nil, errNotConfigured
	}
	if !u.checking.TryLock() {
		return nil, errors.New("a release check is already in progress")
	}
	defer u.checking.Unlock()
	u.set(func(s *Status) { s.Checking = true })
	rels, err := u.fetchCatalog(ctx)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status.Checking = false
	if err != nil {
		u.status.CheckError = err.Error()
		return nil, err
	}
	u.catalog = rels
	u.status.Checked, u.status.CheckError = time.Now(), ""
	return u.annotate(rels, func(Release) bool { return true }), nil
}

func (u *Updater) fetchCatalog(ctx context.Context) ([]Release, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var rels []Release
	for page := 1; ; page++ {
		if page > maxPages {
			return nil, fmt.Errorf("more than %d releases: catalogue incomplete", maxPages*100)
		}
		var list []ghRelease
		if err := u.getJSON(ctx, fmt.Sprintf("%s/repos/%s/releases?per_page=100&page=%d", u.APIBase, u.Repo, page), &list); err != nil {
			return nil, fmt.Errorf("release catalogue page %d: %w", page, err)
		}
		for _, r := range list {
			if rel, ok := u.release(r); ok {
				rels = append(rels, rel)
			}
		}
		if len(list) < 100 {
			break
		}
	}
	slices.SortStableFunc(rels, func(a, b Release) int {
		va, _ := parseVersion(a.Tag)
		vb, _ := parseVersion(b.Tag)
		return vb.compare(va)
	})
	return rels, nil
}

// Releases returns the checked releases of channel ("stable" or "test")
// newer than the running version, newest first.
func (u *Updater) Releases(channel string) []Release {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.annotate(u.catalog, func(r Release) bool { return allowed(r, channel) && Newer(r.Tag, u.Current) })
}

// annotate copies the releases kept by keep with their blocking reason (u.mu held).
func (u *Updater) annotate(rels []Release, keep func(Release) bool) []Release {
	var out []Release
	for _, r := range rels {
		if keep(r) {
			r.Blocked = u.journal().Blocked[r.Tag]
			out = append(out, r)
		}
	}
	return out
}

func (u *Updater) fetchRelease(ctx context.Context, tag string) (Release, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var r ghRelease
	if err := u.getJSON(ctx, fmt.Sprintf("%s/repos/%s/releases/tags/%s", u.APIBase, u.Repo, tag), &r); err != nil {
		return Release{}, fmt.Errorf("release %s: %w", tag, err)
	}
	rel, ok := u.release(r)
	if !ok || rel.Tag != tag {
		return Release{}, fmt.Errorf("release %s is not published", tag)
	}
	return rel, nil
}

func (u *Updater) checksum(ctx context.Context, rel Release) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := u.get(ctx, rel.SumsURL, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: HTTP %d", SumsAsset, resp.StatusCode)
	}
	sum := ""
	sc := bufio.NewScanner(io.LimitReader(resp.Body, maxSumsSize))
	for sc.Scan() {
		m := sumRe.FindStringSubmatch(strings.TrimSpace(sc.Text()))
		if m == nil || m[2] != u.Asset {
			continue
		}
		if sum != "" && sum != m[1] {
			return "", fmt.Errorf("conflicting checksums for %s in %s", u.Asset, SumsAsset)
		}
		sum = m[1]
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	if sum == "" {
		return "", fmt.Errorf("%s not listed in %s", u.Asset, SumsAsset)
	}
	return sum, nil
}

// download fetches the bundle identified by its tag and checksum into Dir,
// resuming a partial download of that same bundle. RAUC must be idle: other
// downloads are removed to keep space for one bundle on the data partition.
func (u *Updater) download(ctx context.Context, rel Release, sum string) (string, error) {
	if err := os.MkdirAll(u.Dir, 0o750); err != nil {
		return "", err
	}
	name := rel.Tag + "-" + sum
	final, part := filepath.Join(u.Dir, name+".raucb"), filepath.Join(u.Dir, name+".part")
	entries, err := os.ReadDir(u.Dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		n := e.Name()
		if (strings.HasSuffix(n, ".raucb") || strings.HasSuffix(n, ".part")) && n != name+".raucb" && n != name+".part" {
			if err := os.Remove(filepath.Join(u.Dir, n)); err != nil {
				return "", err
			}
		}
	}
	if got, err := hashFile(final); err == nil && got == sum {
		return final, nil // verified earlier, installation was not attempted
	}
	os.Remove(final)
	var offset int64
	if fi, err := os.Stat(part); err == nil {
		offset = fi.Size()
	}
	if offset > rel.Size {
		os.Remove(part)
		offset = 0
	}
	if offset < rel.Size {
		if err := u.fetch(ctx, rel, part, offset); err != nil {
			return "", err
		}
	}
	got, err := hashFile(part)
	if err != nil {
		return "", err
	}
	if got != sum {
		os.Remove(part)
		return "", errors.New("bundle checksum mismatch")
	}
	return final, os.Rename(part, final)
}

func (u *Updater) fetch(ctx context.Context, rel Release, part string, offset int64) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	alive := time.AfterFunc(stallTimeout, cancel)
	defer alive.Stop()
	headers := map[string]string{}
	if offset > 0 {
		headers["Range"] = fmt.Sprintf("bytes=%d-", offset)
	}
	resp, err := u.get(ctx, rel.BundleURL, headers)
	if err != nil {
		return fmt.Errorf("download interrupted (will resume): %w", err)
	}
	defer resp.Body.Close()
	flags := os.O_CREATE | os.O_WRONLY
	switch {
	case offset > 0 && resp.StatusCode == http.StatusPartialContent:
		if resp.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", offset, rel.Size-1, rel.Size) {
			os.Remove(part)
			return errors.New("bundle download: unexpected range, will restart")
		}
		flags |= os.O_APPEND
	case resp.StatusCode == http.StatusOK:
		offset = 0
		flags |= os.O_TRUNC
	default:
		return fmt.Errorf("bundle download: HTTP %d", resp.StatusCode)
	}
	f, err := os.OpenFile(part, flags, 0o640)
	if err != nil {
		return err
	}
	pr := &progressReader{r: io.LimitReader(resp.Body, rel.Size-offset+1), done: offset, total: rel.Size, alive: alive, fn: func(p int) {
		u.set(func(s *Status) { s.Progress = p })
	}}
	_, cerr := io.Copy(f, pr)
	if err := f.Close(); cerr == nil {
		cerr = err
	}
	if cerr != nil {
		return fmt.Errorf("download interrupted (will resume): %w", cerr)
	}
	fi, err := os.Stat(part)
	if err != nil {
		return err
	}
	if fi.Size() != rel.Size {
		if fi.Size() > rel.Size {
			os.Remove(part)
		}
		return fmt.Errorf("bundle size %d, expected %d", fi.Size(), rel.Size)
	}
	return nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type progressReader struct {
	r           io.Reader
	done, total int64
	fn          func(int)
	last        int
	alive       *time.Timer
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.alive.Reset(stallTimeout)
	}
	p.done += int64(n)
	if pc := int(p.done * 100 / p.total); pc != p.last {
		p.last = pc
		p.fn(pc)
	}
	return n, err
}

// InstallRelease installs the release tagged tag, automatic and manual
// installations alike: it checks the boot and the journal, re-reads the
// release metadata, downloads and verifies the bundle, journals the attempt
// and hands the bundle to RAUC.
func (u *Updater) InstallRelease(ctx context.Context, tag string, opts InstallOptions) error {
	if !u.Configured() {
		return errNotConfigured
	}
	if !u.operation.TryLock() {
		return errBusy
	}
	defer u.operation.Unlock()
	boot, err := u.admit(tag, opts)
	if err != nil {
		return err
	}
	u.set(func(s *Status) { s.State, s.Error, s.Progress, s.Target = "downloading", "", 0, tag })
	path, sum, err := u.prepare(ctx, tag, opts)
	if err != nil {
		return u.fail(err)
	}
	// Downloads can take hours: the health verdict and RAUC operation may
	// have changed since admission. Check again before touching the other slot.
	if boot, err = u.admit(tag, opts); err != nil {
		return u.fail(err)
	}
	return u.install(ctx, boot, tag, path, sum, opts)
}

func before(opts InstallOptions) error {
	if opts.BeforeInstall == nil {
		return nil
	}
	return opts.BeforeInstall()
}

// prepare returns the verified bundle of tag and its checksum.
func (u *Updater) prepare(ctx context.Context, tag string, opts InstallOptions) (string, string, error) {
	if err := before(opts); err != nil {
		return "", "", err
	}
	rel, err := u.fetchRelease(ctx, tag)
	if err != nil {
		return "", "", err
	}
	if !allowed(rel, opts.Channel) {
		return "", "", fmt.Errorf("%s is a preview release, outside the %s channel", tag, opts.Channel)
	}
	if !rel.Ready {
		return "", "", fmt.Errorf("release %s: %s", tag, rel.Problem)
	}
	sum, err := u.checksum(ctx, rel)
	if err != nil {
		return "", "", err
	}
	path, err := u.download(ctx, rel, sum)
	if err != nil {
		return "", "", err
	}
	return path, sum, before(opts)
}

func (u *Updater) install(ctx context.Context, boot BootState, tag, path, sum string, opts InstallOptions) error {
	u.mu.Lock()
	err := u.commit(func(j *journal) {
		j.Pending = &pending{Tag: tag, SHA256: sum, From: u.Current, FromSlot: boot.Slot, ToSlot: otherSlot(boot.Slot), Channel: opts.Channel,
			BootID: boot.BootID, Phase: phaseInstalling, Automatic: opts.Automatic}
		j.Target = tag
	})
	u.mu.Unlock()
	if err != nil {
		return u.fail(fmt.Errorf("cannot record the installation: %w", err))
	}
	u.set(func(s *Status) { s.State, s.Progress = "installing", 0 })
	ierr := u.Install(ctx, path, func(p int) { u.set(func(s *Status) { s.Progress = p }) })
	after, perr := u.Probe()
	idle := perr == nil && after.Operation == "idle"
	if idle {
		os.Remove(path) // RAUC no longer reads it
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	switch {
	case ierr == nil:
		u.status.State, u.status.Progress = "reboot", 100
		err := u.commit(func(j *journal) {
			j.Pending.Phase = phaseInstalled
			j.LastResult = tag + " installed, restart to finish"
		})
		if err != nil {
			err = fmt.Errorf("%s installed but not recorded: %w", tag, err)
			u.status.Error = err.Error()
		}
		return err
	case idle && ctx.Err() == nil:
		msg := fmt.Sprintf("RAUC refused %s: %v", tag, ierr)
		err := u.commit(func(j *journal) {
			j.Pending = nil
			j.block(tag, ierr.Error())
			j.LastResult = msg
		})
		u.status.State, u.status.Error = "error", msg
		return errors.Join(errors.New(msg), err)
	default: // RAUC may still run, or its result is lost: reconcile decides.
		u.status.Error = ierr.Error()
		u.reconcileLocked(after, perr)
		return ierr
	}
}

// admit refuses installations the boot state or the journal forbid.
func (u *Updater) admit(tag string, o InstallOptions) (BootState, error) {
	_, released := parseVersion(u.Current)
	switch _, ok := parseVersion(tag); {
	case !ok:
		return BootState{}, fmt.Errorf("invalid release tag %q", tag)
	case !Newer(tag, u.Current):
		return BootState{}, fmt.Errorf("%s is not newer than %s", tag, u.Current)
	case o.Channel != "stable" && o.Channel != "test":
		return BootState{}, fmt.Errorf("unknown update channel %q", o.Channel)
	case o.Automatic && o.Retry:
		return BootState{}, errors.New("only a manual installation can be retried")
	case o.Automatic && !released:
		return BootState{}, fmt.Errorf("automatic updates need a released version, running %q", u.Current)
	}
	boot, err := u.reconcile()
	if err != nil {
		return boot, err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	j := u.journal()
	switch p := j.Pending; {
	case boot.BootID == "" || otherSlot(boot.Slot) == "":
		return boot, errors.New("unknown boot identity or A/B slot")
	case boot.Operation != "idle":
		return boot, errors.New("RAUC is busy")
	case boot.Health != "good" && (o.Automatic || boot.Health != "stranded"):
		return boot, errors.New("the running system is not confirmed healthy yet")
	case p != nil && p.Phase == phaseInstalled && p.BootID == boot.BootID:
		return boot, fmt.Errorf("%s is installed: restart to finish", p.Tag)
	case p != nil && !o.Retry:
		return boot, fmt.Errorf("the installation of %s is unresolved: retry explicitly", p.Tag)
	case o.Automatic && j.Suspended != "":
		return boot, errors.New("automatic updates are suspended: " + j.Suspended)
	case j.Suspended != "" && !o.Retry:
		return boot, errors.New("the previous result is uncertain: retry explicitly")
	case j.Blocked[tag] != "" && !o.Retry:
		return boot, fmt.Errorf("%s is blocked (%s): retry explicitly", tag, j.Blocked[tag])
	}
	return boot, nil
}
