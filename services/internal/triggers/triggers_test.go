package triggers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestValidateURL(t *testing.T) {
	for raw, ok := range map[string]bool{
		"http://192.168.1.10:8123/api/webhook/x?y=1":      true,
		"HTTPS://example.com/hook":                        true,
		"http://[::1]:8080/":                              true,
		"ftp://example.com/":                              false,
		"file:///etc/passwd":                              false,
		"javascript:alert(1)":                             false,
		"http://user:pass@example.com/":                   false,
		"http://token@example.com/":                       false,
		"http:///path":                                    false,
		"http:example.com":                                false,
		"example.com/hook":                                false,
		"http://example.com:99999/":                       false,
		"http://example.com:0/":                           false,
		"http://exa mple.com/":                            false,
		"http://example.com/\n":                           false,
		"":                                                false,
		"http://example.com/" + strings.Repeat("a", 3000): false,
	} {
		if _, err := ValidateURL(raw); (err == nil) != ok {
			t.Errorf("ValidateURL(%q) = %v, want ok=%v", raw, err, ok)
		}
	}
}

func TestIFTTT(t *testing.T) {
	var path, query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, query = r.URL.EscapedPath(), r.URL.RawQuery
		if !strings.HasSuffix(path, "/with/key/good-KEY_1") {
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer srv.Close()
	c := NewClient()
	c.IFTTTURL = srv.URL
	if err := c.IFTTT(context.Background(), "good-KEY_1", "front door/../x", "d0:02:1a"); err != nil {
		t.Fatal(err)
	}
	if path != "/trigger/front%20door%2F..%2Fx/with/key/good-KEY_1" {
		t.Errorf("path %s", path)
	}
	if query != "value1=d0%3A02%3A1a&value2=%E2%9D%A4%EF%B8%8F&value3=%F0%9F%90%87" {
		t.Errorf("query %s", query)
	}
	err := c.IFTTT(context.Background(), "wrongkey", "ev", "u")
	if err == nil || strings.Contains(err.Error(), "wrongkey") || !strings.Contains(err.Error(), "401") {
		t.Errorf("bad key: %v", err)
	}
	for _, bad := range [][2]string{{"", "ev"}, {"k/../x", "ev"}, {"k?x", "ev"}, {"k", ""}, {"k", ".."}, {"k", "a\nb"}} {
		if c.IFTTT(context.Background(), bad[0], bad[1], "u") == nil {
			t.Errorf("accepted key %q event %q", bad[0], bad[1])
		}
	}
}

func TestWebhook(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Method + " " + r.URL.RequestURI()
		switch r.URL.Path {
		case "/fail":
			w.WriteHeader(http.StatusInternalServerError)
		case "/redirect-ok":
			http.Redirect(w, r, "/hook", http.StatusFound)
		case "/redirect-creds":
			http.Redirect(w, r, "http://u:p@"+r.Host+"/hook", http.StatusFound)
		case "/redirect-ftp":
			http.Redirect(w, r, "ftp://"+r.Host+"/hook", http.StatusFound)
		case "/redirect-loop":
			http.Redirect(w, r, "/redirect-loop", http.StatusFound)
		case "/huge":
			for range 1 << 10 { // 64 MiB unless the client stops reading
				if _, err := w.Write(make([]byte, 64<<10)); err != nil {
					return
				}
			}
		case "/slow":
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}
	}))
	defer srv.Close()
	c := NewClient()
	ctx := context.Background()
	if err := c.Webhook(ctx, srv.URL+"/hook?token=s3cret"); err != nil || got != "GET /hook?token=s3cret" {
		t.Fatalf("webhook: %v %q", err, got)
	}
	if err := c.Webhook(ctx, srv.URL+"/fail?token=s3cret"); err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("fail: %v", err)
	}
	if err := c.Webhook(ctx, "ftp://h/x"); err == nil {
		t.Error("ftp accepted")
	}
	if err := c.Webhook(ctx, srv.URL+"/redirect-ok"); err != nil || got != "GET /hook" {
		t.Errorf("plain redirect: %v %q", err, got)
	}
	for path, want := range map[string]string{"/redirect-creds": "credentials", "/redirect-ftp": "http or https", "/redirect-loop": "10 redirects"} {
		got = ""
		err := c.Webhook(ctx, srv.URL+path)
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "u:p") || got == "GET /hook" {
			t.Errorf("%s: %v (reached %q)", path, err, got)
		}
	}
	if c.HTTP.CheckRedirect != nil {
		t.Error("injected client mutated")
	}
	if err := c.Webhook(ctx, srv.URL+"/huge"); err != nil {
		t.Errorf("huge: %v", err)
	}
	c.HTTP.Timeout = 100 * time.Millisecond
	if err := c.Webhook(ctx, srv.URL+"/slow?token=s3cret"); err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("timeout: %v", err)
	}
	c.HTTP.Timeout = 0
	cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err := c.Webhook(cctx, srv.URL+"/slow?token=s3cret"); err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("cancel: %v", err)
	}
}
