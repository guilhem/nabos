// Package update finds releases on GitHub, downloads the RAUC bundle for this
// board and installs it with RAUC over D-Bus. RAUC verifies the bundle
// signature and compatibility; this package additionally refuses unexpected
// URLs, sizes and checksums before handing a local file to RAUC.
package update

import (
	"bufio"
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
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	MaxBundleSize = 2 << 30 // GitHub release asset limit
	SumsAsset     = "SHA256SUMS"
)

var (
	tagRe   = regexp.MustCompile(`^v(\d{1,6})\.(\d{1,6})\.(\d{1,6})$`)
	assetRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	repoRe  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)
	sumRe   = regexp.MustCompile(`^([0-9a-f]{64}) [ *](?:\./)?([A-Za-z0-9._-]{1,128})$`)
)

type Release struct {
	Tag       string
	BundleURL string
	Size      int64
	SumsURL   string
	Notes     string
}

type Status struct {
	Current   string
	Latest    string
	Available bool
	State     string // idle, checking, downloading, installing, reboot, error
	Progress  int
	Error     string
	Checked   time.Time
}

type Updater struct {
	Repo, Asset, Current string
	APIBase              string // https://api.github.com
	DownloadBase         string // https://github.com
	Dir                  string
	HTTP                 *http.Client
	// Install hands a verified local bundle to RAUC.
	Install func(ctx context.Context, path string, progress func(int)) error

	operation sync.Mutex // A check or install owns the lifecycle; Status stays readable.
	mu        sync.Mutex
	status    Status
	release   *Release
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
		status:       Status{Current: current, State: "idle"},
	}
}

func (u *Updater) Configured() bool {
	return repoRe.MatchString(u.Repo) && assetRe.MatchString(u.Asset)
}

func (u *Updater) Status() Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.status
}

func (u *Updater) set(fn func(*Status)) {
	u.mu.Lock()
	fn(&u.status)
	u.mu.Unlock()
}

func version(tag string) ([3]int, bool) {
	m := tagRe.FindStringSubmatch(tag)
	if m == nil {
		return [3]int{}, false
	}
	var v [3]int
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v, true
}

// Newer reports whether tag is a newer release than current (an unparsable
// current version, e.g. a development build, accepts any release).
func Newer(tag, current string) bool {
	t, ok := version(tag)
	if !ok {
		return false
	}
	c, _ := version(current)
	for i := range t {
		if t[i] != c[i] {
			return t[i] > c[i]
		}
	}
	return false
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

// Check queries the latest release.
func (u *Updater) Check(ctx context.Context) (*Release, error) {
	if !u.Configured() {
		return nil, errors.New("updates are not configured on this image")
	}
	if !u.operation.TryLock() {
		return nil, errors.New("an update operation is already in progress")
	}
	defer u.operation.Unlock()
	u.set(func(s *Status) { s.State, s.Error = "checking", "" })
	rel, err := u.check(ctx)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status.Checked = time.Now()
	u.status.State = "idle"
	if err != nil {
		u.status.State, u.status.Error = "error", err.Error()
		return nil, err
	}
	u.release = rel
	u.status.Latest = rel.Tag
	u.status.Available = Newer(rel.Tag, u.Current)
	return rel, nil
}

func (u *Updater) check(ctx context.Context) (*Release, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := u.get(ctx, fmt.Sprintf("%s/repos/%s/releases/latest", u.APIBase, u.Repo), map[string]string{"Accept": "application/vnd.github+json"})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API: HTTP %d", resp.StatusCode)
	}
	var r struct {
		Tag    string `json:"tag_name"`
		Body   string `json:"body"`
		Assets []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&r); err != nil {
		return nil, err
	}
	if _, ok := version(r.Tag); !ok {
		return nil, fmt.Errorf("unexpected release tag %q", r.Tag)
	}
	rel := &Release{Tag: r.Tag, Notes: r.Body}
	for _, a := range r.Assets {
		want := fmt.Sprintf("%s/%s/releases/download/%s/%s", u.DownloadBase, u.Repo, r.Tag, a.Name)
		switch a.Name {
		case u.Asset:
			if a.URL != want {
				return nil, fmt.Errorf("unexpected bundle URL %q", a.URL)
			}
			if a.Size <= 0 || a.Size > MaxBundleSize {
				return nil, fmt.Errorf("unexpected bundle size %d", a.Size)
			}
			rel.BundleURL, rel.Size = a.URL, a.Size
		case SumsAsset:
			if a.URL != want {
				return nil, fmt.Errorf("unexpected checksum URL %q", a.URL)
			}
			rel.SumsURL = a.URL
		}
	}
	if rel.BundleURL == "" || rel.SumsURL == "" {
		return nil, fmt.Errorf("release %s has no %s and %s", r.Tag, u.Asset, SumsAsset)
	}
	return rel, nil
}

