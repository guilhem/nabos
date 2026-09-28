package radio

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

var frame = []byte{0xFF, 0xFB, 0x90, 0x64, 1, 2, 3, 4}

// noType makes upstream omit the Content-Type header entirely.
const noType = "\x00none"

// upstream is a continuous stream: it sends first, then blocks until the
// client goes away, reported on closed.
func upstream(t *testing.T, ctype string, first []byte) (*httptest.Server, chan struct{}) {
	closed := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/404":
			http.NotFound(w, r)
			return
		case r.URL.Path == "/redirect-ok":
			http.Redirect(w, r, "/live", http.StatusFound)
			return
		case r.URL.Path == "/redirect-creds":
			http.Redirect(w, r, "http://u:p@"+r.Host+"/live", http.StatusFound)
			return
		case r.Header.Get("Authorization") != "":
			t.Error("credentials reached upstream")
		}
		if ctype == noType {
			w.Header()["Content-Type"] = nil // no header and no sniffing
		} else {
			w.Header().Set("Content-Type", ctype)
		}
		w.WriteHeader(http.StatusOK)
		w.Write(first)
		w.(http.Flusher).Flush()
		switch r.URL.Path {
		case "/finite": // clean end of body
			return
		case "/cut": // connection drops mid-stream
			c, _, _ := w.(http.Hijacker).Hijack()
			c.Close()
			return
		}
		<-r.Context().Done()
		closed <- struct{}{}
	}))
	t.Cleanup(srv.Close)
	return srv, closed
}

func waitClosed(t *testing.T, closed chan struct{}) {
	t.Helper()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream request not closed")
	}
}

var localRe = regexp.MustCompile(`^http://127\.0\.0\.1:(\d+)/radio/[A-Za-z0-9_-]{16,64}$`)

func refused(t *testing.T, local string) {
	t.Helper()
	u, _ := url.Parse(local)
	if c, err := net.DialTimeout("tcp", u.Host, time.Second); err == nil {
		c.Close()
		t.Errorf("listener %s still open", u.Host)
	}
}

// get fetches the local stream; the first bytes must arrive long before the
// default 15 s stall timeout ends the response.
func get(t *testing.T, local string) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, local, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(frame))
	if _, err := io.ReadFull(resp.Body, got); err != nil || !bytes.Equal(got, frame) {
		t.Fatalf("first bytes %x %v", got, err)
	}
	return resp
}

func TestContinuousStream(t *testing.T) {
	up, closed := upstream(t, "audio/mpeg", frame)
	r := New(nil)
	local, err := r.Start(context.Background(), up.URL+"/live?token=s3cret")
	if err != nil {
		t.Fatal(err)
	}
	m := localRe.FindStringSubmatch(local)
	if m == nil {
		t.Fatalf("local URL %s", local)
	}
	if p, _ := strconv.Atoi(m[1]); p < 1024 {
		t.Fatalf("port %d", p)
	}
	u, _ := url.Parse(local)
	if resp, err := http.Get("http://" + u.Host + "/radio/wrongtokenwrongtoken"); err != nil || resp.StatusCode != 404 {
		t.Fatalf("wrong token: %v %v", err, resp)
	}
	resp := get(t, local)
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "audio/mpeg" || len(resp.TransferEncoding) != 0 {
		t.Errorf("headers %v %v", resp.Header, resp.TransferEncoding)
	}
	refused(t, local) // single consumer: listener closed once claimed
	r.Close()
	waitClosed(t, closed)
	if _, err := io.ReadAll(resp.Body); err != nil && !strings.Contains(err.Error(), "reset") {
		t.Errorf("body end: %v", err)
	}
	if err := r.Err(); err != nil {
		t.Errorf("Close is not a failure: %v", err)
	}
	r.Close() // idempotent
}

