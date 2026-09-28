package mastodon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOAuthAndUserActions(t *testing.T) {
	var posts []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/apps":
			if r.FormValue("client_name") != "nabmastodond" || r.FormValue("scopes") != "read write" {
				t.Error("bad app registration")
			}
			fmt.Fprint(w, `{"client_id":"id","client_secret":"secret"}`)
		case "/oauth/token":
			if r.FormValue("code") != "code" || r.FormValue("client_secret") != "secret" {
				t.Error("bad code exchange")
			}
			fmt.Fprint(w, `{"access_token":"token"}`)
		case "/api/v1/accounts/verify_credentials":
			if r.Header.Get("Authorization") != "Bearer token" {
				t.Error("missing token")
			}
			fmt.Fprint(w, `{"id":"1","username":"rabbit","display_name":"Rabbit","avatar":"avatar"}`)
		case "/api/v1/statuses":
			if r.Header.Get("Authorization") != "Bearer token" || r.FormValue("visibility") != "direct" {
				t.Error("not a direct authenticated status")
			}
			posts = append(posts, r.Form)
			fmt.Fprintf(w, `{"id":"%d","created_at":"2026-01-01T00:00:00Z"}`, len(posts))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id, secret, err := c.RegisterApp(ctx, "http://localhost/cb")
	if err != nil || id != "id" || secret != "secret" {
		t.Fatalf("register: %q %q %v", id, secret, err)
	}
	auth, err := c.AuthorizeURL(id, "http://localhost/cb", "random-state")
	if err != nil || !strings.Contains(auth, "state=random-state") || !strings.Contains(auth, "scope=read+write") {
		t.Fatalf("authorize: %s %v", auth, err)
	}
	token, err := c.ExchangeCode(ctx, id, secret, "http://localhost/cb", "code")
	if err != nil || token != "token" {
		t.Fatalf("exchange: %q %v", token, err)
	}
	a, err := c.VerifyAccount(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	s := State{Instance: c.Instance()}
	if err := s.SetAccount(token, a); err != nil {
		t.Fatal(err)
	}
	if err := c.Propose(ctx, &s, "peer"); err != nil {
		t.Fatal(err)
	}
	if s.PairingState != "proposed" || s.SpouseHandle != "peer@127.0.0.1" {
		t.Fatalf("proposal state: %+v", s)
	}
	if err := c.SendEars(ctx, &s, 4, 6); err == nil {
		t.Fatal("ears before marriage accepted")
	}
	s.PairingState = "waiting_approval"
	if err := c.Accept(ctx, &s); err != nil {
		t.Fatal(err)
	}
	if err := c.SendEars(ctx, &s, 4, 6); err != nil {
		t.Fatal(err)
	}
	if err := c.Divorce(ctx, &s); err != nil {
		t.Fatal(err)
	}
	if s.PairingState != "" || s.SpouseHandle != "" {
		t.Fatalf("divorce state: %+v", s)
	}
	s.PairingState, s.SpouseHandle = "waiting_approval", "peer@127.0.0.1"
	if err := c.Reject(ctx, &s); err != nil {
		t.Fatal(err)
	}
	if len(posts) != 5 {
		t.Fatalf("posts: %d", len(posts))
	}
	for i, marker := range []string{"NabPairing Proposal", "NabPairing Acceptation", "NabPairing Ears 4 6", "NabPairing Divorce", "NabPairing Rejection"} {
		if !strings.Contains(posts[i].Get("status"), marker+" - "+protocolURL+")") {
			t.Errorf("bad protocol message: %q", posts[i].Get("status"))
		}
	}
	if _, err := c.AuthorizeURL(id, "http://localhost/cb", ""); err == nil {
		t.Error("accepted absent OAuth state")
	}
	if _, err := c.AuthorizeURL(id, "http://nabaztag.local:8080/services/mastodon/callback", "state"); err != nil {
		t.Errorf("local UI callback rejected: %v", err)
	}
	if _, err := c.AuthorizeURL(id, "http://public.example/cb", "state"); err == nil {
		t.Error("accepted public HTTP callback")
	}
	if _, err := NewClient("http://example.com", nil); err == nil {
		t.Error("accepted insecure remote instance")
	}
	if err := c.Propose(ctx, &s, "bad handle with spaces"); err == nil {
		t.Error("accepted bad handle")
	}
}

func TestTransitionAndRenderedMarkers(t *testing.T) {
	content := `<p>@rabbit Yup! (NabPairing Acceptation - <a href="https://github.com/nabaztag2018/pynab"><span class="invisible">https://</span><span>github.com/nabaztag2018/pynab</span></a>)</p>`
	kind, _, _, ok := ParseMessage(content)
	if !ok || kind != "acceptation" {
		t.Fatalf("parsed %q %v", kind, ok)
	}
	if _, _, _, ok := ParseMessage(`<p>(NabPairing Ears 17 0 - https://github.com/nabaztag2018/pynab)</p>`); ok {
		t.Error("accepted invalid ears")
	}
	if _, _, _, ok := ParseMessage(`<p>(NabPairing Proposal - https://evil.example/pynab)</p>`); ok {
		t.Error("accepted false marker")
	}
	now := time.Now().UTC()
	s, effects := Transition(State{}, "a@example.org", "proposal", 0, 0, now)
	if s.PairingState != "waiting_approval" || len(effects) != 1 || effects[0].Sound != "mastodon/proposal_received.mp3" {
		t.Fatalf("incoming proposal: %+v %+v", s, effects)
	}
	s, effects = Transition(s, "b@example.org", "proposal", 0, 0, now)
	if s.SpouseHandle != "b@example.org" || len(effects) != 2 || effects[0].To != "a@example.org" {
		t.Fatalf("replacement: %+v %+v", s, effects)
	}
	s, effects = Transition(s, "b@example.org", "acceptation", 0, 0, now)
	if s.PairingState != "" || len(effects) != 1 || effects[0].Reply != "divorce" {
		t.Fatalf("unsolicited acceptance: %+v %+v", s, effects)
	}
	s = State{SpouseHandle: "b@example.org", PairingState: "proposed"}
	s, effects = Transition(s, "b@example.org", "proposal", 0, 0, now)
	if s.PairingState != "married" || len(effects) != 2 || effects[0].Reply != "acceptation" {
		t.Fatalf("crossed proposal: %+v %+v", s, effects)
	}
	s, effects = Transition(s, "b@example.org", "ears", 4, 6, now)
	if s.LeftEar == nil || *s.LeftEar != 4 || effects[0].Sound != "mastodon/communion.wav" {
		t.Fatalf("spouse ears: %+v %+v", s, effects)
	}
	s, effects = Transition(s, "b@example.org", "divorce", 0, 0, now)
	if s.PairingState != "" || s.LeftEar != nil || effects[0].Sound != "mastodon/pairing_cancelled.mp3" {
		t.Fatalf("divorce: %+v %+v", s, effects)
	}
}

func TestCatchUpDedupMalformedAndTokenErrors(t *testing.T) {
	now := time.Now().UTC()
	var sendCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			http.Error(w, "unauthorized", 401)
			return
		}
		switch r.URL.Path {
		case "/api/v1/conversations":
			if r.URL.Query().Has("since_id") {
				t.Error("status cursor used as conversation cursor")
			}
			fmt.Fprintf(w, `[{"last_status":{"id":"41","created_at":%q,"visibility":"public","account":{"id":"2","acct":"peer"},"content":"(NabPairing Proposal - %s)"}},{"last_status":{"id":"42","created_at":%q,"visibility":"direct","account":{"id":"2","acct":"peer"},"content":"<p>(NabPairing Proposal - %s)</p>"}}]`, now.Format(time.RFC3339), protocolURL, now.Format(time.RFC3339), protocolURL)
		case "/api/v1/notifications":
			fmt.Fprint(w, `[]`)
		case "/api/v1/statuses":
			sendCount++
			fmt.Fprint(w, `{"id":"43","created_at":"2026-01-01T00:00:00Z"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, _ := NewClient(srv.URL, srv.Client())
	s := State{Instance: c.Instance(), AccessToken: "good", AccountID: "1", Username: "rabbit"}
	load := func() State { return s }
	update := func(f func(*State) error) error { return f(&s) }
	var sounds []string
	play := func(v string) { sounds = append(sounds, v) }
	if err := c.catchUp(context.Background(), "good", load, update, nil, play); err != nil {
		t.Fatal(err)
	}
	if s.PairingState != "waiting_approval" || s.LastStatusID != "42" || len(sounds) != 1 {
		t.Fatalf("catch-up: %+v %v", s, sounds)
	}
	if err := c.catchUp(context.Background(), "good", load, update, nil, play); err != nil {
		t.Fatal(err)
	}
	if len(sounds) != 1 {
		t.Error("duplicate proposal")
	}
	malformed := Status{ID: "garbage", Visibility: "direct", CreatedAt: now, Account: Account{ID: "2", Acct: "peer"}, Content: "<p>(NabPairing Divorce - " + protocolURL + ")</p>"}
	if err := c.process(context.Background(), "good", malformed, time.Time{}, update, nil, play); err != nil || s.PairingState != "waiting_approval" {
		t.Fatalf("malformed status changed state: %+v %v", s, err)
	}
	if _, err := c.VerifyAccount(context.Background(), "bad"); err == nil || strings.Contains(err.Error(), "bad") {
		t.Fatalf("token error: %v", err)
	}
	if sendCount != 0 {
		t.Fatal("unexpected reply")
	}
	s.AccessToken = "bad"
	ctx, cancel := context.WithCancel(context.Background())
	c.Run(ctx, load, update, nil, nil, func(err error) {
		if err != ErrUnauthorized && !strings.Contains(err.Error(), ErrUnauthorized.Error()) {
			t.Errorf("authorization error: %v", err)
		}
		cancel()
	})
	if s.AccessToken != "" || s.PairingState != "" {
		t.Fatalf("expired token retained state: %+v", s)
	}
}

func TestRunReconnect(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	var mu sync.Mutex
	var streams int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/conversations":
			fmt.Fprint(w, `[]`)
		case "/api/v1/notifications":
			fmt.Fprint(w, `[]`)
		case "/api/v1/streaming/direct":
			mu.Lock()
			streams++
			n := streams
			mu.Unlock()
			if n == 1 {
				http.Error(w, "outage", 503)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			v, _ := json.Marshal(Status{ID: "10", CreatedAt: mustTime(now), Visibility: "direct", Content: "<p>(NabPairing Ears 4 6 - " + protocolURL + ")</p>", Account: Account{ID: "2", Acct: "peer"}})
			fmt.Fprintf(w, "event: conversation\ndata: {\"last_status\":%s}\n\n", v)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, _ := NewClient(srv.URL, srv.Client())
	s := State{AccessToken: "token", AccountID: "1", Username: "rabbit", SpouseHandle: "peer@127.0.0.1", PairingState: "married"}
	load := func() State { mu.Lock(); defer mu.Unlock(); return s }
	update := func(f func(*State) error) error { mu.Lock(); defer mu.Unlock(); return f(&s) }
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	ears := make(chan [2]int, 2)
	errs := make(chan error, 2)
	done := make(chan struct{})
	go func() {
		c.Run(ctx, load, update, func(l, r int) { ears <- [2]int{l, r} }, nil, func(e error) { errs <- e })
		close(done)
	}()
	select {
	case got := <-ears:
		if got != [2]int{4, 6} {
			t.Fatalf("ears: %v", got)
		}
	case <-ctx.Done():
		t.Fatal("reconnect did not process ears")
	}
	select {
	case e := <-errs:
		if !strings.Contains(e.Error(), "HTTP 503") {
			t.Fatalf("error: %v", e)
		}
	default:
		t.Error("outage not reported")
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if streams < 2 || s.LastStatusID != "10" {
		t.Fatalf("reconnect streams=%d state=%+v", streams, s)
	}
}

func TestConversationPaginationFollowsLink(t *testing.T) {
	var pages int
	now := time.Now().UTC().Format(time.RFC3339)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/notifications" {
			fmt.Fprint(w, `[]`)
			return
		}
		pages++
		if pages == 1 {
			w.Header().Set("Link", `<`+"http://"+r.Host+`/api/v1/conversations?limit=40&max_id=900000>; rel="next"`)
			var conversations []map[string]any
			for id := 100; id > 60; id-- {
				conversations = append(conversations, map[string]any{"id": fmt.Sprint(id), "last_status": map[string]any{"id": fmt.Sprint(id + 100), "created_at": now, "visibility": "public"}})
			}
			json.NewEncoder(w).Encode(conversations)
			return
		}
		if r.URL.Query().Get("max_id") != "900000" {
			t.Errorf("wrong conversation cursor: %s", r.URL.RawQuery)
		}
		fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()
	c, _ := NewClient(srv.URL, srv.Client())
	s := State{AccessToken: "token"}
	if err := c.catchUp(context.Background(), "token", func() State { return s }, func(f func(*State) error) error { return f(&s) }, nil, nil); err != nil {
		t.Fatal(err)
	}
	if pages != 2 || s.LastStatusID != "200" {
		t.Fatalf("pages=%d cursor=%s", pages, s.LastStatusID)
	}
}

func TestStreamingHostDiscoveryDoesNotForwardToken(t *testing.T) {
	stream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/streaming/direct" || r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("wrong stream request: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: conversation\ndata: {\"last_status\":{\"id\":\"1\",\"created_at\":\"2026-01-01T00:00:00Z\"}}\n\n")
	}))
	defer stream.Close()
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("token sent during host discovery")
		}
		http.Redirect(w, r, stream.URL+"/api/v1/streaming", http.StatusFound)
	}))
	defer primary.Close()
	c, _ := NewClient(primary.URL, primary.Client())
	s := State{AccessToken: "token"}
	if err := c.stream(context.Background(), "token", func() State { return s }, func(f func(*State) error) error { return f(&s) }, nil, nil); err != nil {
		t.Fatal(err)
	}
	if s.LastStatusID != "1" {
		t.Fatalf("stream cursor: %q", s.LastStatusID)
	}
}

func TestConversationLinkRejectsOtherHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/notifications" {
			fmt.Fprint(w, `[]`)
			return
		}
		w.Header().Set("Link", `<https://other.example/api/v1/conversations?max_id=1>; rel="next"`)
		fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()
	c, _ := NewClient(srv.URL, srv.Client())
	s := State{AccessToken: "token"}
	if err := c.catchUp(context.Background(), "token", func() State { return s }, func(f func(*State) error) error { return f(&s) }, nil, nil); err == nil {
		t.Fatal("followed cross-host conversation link")
	}
}

func TestNotificationsRecoverIntermediateDirectMessages(t *testing.T) {
	now := time.Now().UTC()
	// The server received these statuses after ID 1, despite their earlier
	// creation dates on the peer's instance. Acceptance is on the next page.
	earlier := now.Add(-time.Hour).Format(time.RFC3339)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/conversations":
			fmt.Fprintf(w, `[{"last_status":{"id":"4","created_at":%q,"visibility":"direct","account":{"id":"peer-id","acct":"peer"},"content":"<p>(NabPairing Ears 4 6 - %s)</p>"}}]`, earlier, protocolURL)
		case "/api/v1/notifications":
			if r.URL.Query().Get("max_id") == "90" {
				fmt.Fprintf(w, `[{"type":"mention","created_at":%q,"status":{"id":"2","created_at":%q,"visibility":"direct","account":{"id":"peer-id","acct":"peer"},"content":"<p>(NabPairing Acceptation - %s)</p>"}}]`, earlier, earlier, protocolURL)
			} else {
				w.Header().Set("Link", `<http://`+r.Host+`/api/v1/notifications?max_id=90>; rel="next"`)
				fmt.Fprintf(w, `[{"type":"mention","created_at":%q,"status":{"id":"3","created_at":%q,"visibility":"direct","account":{"id":"peer-id","acct":"peer"},"content":"<p>(NabPairing Ears 2 4 - %s)</p>"}}]`, earlier, earlier, protocolURL)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, _ := NewClient(srv.URL, srv.Client())
	s := State{AccessToken: "token", AccountID: "self", Username: "rabbit", SpouseHandle: "peer@127.0.0.1", PairingState: "proposed", LastStatusID: "1", LastStatusDate: now}
	var ears [][2]int
	for range 2 { // Reconnection must not replay any of these statuses.
		if err := c.catchUp(context.Background(), "token", func() State { return s }, func(f func(*State) error) error { return f(&s) }, func(l, r int) { ears = append(ears, [2]int{l, r}) }, nil); err != nil {
			t.Fatal(err)
		}
	}
	if s.PairingState != "married" || s.LastStatusID != "4" || len(ears) != 2 || ears[0] != [2]int{2, 4} || ears[1] != [2]int{4, 6} {
		t.Fatalf("recovery: state=%+v ears=%v", s, ears)
	}
}

func TestFreshAccountSkipsHistoryThenAcceptsDelayedDelivery(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour)
	status := func(id, kind string) Status {
		return Status{ID: id, CreatedAt: old, Visibility: "direct", Account: Account{ID: "peer-id", Acct: "peer"},
			Content: "<p>(NabPairing " + kind + " - " + protocolURL + ")</p>"}
	}
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/conversations":
			fmt.Fprint(w, `[]`)
		case "/api/v1/notifications":
			pages++
			w.Header().Set("Link", `<http://`+r.Host+`/api/v1/notifications?max_id=1>; rel="next"`)
			json.NewEncoder(w).Encode([]any{
				map[string]any{"type": "mention", "created_at": old, "status": status("2", "Acceptation")},
				map[string]any{"type": "mention", "created_at": old, "status": status("1", "Proposal")},
			})
		default:
			t.Errorf("historical message triggered a request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, _ := NewClient(srv.URL, srv.Client())
	var s State
	if err := s.SetAccount("token", Account{ID: "self", Username: "rabbit"}); err != nil {
		t.Fatal(err)
	}
	update := func(f func(*State) error) error { return f(&s) }
	var sounds []string
	play := func(sound string) { sounds = append(sounds, sound) }
	if err := c.catchUp(context.Background(), "token", func() State { return s }, update, nil, play); err != nil {
		t.Fatal(err)
	}
	if pages != 1 || s.LastStatusID != "2" || s.PairingState != "" || len(sounds) != 0 {
		t.Fatalf("history replayed: pages=%d state=%+v sounds=%v", pages, s, sounds)
	}
	for range 2 {
		if err := c.process(context.Background(), "token", status("3", "Proposal"), time.Time{}, update, nil, play); err != nil {
			t.Fatal(err)
		}
	}
	if s.LastStatusID != "3" || s.PairingState != "waiting_approval" || len(sounds) != 1 {
		t.Fatalf("new delayed delivery: state=%+v sounds=%v", s, sounds)
	}
}

func mustTime(v string) time.Time { t, _ := time.Parse(time.RFC3339, v); return t }
