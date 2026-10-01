package main

import (
	"bytes"
	"crypto/sha256"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guilhem/nabos/services/internal/device"
)

func TestUpdateUIUsesRemoteOperationsAndConfig(t *testing.T) {
	a := testApp(t)
	f := appFixture(t, a)
	f.Mu.Lock()
	f.Configured = true
	f.Catalog = []device.Release{{Tag: "v1.1.0", Ready: true, Published: "2026-09-30T10:00:00Z"}}
	f.Mu.Unlock()
	cookie := serviceSession(t, a)
	h := a.routes()
	for _, path := range []string{"/updates/check", "/updates/install", "/updates/settings"} {
		if w := serviceRequest(h, "POST", path, url.Values{}, nil); w.Code != http.StatusUnauthorized {
			t.Fatal(path, w.Code)
		}
	}
	revision, _, _ := a.device.ReadConfig(a.ctx)
	form := url.Values{"revision": {revision}, "mode": {"auto"}, "channel": {"test"}, "start": {"23:00"}, "end": {"02:00"}}
	if w := serviceRequest(h, "POST", "/updates/settings", form, cookie); strings.Contains(w.Header().Get("Location"), "err=") {
		t.Fatal(w.Header())
	}
	if w := serviceRequest(h, "POST", "/updates/check", url.Values{}, cookie); strings.Contains(w.Header().Get("Location"), "err=") {
		t.Fatal(w.Header())
	}
	if w := serviceRequest(h, "POST", "/updates/install", url.Values{"tag": {"v1.1.0"}, "retry": {"true"}}, cookie); strings.Contains(w.Header().Get("Location"), "err=") {
		t.Fatal(w.Header())
	}
	f.Mu.Lock()
	if !f.Settings.Updates.Automatic || f.Checked != 1 || f.InstallTag != "v1.1.0" || f.InstallChannel != "test" || f.InstallAutomatic || !f.InstallRetry {
		t.Fatal("wrong remote arguments", f.Settings, f.InstallTag)
	}
	f.Mu.Unlock()
	page := serviceRequest(h, "GET", "/updates", nil, cookie)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "v1.1.0") || !strings.Contains(page.Body.String(), "30/09/2026") {
		t.Fatal(page.Code, page.Body.String())
	}
}

type updatePart struct {
	name, filename string
	body           io.Reader
}

// Interleave multipart headers with readers so large uploads never live in RAM.
func manualUpdateRequest(t *testing.T, parts ...updatePart) *http.Request {
	t.Helper()
	var headers bytes.Buffer
	w := multipart.NewWriter(&headers)
	var readers []io.Reader
	for _, part := range parts {
		var err error
		if part.filename != "" {
			_, err = w.CreateFormFile(part.name, part.filename)
		} else {
			_, err = w.CreateFormField(part.name)
		}
		if err != nil {
			t.Fatal(err)
		}
		readers = append(readers, strings.NewReader(headers.String()), part.body)
		headers.Reset()
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	readers = append(readers, strings.NewReader(headers.String()))
	r := httptest.NewRequest("POST", "/updates/upload", io.MultiReader(readers...))
	r.Header.Set("Content-Type", w.FormDataContentType())
	r.Header.Set("Origin", "http://example.com")
	return r
}

func assertNoUpdateStaging(t *testing.T, a *App) {
	t.Helper()
	files, err := os.ReadDir(filepath.Join(a.env.DataDir, "updates"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatal("staging files left behind", files)
	}
}

func TestManualUpdateAuthenticationAndCSRF(t *testing.T) {
	a := testApp(t)
	f := appFixture(t, a)
	cookie, h := serviceSession(t, a), a.routes()
	for _, tc := range []struct {
		name, origin string
		auth         bool
		want         int
	}{
		{"authentication", "http://example.com", false, http.StatusUnauthorized},
		{"missing origin", "", true, http.StatusForbidden},
		{"foreign origin", "http://other.example", true, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := manualUpdateRequest(t, updatePart{"bundle", "update.rauc", strings.NewReader("bundle")})
			r.Header.Set("Origin", tc.origin)
			if tc.auth {
				r.AddCookie(cookie)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatal(w.Code, w.Body.String())
			}
			assertNoUpdateStaging(t, a)
		})
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if f.BundleCalls != 0 {
		t.Fatal("unauthorized install", f.BundleCalls)
	}
}

func TestManualUpdateFlagsAndDescriptorOwnership(t *testing.T) {
	a := testApp(t)
	f := appFixture(t, a)
	cookie, h := serviceSession(t, a), a.routes()
	const content = "manual RAUC bundle content"
	for _, tc := range []struct {
		name, extension, ignore string
		before, retry           bool
	}{
		{"default verification", ".rauc", "", false, false},
		{"explicit bypass after file", ".raucb", "true", false, true},
		{"explicit bypass before file", ".rauc", "true", true, false},
		{"explicit verification", ".raucb", "false", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts := []updatePart{{"bundle", "update" + tc.extension, strings.NewReader(content)}}
			if tc.ignore != "" {
				flag := updatePart{"ignore_certificate", "", strings.NewReader(tc.ignore)}
				if tc.before {
					parts = append([]updatePart{flag}, parts...)
				} else {
					parts = append(parts, flag)
				}
			}
			if tc.retry {
				parts = append(parts, updatePart{"retry", "", strings.NewReader("true")})
			}
			r := manualUpdateRequest(t, parts...)
			r.AddCookie(cookie)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "ok=") {
				t.Fatal(w.Code, w.Header(), w.Body.String())
			}
			assertNoUpdateStaging(t, a)
			f.Mu.Lock()
			defer f.Mu.Unlock()
			if f.BundleIgnoreCertificate != (tc.ignore == "true") || f.BundleRetry != tc.retry || f.BundleSize != int64(len(content)) || f.BundleDigest != sha256.Sum256([]byte(content)) {
				t.Fatal("wrong descriptor content or flags", f.BundleSize, f.BundleIgnoreCertificate, f.BundleRetry)
			}
			// The frontend has closed and unlinked its file; the backend can still read.
			got, err := io.ReadAll(f.Bundle)
			if err != nil || string(got) != content {
				t.Fatal("backend lost descriptor ownership", string(got), err)
			}
		})
	}
}