// readToEnd plays a stream like the player: the first bytes, then until the
// relay ends the response.
func readToEnd(t *testing.T, r *Relay, raw string) {
	t.Helper()
	local, err := r.Start(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	resp := get(t, local)
	io.ReadAll(resp.Body)
	resp.Body.Close()
}

func TestErr(t *testing.T) {
	t.Run("natural end", func(t *testing.T) {
		up, _ := upstream(t, "audio/mpeg", frame)
		r := New(nil)
		readToEnd(t, r, up.URL+"/finite")
		if err := r.Err(); err != nil {
			t.Errorf("EOF is not a failure: %v", err)
		}
		r.Close()
	})
	t.Run("cut", func(t *testing.T) {
		up, _ := upstream(t, "audio/mpeg", frame)
		r := New(nil)
		defer r.Close()
		readToEnd(t, r, up.URL+"/cut?token=s3cret")
		// Checked as soon as the player sees the end, like the app does.
		err := r.Err()
		if err == nil || !strings.Contains(err.Error(), "interrupted") || strings.Contains(err.Error(), "s3cret") {
			t.Errorf("cut: %v", err)
		}
		if _, err := r.Start(context.Background(), up.URL); err != nil || r.Err() != nil {
			t.Errorf("new stream keeps old error: %v %v", err, r.Err())
		}
	})
	t.Run("stall", func(t *testing.T) {
		up, closed := upstream(t, "audio/mpeg", frame)
		r := New(nil)
		defer r.Close()
		r.StallTimeout = 100 * time.Millisecond
		readToEnd(t, r, up.URL)
		if err := r.Err(); err == nil || !strings.Contains(err.Error(), "stalled") {
			t.Errorf("stall: %v", err)
		}
		waitClosed(t, closed)
		r.Close()
		if err := r.Err(); err == nil {
			t.Error("Close cleared the stall error")
		}
	})
}

func TestStopPaths(t *testing.T) {
	t.Run("context", func(t *testing.T) {
		up, closed := upstream(t, "audio/mpeg", frame)
		ctx, cancel := context.WithCancel(context.Background())
		r := New(nil)
		local, err := r.Start(ctx, up.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp := get(t, local)
		cancel()
		waitClosed(t, closed)
		io.ReadAll(resp.Body)
		r.Close()
		if err := r.Err(); err != nil {
			t.Errorf("cancel is not a failure: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
		refused(t, local)
	})
	t.Run("player disconnect", func(t *testing.T) {
		up, closed := upstream(t, "audio/mpeg", frame)
		r := New(nil)
		defer r.Close()
		local, err := r.Start(context.Background(), up.URL)
		if err != nil {
			t.Fatal(err)
		}
		get(t, local).Body.Close()
		waitClosed(t, closed)
		r.Close()
		if err := r.Err(); err != nil {
			t.Errorf("player stop is not a failure: %v", err)
		}
	})
	t.Run("accept timeout", func(t *testing.T) {
		up, closed := upstream(t, "audio/mpeg", frame)
		r := New(nil)
		r.AcceptTimeout = 50 * time.Millisecond
		local, err := r.Start(context.Background(), up.URL)
		if err != nil {
			t.Fatal(err)
		}
		waitClosed(t, closed)
		time.Sleep(50 * time.Millisecond)
		refused(t, local)
		if err := r.Err(); err == nil || !strings.Contains(err.Error(), "did not connect") {
			t.Errorf("accept timeout: %v", err)
		}
	})
	t.Run("replaced", func(t *testing.T) {
		up, closed := upstream(t, "audio/mpeg", frame)
		r := New(nil)
		defer r.Close()
		first, err := r.Start(context.Background(), up.URL)
		if err != nil {
			t.Fatal(err)
		}
		second, err := r.Start(context.Background(), up.URL)
		if err != nil || second == first {
			t.Fatal(err, second)
		}
		waitClosed(t, closed)
		refused(t, first)
	})
	t.Run("stall", func(t *testing.T) {
		up, closed := upstream(t, "audio/mpeg", frame)
		r := New(nil)
		r.StallTimeout = 100 * time.Millisecond
		local, err := r.Start(context.Background(), up.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.Get(local)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body) // ends once the upstream stalls
		resp.Body.Close()
		if !bytes.Equal(b, frame) {
			t.Errorf("got %x", b)
		}
		waitClosed(t, closed)
	})
}

func TestFormats(t *testing.T) {
	for _, c := range []struct {
		ctype string
		first []byte
		err   string
	}{
		{"audio/mpeg; charset=x", nil, ""},
		{"application/octet-stream", []byte("ID3\x04"), ""},
		{"", frame, ""},
		{noType, frame, ""},
		{noType, []byte("ID3\x03"), ""},
		{noType, []byte{0xFF, 0xF1, 0x50, 0x80}, "MP3 required"},
		{noType, []byte("<html>"), "MP3 required"},
		{"", []byte("#EXTM3U\n"), "MP3 required"},
		{"not a/media type;;", frame, ""}, // unparsable: sniffed
		{"not a/media type;;", []byte("#EXTM3U\n"), "MP3 required"},
		{"application/octet-stream", []byte{0xFF, 0xF1, 0x50, 0x80}, "MP3 required"}, // ADTS AAC
		{"audio/aacp", frame, "AAC"},
		{"application/vnd.apple.mpegurl", nil, "HLS"},
		{"audio/x-scpls", nil, "playlists"},
		{"text/html", nil, "web pages"},
		{"audio/ogg", nil, "audio/ogg"},
	} {
		up, _ := upstream(t, c.ctype, c.first)
		r := New(nil)
		r.StallTimeout = 200 * time.Millisecond
		_, err := r.Start(context.Background(), up.URL)
		r.Close()
		if c.err == "" && err != nil || c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%q %x: %v, want %q", c.ctype, c.first, err, c.err)
		}
	}
	// Generic type and silence: stall while sniffing.
	up, closed := upstream(t, "application/octet-stream", nil)
	r := New(nil)
	r.StallTimeout = 100 * time.Millisecond
	if _, err := r.Start(context.Background(), up.URL); err == nil || !strings.Contains(err.Error(), "stalled") {
		t.Errorf("silent generic stream: %v", err)
	}
	waitClosed(t, closed)
}

func TestErrors(t *testing.T) {
	up, _ := upstream(t, "audio/mpeg", frame)
	r := New(nil)
	defer r.Close()
	for _, raw := range []string{"ftp://h/x", "http://u:p@h/x", "http://h:bad/", "not a url", up.URL + "/404", "http://127.0.0.1:1/s?token=s3cret"} {
		_, err := r.Start(context.Background(), raw)
		if err == nil || strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "u:p") {
			t.Errorf("%s: %v", raw, err)
		}
	}
	if _, err := r.Start(context.Background(), up.URL+"/404"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("404: %v", err)
	}
	if _, err := r.Start(context.Background(), up.URL+"/redirect-creds"); err == nil || !strings.Contains(err.Error(), "credentials") || strings.Contains(err.Error(), "u:p") {
		t.Errorf("redirect to credentials: %v", err)
	}
	if _, err := r.Start(context.Background(), up.URL+"/redirect-ok"); err != nil {
		t.Errorf("plain redirect: %v", err)
	}
}
