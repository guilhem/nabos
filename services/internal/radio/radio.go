// Package radio relays one continuous MP3 HTTP stream to the core player
// through a private loopback listener: http://127.0.0.1:<port>/radio/<token>.
// Only the first matching GET is served; nothing touches the disk.
package radio

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/guilhem/nabos/services/internal/triggers"
)

type Relay struct {
	HTTP          *http.Client  // must not set Timeout: the body never ends
	AcceptTimeout time.Duration // player connection deadline after Start
	StallTimeout  time.Duration // per upstream read

	mu  sync.Mutex
	cur *stream
}

type stream struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu  sync.Mutex
	err error // first failure after Start succeeded
}

func (s *stream) fail(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
}

// New returns a relay; nil client uses one with dial and header timeouts.
func New(client *http.Client) *Relay {
	if client == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.ResponseHeaderTimeout = 10 * time.Second
		client = &http.Client{Transport: t}
	}
	return &Relay{HTTP: client, AcceptTimeout: 2 * time.Minute, StallTimeout: 15 * time.Second}
}

// Start stops any active stream, opens rawURL, checks it is MPEG audio and
// returns the loopback URL for the player. ctx bounds the whole stream.
func (r *Relay) Start(ctx context.Context, rawURL string) (string, error) {
	u, err := triggers.ValidateURL(rawURL)
	if err != nil {
		return "", fmt.Errorf("radio: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &stream{cancel: cancel, done: make(chan struct{})}
	r.mu.Lock()
	prev := r.cur
	r.cur = s
	r.mu.Unlock()
	if prev != nil {
		prev.stop()
	}
	local, err := r.open(ctx, s, u.String())
	if err != nil {
		cancel()
		close(s.done)
	}
	return local, err
}

// Close stops the active stream and waits for its listener and upstream
// request to be released. Err stays readable afterwards.
func (r *Relay) Close() {
	r.mu.Lock()
	s := r.cur
	r.mu.Unlock()
	if s != nil {
		s.stop()
	}
}

// Err reports why the latest stream ended early: stall, upstream read error
// or no player connection. It is set before the player sees the end of the
// stream. Normal end, Close, ctx cancellation and player disconnect give nil.
func (r *Relay) Err() error {
	r.mu.Lock()
	s := r.cur
	r.mu.Unlock()
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *stream) stop() {
	s.cancel()
	<-s.done
}

// stallReader cancels the stream when one upstream read takes too long and
// keeps the last upstream read error.
type stallReader struct {
	r   io.Reader
	t   *time.Timer
	d   time.Duration
	err error
}

func (s *stallReader) Read(p []byte) (int, error) {
	s.t.Reset(s.d)
	defer s.t.Stop()
	n, err := s.r.Read(p)
	if err != nil {
		s.err = err
	}
	return n, err
}

// flushWriter sends each chunk to the player immediately.
type flushWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err == nil {
		err = f.rc.Flush()
	}
	return n, err
}

func (r *Relay) open(ctx context.Context, s *stream, raw string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return "", errors.New("radio: invalid URL")
	}
	hc := *r.HTTP // validate every redirect hop, whatever client was injected
	hc.CheckRedirect = triggers.CheckRedirect
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("radio: %w", triggers.StripURL(err))
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return "", fmt.Errorf("radio: HTTP %d", resp.StatusCode)
	}
	stall := time.AfterFunc(time.Hour, func() {
		s.fail(errors.New("radio: stream stalled"))
		s.cancel()
	})
	stall.Stop()
	sr := &stallReader{r: resp.Body, t: stall, d: r.StallTimeout}
	body := bufio.NewReader(sr)
	if err := checkMPEG(resp.Header.Get("Content-Type"), body); err != nil {
		resp.Body.Close()
		if ctx.Err() != nil {
			err = errors.New("radio: stream stalled or cancelled")
		}
		return "", err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		resp.Body.Close()
		return "", fmt.Errorf("radio: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	path := "/radio/" + rand.Text() // 26 chars of [A-Z2-7]
	var once sync.Once
	srv := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			served := false
			if req.Method == http.MethodGet && req.URL.Path == path {
				once.Do(func() { served = true })
			}
			if !served {
				http.NotFound(w, req)
				return
			}
			ln.Close() // single consumer
			context.AfterFunc(req.Context(), s.cancel)
			w.Header().Set("Content-Type", "audio/mpeg")
			w.Header().Set("Transfer-Encoding", "identity") // raw body, close at end
			w.WriteHeader(http.StatusOK)
			io.Copy(flushWriter{w, http.NewResponseController(w)}, body)
			// Before returning, so the player's EOF comes after the error.
			if sr.err != nil && sr.err != io.EOF && ctx.Err() == nil {
				s.fail(fmt.Errorf("radio: stream interrupted: %w", triggers.StripURL(sr.err)))
			}
			s.cancel()
		}),
	}
	accept := time.AfterFunc(r.AcceptTimeout, func() {
		once.Do(func() {
			s.fail(errors.New("radio: player did not connect"))
			s.cancel()
		})
	})
	go srv.Serve(ln)
	go func() {
		<-ctx.Done()
		accept.Stop()
		stall.Stop()
		sctx, c := context.WithTimeout(context.Background(), time.Second)
		srv.Shutdown(sctx) // let a finished response flush
		c()
		srv.Close()
		ln.Close()
		resp.Body.Close()
		close(s.done)
	}()
	return fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil
}

// checkMPEG accepts declared MPEG audio, or generic types whose first bytes
// are an ID3 tag or an MPEG audio frame header (ADTS AAC has layer 00).
func checkMPEG(contentType string, body *bufio.Reader) error {
	mt, _, _ := mime.ParseMediaType(contentType)
	switch mt {
	case "audio/mpeg", "audio/mp3", "audio/mpeg3", "audio/x-mpeg", "audio/x-mp3", "audio/mpa", "audio/mpg":
		return nil
	case "", "application/octet-stream", "binary/octet-stream", "audio/x-unknown", "audio/unknown":
		b, err := body.Peek(3)
		if err != nil {
			return errors.New("radio: stream sent no audio")
		}
		if string(b) == "ID3" || b[0] == 0xFF && b[1]&0xE0 == 0xE0 && b[1]&0x06 != 0 {
			return nil
		}
		return errors.New("radio: unsupported stream format (MP3 required)")
	}
	switch {
	case strings.Contains(mt, "mpegurl"):
		return errors.New("radio: HLS or M3U playlists are not supported; use a direct MP3 stream URL")
	case strings.Contains(mt, "scpls") || strings.HasPrefix(mt, "text/"):
		return errors.New("radio: playlists and web pages are not supported; use a direct MP3 stream URL")
	case strings.Contains(mt, "aac") || mt == "audio/mp4":
		return errors.New("radio: AAC streams are not supported (MP3 required)")
	}
	return fmt.Errorf("radio: unsupported stream format %q (MP3 required)", mt)
}