type updateZeroReader struct{}

func (updateZeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestManualUpdateStreamsBeyondTmpCapacity(t *testing.T) {
	a := testApp(t)
	f := appFixture(t, a)
	cookie := serviceSession(t, a)
	// /tmp on the image is a 64 MiB tmpfs. A larger body must use /data/nabos.
	const size = 65 << 20
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	r := manualUpdateRequest(t,
		updatePart{"bundle", "update.raucb", io.LimitReader(updateZeroReader{}, size)},
		updatePart{"ignore_certificate", "", strings.NewReader("true")})
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "ok=") {
		t.Fatal(w.Code, w.Header())
	}
	assertNoUpdateStaging(t, a)
	files, err := os.ReadDir(tmp)
	if err != nil || len(files) != 0 {
		t.Fatal("unexpected multipart spill", files, err)
	}
	digest := sha256.New()
	io.Copy(digest, io.LimitReader(updateZeroReader{}, size))
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if f.BundleSize != size || !bytes.Equal(f.BundleDigest[:], digest.Sum(nil)) || !f.BundleIgnoreCertificate {
		t.Fatal("large upload truncated or flags lost", f.BundleSize, f.BundleIgnoreCertificate)
	}
}

func TestManualUpdateRejectsInvalidInputsAndCleansUp(t *testing.T) {
	a := testApp(t)
	f := appFixture(t, a)
	cookie, h := serviceSession(t, a), a.routes()
	for _, name := range []string{"missing file", "empty file", "extension", "two files", "invalid bypass", "empty bypass", "duplicate bypass", "duplicate retry", "oversized scalar", "file as scalar", "unknown field", "malformed body", "truncated file", "request limit", "chunked request limit"} {
		t.Run(name, func(t *testing.T) {
			parts := []updatePart{{"bundle", "update.rauc", strings.NewReader("bundle")}}
			switch name {
			case "missing file":
				parts = []updatePart{{"ignore_certificate", "", strings.NewReader("false")}}
			case "empty file":
				parts[0].body = strings.NewReader("")
			case "extension":
				parts[0].filename = "update.zip"
			case "two files":
				parts = append(parts, updatePart{"bundle", "second.raucb", strings.NewReader("bundle")})
			case "invalid bypass", "empty bypass", "oversized scalar":
				value := "on"
				if name == "empty bypass" {
					value = ""
				} else if name == "oversized scalar" {
					value = strings.Repeat("true", 1000)
				}
				parts = append(parts, updatePart{"ignore_certificate", "", strings.NewReader(value)})
			case "duplicate bypass", "duplicate retry":
				field := "ignore_certificate"
				if name == "duplicate retry" {
					field = "retry"
				}
				parts = append(parts, updatePart{field, "", strings.NewReader("false")}, updatePart{field, "", strings.NewReader("true")})
			case "file as scalar":
				parts = append(parts, updatePart{"ignore_certificate", "flag.txt", strings.NewReader("true")})
			case "unknown field":
				parts = append(parts, updatePart{"automatic", "", strings.NewReader("true")})
			}
			r := manualUpdateRequest(t, parts...)
			if name == "malformed body" {
				r.Body = io.NopCloser(strings.NewReader("broken multipart"))
			} else if name == "truncated file" {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				end := bytes.LastIndex(raw, []byte("\r\n--"))
				r.Body = io.NopCloser(bytes.NewReader(raw[:end]))
			} else if name == "request limit" {
				r.ContentLength = maxUpdateRequest + 1
			} else if name == "chunked request limit" {
				// Exercise MaxBytesReader without allocating or writing a 2 GiB file.
				r.Body = http.MaxBytesReader(httptest.NewRecorder(), r.Body, 220)
			}
			r.AddCookie(cookie)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "err=") {
				t.Fatal("invalid upload accepted", w.Code, w.Header())
			}
			assertNoUpdateStaging(t, a)
		})
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if f.BundleCalls != 0 {
		t.Fatal("invalid bundle reached D-Bus", f.BundleCalls)
	}
}

