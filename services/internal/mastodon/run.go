package mastodon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/html"
)

type Load func() State
type Update func(func(*State) error) error

// Effect describes protocol side effects from a pure transition.
type Effect struct {
	Reply string // NabPairing message type
	To    string
	Sound string // resource path, if any
	Left  *int
	Right *int
}

var marker = regexp.MustCompile(`\(NabPairing (Proposal|Acceptation|Rejection|Divorce|Ears ([0-9]{1,2}) ([0-9]{1,2})) - https://github\.com/nabaztag2018/pynab\)`)

// ParseMessage recognizes the exact pynab marker in Mastodon's rendered HTML.
func ParseMessage(content string) (kind string, left, right int, ok bool) {
	var b strings.Builder
	z := html.NewTokenizer(strings.NewReader(content))
	skip := false
	for {
		t := z.Next()
		if t == html.ErrorToken {
			break
		}
		if t == html.StartTagToken {
			name, _ := z.TagName()
			if string(name) == "script" || string(name) == "style" {
				skip = true
			}
		} else if t == html.EndTagToken {
			name, _ := z.TagName()
			if string(name) == "script" || string(name) == "style" {
				skip = false
			}
		} else if t == html.TextToken && !skip {
			b.Write(z.Text())
		}
	}
	m := marker.FindStringSubmatch(b.String())
	if m == nil {
		return "", 0, 0, false
	}
	if m[2] != "" {
		fmt.Sscanf(m[2]+" "+m[3], "%d %d", &left, &right)
		if validEars(left, right) != nil {
			return "", 0, 0, false
		}
		return "ears", left, right, true
	}
	return strings.ToLower(m[1]), 0, 0, true
}

func sound(name string) Effect     { return Effect{Sound: "mastodon/" + name + ".mp3"} }
func reply(to, kind string) Effect { return Effect{To: to, Reply: kind} }

// Transition applies one trusted direct message. The caller validates the
// sender, visibility, date and cursor before invoking it.
func Transition(s State, sender, kind string, left, right int, date time.Time) (State, []Effect) {
	if kind == "ears" && validEars(left, right) != nil {
		return s, nil
	}
	matching := s.SpouseHandle != "" && strings.EqualFold(s.SpouseHandle, sender)
	var effects []Effect
	reset := func() { s.clearPairing(); s.PairingDate = date }
	setDate := func() { s.PairingDate = date }
	switch s.PairingState {
	case "":
		switch kind {
		case "proposal":
			s.SpouseHandle, s.PairingState = sender, "waiting_approval"
			setDate()
			effects = append(effects, sound("proposal_received"))
		case "acceptation", "ears":
			effects = append(effects, reply(sender, "divorce"))
		}
	case "proposed":
		switch {
		case matching && (kind == "rejection" || kind == "divorce"):
			reset()
			effects = append(effects, sound("proposal_refused"))
		case matching && (kind == "acceptation" || kind == "proposal"):
			if kind == "proposal" {
				effects = append(effects, reply(sender, "acceptation"))
			}
			s.PairingState = "married"
			setDate()
			effects = append(effects, sound("proposal_accepted"))
		case !matching && (kind == "acceptation" || kind == "ears"):
			effects = append(effects, reply(sender, "divorce"))
		case !matching && kind == "proposal":
			effects = append(effects, reply(sender, "rejection"))
		}
	case "waiting_approval":
		switch {
		case matching && kind == "rejection":
			reset()
		case matching && kind == "divorce":
			reset()
			effects = append(effects, sound("pairing_cancelled"))
		case matching && kind == "acceptation":
			reset()
			effects = append(effects, reply(sender, "divorce"))
		case kind == "proposal":
			if !matching {
				effects = append(effects, reply(s.SpouseHandle, "rejection"))
				s.SpouseHandle = sender
			}
			setDate()
			effects = append(effects, sound("proposal_received"))
		case !matching && (kind == "acceptation" || kind == "ears"):
			effects = append(effects, reply(sender, "divorce"))
		}
	case "married":
		switch {
		case matching && (kind == "rejection" || kind == "divorce"):
			reset()
			effects = append(effects, sound("pairing_cancelled"))
		case matching && kind == "acceptation":
			setDate()
		case matching && kind == "proposal":
			setDate()
			effects = append(effects, reply(sender, "acceptation"))
		case matching && kind == "ears":
			s.LeftEar, s.RightEar = &left, &right
			setDate()
			effects = append(effects, Effect{Sound: "mastodon/communion.wav", Left: &left, Right: &right})
		case !matching && (kind == "acceptation" || kind == "ears"):
			effects = append(effects, reply(sender, "divorce"))
		case !matching && kind == "proposal":
			effects = append(effects, reply(sender, "rejection"))
		}
	}
	return s, effects
}