func (u *Updater) checksum(ctx context.Context, rel *Release) (string, error) {
	resp, err := u.get(ctx, rel.SumsURL, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: HTTP %d", SumsAsset, resp.StatusCode)
	}
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 64<<10))
	for sc.Scan() {
		if m := sumRe.FindStringSubmatch(strings.TrimSpace(sc.Text())); m != nil && m[2] == u.Asset {
			return m[1], nil
		}
	}
	return "", fmt.Errorf("%s not listed in %s", u.Asset, SumsAsset)
}

// download fetches the bundle into dir, resuming a previous partial download.
func (u *Updater) download(ctx context.Context, rel *Release, sum string) (string, error) {
	if err := os.MkdirAll(u.Dir, 0o750); err != nil {
		return "", err
	}
	final := filepath.Join(u.Dir, u.Asset)
	// A power cut can leave the last completed bundle behind. Keep space for
	// one bundle, including a resumable partial download, on the data partition.
	if err := os.Remove(final); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	part := final + ".part"
	var offset int64
	if fi, err := os.Stat(part); err == nil && fi.Size() < rel.Size {
		offset = fi.Size()
	} else {
		os.Remove(part)
	}
	headers := map[string]string{}
	if offset > 0 {
		headers["Range"] = fmt.Sprintf("bytes=%d-", offset)
	}
	resp, err := u.get(ctx, rel.BundleURL, headers)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	flags := os.O_CREATE | os.O_WRONLY
	switch {
	case offset > 0 && resp.StatusCode == http.StatusPartialContent:
		flags |= os.O_APPEND
	case resp.StatusCode == http.StatusOK:
		offset = 0
		flags |= os.O_TRUNC
	default:
		return "", fmt.Errorf("bundle download: HTTP %d", resp.StatusCode)
	}
	f, err := os.OpenFile(part, flags, 0o640)
	if err != nil {
		return "", err
	}
	pr := &progressReader{r: io.LimitReader(resp.Body, rel.Size-offset+1), done: offset, total: rel.Size, fn: func(p int) {
		u.set(func(s *Status) { s.Progress = p })
	}}
	_, cerr := io.Copy(f, pr)
	if err := f.Close(); cerr == nil {
		cerr = err
	}
	if cerr != nil {
		return "", fmt.Errorf("download interrupted (will resume): %w", cerr)
	}
	fi, err := os.Stat(part)
	if err != nil {
		return "", err
	}
	if fi.Size() != rel.Size {
		if fi.Size() > rel.Size {
			os.Remove(part)
		}
		return "", fmt.Errorf("bundle size %d, expected %d", fi.Size(), rel.Size)
	}
	h := sha256.New()
	rf, err := os.Open(part)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(h, rf)
	rf.Close()
	if err != nil {
		return "", err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum {
		os.Remove(part)
		return "", fmt.Errorf("bundle checksum mismatch")
	}
	return final, os.Rename(part, final)
}

type progressReader struct {
	r           io.Reader
	done, total int64
	fn          func(int)
	last        int
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)
	if pc := int(p.done * 100 / p.total); pc != p.last {
		p.last = pc
		p.fn(pc)
	}
	return n, err
}

// InstallLatest downloads, verifies and installs the checked release.
func (u *Updater) InstallLatest(ctx context.Context) error {
	if !u.operation.TryLock() {
		return errors.New("an update operation is already in progress")
	}
	defer u.operation.Unlock()
	u.mu.Lock()
	rel := u.release
	u.mu.Unlock()
	if rel == nil || !Newer(rel.Tag, u.Current) {
		return errors.New("no newer release")
	}
	fail := func(err error) error {
		u.set(func(s *Status) { s.State, s.Error = "error", err.Error() })
		return err
	}
	u.set(func(s *Status) { s.State, s.Error, s.Progress = "downloading", "", 0 })
	sum, err := u.checksum(ctx, rel)
	if err != nil {
		return fail(err)
	}
	path, err := u.download(ctx, rel, sum)
	if err != nil {
		return fail(err)
	}
	defer os.Remove(path)
	u.set(func(s *Status) { s.State, s.Progress = "installing", 0 })
	if err := u.Install(ctx, path, func(p int) { u.set(func(s *Status) { s.Progress = p }) }); err != nil {
		return fail(err)
	}
	u.set(func(s *Status) { s.State, s.Progress = "reboot", 100 })
	return nil
}