func TestManualUpdateRefusalCleansUpAndHidesRemoteError(t *testing.T) {
	a := testApp(t)
	f := appFixture(t, a)
	f.Mu.Lock()
	f.RejectBundle = true
	f.Mu.Unlock()
	r := manualUpdateRequest(t, updatePart{"bundle", "update.rauc", strings.NewReader("bundle")})
	r.AddCookie(serviceSession(t, a))
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	location, _ := url.Parse(w.Header().Get("Location"))
	if w.Code != http.StatusSeeOther || location.Query().Get("err") != device.ErrRefused.Error() {
		t.Fatal(w.Code, w.Header())
	}
	assertNoUpdateStaging(t, a)
}

func TestManualUpdateAvailabilityIgnoresCatalogErrors(t *testing.T) {
	a := testApp(t)
	f := appFixture(t, a)
	cookie, h := serviceSession(t, a), a.routes()
	for _, tc := range []struct {
		name, state                           string
		configured, catalogError, statusError bool
		checking, retry, disabled             bool
	}{
		{name: "unconfigured", state: "idle"},
		{name: "catalog outage", state: "idle", configured: true, catalogError: true},
		{name: "catalog checking", state: "idle", configured: true, checking: true},
		{name: "retry", state: "uncertain", retry: true},
		{name: "certificate refusal retry", state: "error", retry: true},
		{name: "downloading", state: "downloading", disabled: true},
		{name: "installing", state: "installing", disabled: true},
		{name: "confirming", state: "confirming", disabled: true},
		{name: "reboot without catalog", state: "reboot", disabled: true},
		{name: "core unavailable", state: "idle", statusError: true, disabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.Mu.Lock()
			f.Configured, f.FailReleases, f.FailUpdateStatus = tc.configured, tc.catalogError, tc.statusError
			f.UpdateState.State, f.UpdateState.Checking, f.UpdateState.RetryRequired = tc.state, tc.checking, tc.retry
			f.UpdateState.CheckError = "GitHub indisponible"
			calls := f.BundleCalls
			f.Mu.Unlock()
			page := serviceRequest(h, "GET", "/updates", nil, cookie)
			body := page.Body.String()
			if page.Code != http.StatusOK || !strings.Contains(body, `action="/updates/upload" enctype="multipart/form-data"`) || !strings.Contains(body, `accept=".rauc,.raucb" required`) || strings.Contains(body, `name="ignore_certificate" value="true" checked`) {
				t.Fatal(page.Code, body)
			}
			if strings.Contains(body, "<fieldset disabled>") != tc.disabled || strings.Contains(body, `name="retry" value="true"`) != tc.retry {
				t.Fatal("wrong manual availability or retry", body)
			}
			if tc.state == "reboot" && !strings.Contains(body, "Redémarrer sur la nouvelle version") {
				t.Fatal("reboot hidden without catalogue", body)
			}
			r := manualUpdateRequest(t, updatePart{"bundle", "update.rauc", strings.NewReader("bundle")})
			r.AddCookie(cookie)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if strings.Contains(w.Header().Get("Location"), "err=") != tc.disabled {
				t.Fatal("wrong admission", w.Header())
			}
			assertNoUpdateStaging(t, a)
			f.Mu.Lock()
			defer f.Mu.Unlock()
			want := calls
			if !tc.disabled {
				want++
			}
			if f.BundleCalls != want {
				t.Fatal("busy/status failure reached D-Bus", f.BundleCalls, want)
			}
		})
	}
}