// Run catches up from conversations, then reads Mastodon's user SSE stream.
// It reconnects after outages and checks for changed credentials each cycle.
// Update must persist a successful mutation before returning.
func (c *Client) Run(ctx context.Context, load Load, update Update, onEars func(int, int), onSound func(string), onError func(error)) {
	if load == nil || update == nil {
		if onError != nil {
			onError(errors.New("Mastodon state callbacks required"))
		}
		return
	}
	if s := load(); s.PairingState == "married" && s.LeftEar != nil && s.RightEar != nil && onEars != nil {
		if validEars(*s.LeftEar, *s.RightEar) == nil {
			onEars(*s.LeftEar, *s.RightEar)
		}
	}
	for ctx.Err() == nil {
		s := load()
		if s.AccessToken == "" {
			if !pause(ctx, 5*time.Second) {
				return
			}
			continue
		}
		if err := c.catchUp(ctx, s.AccessToken, load, update, onEars, onSound); err != nil {
			if errors.Is(err, ErrUnauthorized) {
				_ = update(func(current *State) error {
					if current.AccessToken == s.AccessToken {
						current.ClearAccount()
					}
					return nil
				})
			}
			if ctx.Err() == nil && onError != nil {
				onError(err)
			}
			if !pause(ctx, 15*time.Second) {
				return
			}
			continue
		}
		if err := c.stream(ctx, s.AccessToken, load, update, onEars, onSound); err != nil && ctx.Err() == nil {
			if errors.Is(err, ErrUnauthorized) {
				_ = update(func(current *State) error {
					if current.AccessToken == s.AccessToken {
						current.ClearAccount()
					}
					return nil
				})
			}
			if onError != nil {
				onError(err)
			}
		}
		if !pause(ctx, time.Second) {
			return
		}
	}
}

func pause(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (c *Client) catchUp(ctx context.Context, token string, load Load, update Update, onEars func(int, int), onSound func(string)) error {
	var statuses []Status
	cursor, _ := statusNumber(load().LastStatusID)
	path := "/api/v1/conversations?limit=40"
	// ponytail: cap at 4000 conversations; use a durable page cursor if this ceiling is reached.
	for page := 0; page < 100; page++ {
		var conversations []struct {
			LastStatus *Status `json:"last_status"`
		}
		next, err := c.page(ctx, token, path, &conversations)
		if err != nil {
			return err
		}
		for _, v := range conversations {
			if v.LastStatus == nil {
				continue
			}
			statuses = append(statuses, *v.LastStatus)
		}
		if next == "" {
			break
		}
		if page == 99 {
			return errors.New("Mastodon conversation catch-up limit reached")
		}
		if next == path {
			return errors.New("Mastodon conversation pagination did not advance")
		}
		path = next
	}
	path = "/api/v1/notifications?limit=40&types%5B%5D=mention"
	for page := 0; page < 100; page++ {
		var notifications []struct {
			Type   string  `json:"type"`
			Status *Status `json:"status"`
		}
		next, err := c.page(ctx, token, path, &notifications)
		if err != nil {
			return err
		}
		older := len(notifications) > 0
		for _, n := range notifications {
			if n.Status == nil {
				older = false
				continue
			}
			id, err := statusNumber(n.Status.ID)
			if err != nil || id > cursor {
				older = false
			}
			if n.Type == "mention" {
				statuses = append(statuses, *n.Status)
			}
		}
		if next == "" || older {
			break
		}
		if page == 99 {
			return errors.New("Mastodon notification catch-up limit reached")
		}
		if next == path {
			return errors.New("Mastodon notification pagination did not advance")
		}
		path = next
	}
	sort.Slice(statuses, func(i, j int) bool {
		a, _ := statusNumber(statuses[i].ID)
		b, _ := statusNumber(statuses[j].ID)
		return a < b
	})
	for _, st := range statuses {
		if err := c.process(ctx, token, st, load, update, onEars, onSound); err != nil {
			return err
		}
	}
	return nil
}

var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="?next"?`)

func (c *Client) page(ctx context.Context, token, path string, result any) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return "", ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Mastodon %s: HTTP %d", strings.Split(path, "?")[0], resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(result); err != nil {
		return "", err
	}
	m := nextLink.FindStringSubmatch(resp.Header.Get("Link"))
	if m == nil {
		return "", nil
	}
	u, err := req.URL.Parse(m[1])
	if err != nil || u.Scheme != req.URL.Scheme || u.Host != req.URL.Host || u.Path != strings.Split(path, "?")[0] || u.User != nil {
		return "", errors.New("invalid Mastodon conversation page URL")
	}
	return u.RequestURI(), nil
}

func (c *Client) process(ctx context.Context, token string, st Status, load Load, update Update, onEars func(int, int), onSound func(string)) error {
	id, err := statusNumber(st.ID)
	if err != nil || st.CreatedAt.IsZero() {
		return nil
	}
	var effects []Effect
	err = update(func(s *State) error {
		if s.AccessToken != token {
			return nil
		}
		if old, e := statusNumber(s.LastStatusID); s.LastStatusID != "" && e == nil && id <= old {
			return nil
		}
		// Federated delivery and remote clocks can put newer IDs before the
		// last processed creation date. Only the local ID orders delivery.
		if st.Visibility == "direct" && st.Account.ID != "" && st.Account.ID != s.AccountID {
			sender, e := c.handle(st.Account.Acct)
			if e == nil && !strings.EqualFold(sender, s.Username+"@"+c.Instance()) {
				if kind, left, right, ok := ParseMessage(st.Content); ok {
					next, pending := Transition(*s, sender, kind, left, right, st.CreatedAt)
					for _, effect := range pending {
						if effect.Reply != "" {
							if _, err := c.send(ctx, token, effect.To, effect.Reply, 0, 0); err != nil {
								return err
							}
						}
					}
					*s, effects = next, pending
				}
			}
		}
		s.LastStatusID = st.ID
		if st.CreatedAt.After(s.LastStatusDate) {
			s.LastStatusDate = st.CreatedAt
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, e := range effects {
		if e.Sound != "" && onSound != nil {
			onSound(e.Sound)
		}
		if e.Left != nil && e.Right != nil && onEars != nil {
			onEars(*e.Left, *e.Right)
		}
	}
	return nil
}

func (c *Client) stream(ctx context.Context, token string, load Load, update Update, onEars func(int, int), onSound func(string)) error {
	streamCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	base, err := c.streamingBase(streamCtx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, base+"/api/v1/streaming/direct", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	h := *c.http
	h.Timeout = 0
	resp, err := h.Do(req)
	if err != nil {
		if streamCtx.Err() == context.DeadlineExceeded {
			return nil
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Mastodon stream: HTTP %d", resp.StatusCode)
	}
	scanner := bufio.NewScanner(io.LimitReader(resp.Body, 8<<20))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var event, data string
	dispatch := func() error {
		if data == "" {
			return nil
		}
		var st Status
		switch event {
		case "conversation":
			var v struct {
				LastStatus *Status `json:"last_status"`
			}
			if err := json.Unmarshal([]byte(data), &v); err != nil {
				return err
			}
			if v.LastStatus == nil {
				return nil
			}
			st = *v.LastStatus
		case "update":
			if err := json.Unmarshal([]byte(data), &st); err != nil {
				return err
			}
		case "notification":
			var n struct {
				Type   string  `json:"type"`
				Status *Status `json:"status"`
			}
			if err := json.Unmarshal([]byte(data), &n); err != nil {
				return err
			}
			if n.Type != "mention" || n.Status == nil {
				return nil
			}
			st = *n.Status
		default:
			return nil
		}
		return c.process(streamCtx, token, st, load, update, onEars, onSound)
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			event, data = "", ""
			if load().AccessToken != token {
				return nil
			}
		} else if v, ok := strings.CutPrefix(line, "event:"); ok {
			event = strings.TrimSpace(v)
		} else if v, ok := strings.CutPrefix(line, "data:"); ok {
			data += strings.TrimSpace(v)
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil && streamCtx.Err() != context.DeadlineExceeded {
		return err
	}
	return nil
}

// streamingBase follows Mastodon's unauthenticated host discovery redirect.
// The bearer token is only sent after the destination has passed URL validation.
func (c *Client) streamingBase(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v1/streaming", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		return c.base, nil
	}
	location := resp.Header.Get("Location")
	if location == "" {
		return "", errors.New("Mastodon streaming redirect missing Location")
	}
	u, err := req.URL.Parse(location)
	if err != nil {
		return "", errors.New("invalid Mastodon streaming redirect")
	}
	if u.User != nil || u.Hostname() == "" {
		return "", errors.New("invalid Mastodon streaming redirect")
	}
	stream, err := NewClient(u.Scheme+"://"+u.Host, c.http)
	if err != nil {
		return "", err
	}
	return stream.base, nil
}
