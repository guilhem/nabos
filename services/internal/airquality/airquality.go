// Package airquality reads the World Air Quality Index (WAQI/aqicn.org)
// feed like PyNab's nabairqualityd: AQI or PM2.5, 50/100 thresholds.
package airquality

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/guilhem/nabos/services/internal/rabbit"
	"github.com/guilhem/nabos/services/internal/triggers"
)

const FeedURL = "https://api.waqi.info/feed/"

// Levels, as in PyNab.
const (
	Bad = iota
	Moderate
	Good
)

type Client struct {
	HTTP    *http.Client
	FeedURL string
}

func NewClient() *Client {
	return &Client{HTTP: &http.Client{Timeout: 10 * time.Second}, FeedURL: FeedURL}
}

// Query: Index "pm25" uses PM2.5 when the station has it, anything else AQI.
// Without Located, WAQI geolocates the caller's IP.
type Query struct {
	Token, Index        string
	Located             bool
	Latitude, Longitude float64
}

type Result struct {
	Level int
	City  string
}

func (c *Client) Fetch(ctx context.Context, q Query) (*Result, error) {
	if q.Token == "" {
		return nil, errors.New("airquality: missing WAQI token")
	}
	where := "here/"
	if q.Located {
		where = "geo:" + strconv.FormatFloat(q.Latitude, 'f', -1, 64) + ";" + strconv.FormatFloat(q.Longitude, 'f', -1, 64) + "/"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.FeedURL+where+"?token="+url.QueryEscape(q.Token), nil)
	if err != nil {
		return nil, errors.New("airquality: invalid feed URL")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("airquality: %w", triggers.StripURL(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("airquality: HTTP %d", resp.StatusCode)
	}
	var r struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return nil, errors.New("airquality: invalid WAQI response")
	}
	if r.Status != "ok" {
		var msg string
		json.Unmarshal(r.Data, &msg)
		if len(msg) > 100 {
			msg = msg[:100]
		}
		return nil, fmt.Errorf("airquality: WAQI %s: %s", r.Status, strings.ReplaceAll(msg, q.Token, "***"))
	}
	var d struct {
		AQI  any `json:"aqi"`
		City struct {
			Name string `json:"name"`
		} `json:"city"`
		IAQI map[string]struct {
			V any `json:"v"`
		} `json:"iaqi"`
	}
	if err := json.Unmarshal(r.Data, &d); err != nil {
		return nil, errors.New("airquality: invalid WAQI data")
	}
	v := d.AQI
	if p, ok := d.IAQI["pm25"]; ok && q.Index == "pm25" {
		v = p.V
	}
	res := &Result{Level: Bad, City: d.City.Name} // non-numeric ("-"): assume worst
	if n, ok := v.(float64); ok {
		switch {
		case n > 100:
		case n > 50:
			res.Level = Moderate
		default:
			res.Level = Good
		}
	}
	return res, nil
}

// PyNab animations, indexed by level.
var animations = [3]*rabbit.Animation{{Tempo: 14, Frames: [][3]rabbit.RGB{{{0, 0, 0}, {0, 255, 255}, {0, 0, 0}}, {{0, 0, 0}, {0, 255, 255}, {0, 255, 255}}, {{0, 255, 255}, {0, 255, 255}, {0, 255, 255}}, {{0, 255, 255}, {0, 0, 0}, {0, 0, 0}}, {{0, 0, 0}, {0, 0, 0}, {0, 0, 0}}, {{0, 0, 0}, {0, 255, 255}, {0, 0, 0}}, {{0, 0, 0}, {0, 255, 255}, {0, 255, 255}}, {{0, 0, 0}, {0, 0, 0}, {0, 0, 0}}, {{0, 255, 255}, {0, 255, 255}, {0, 0, 0}}, {{0, 255, 255}, {0, 255, 255}, {0, 255, 255}}, {{0, 0, 0}, {0, 0, 0}, {0, 255, 255}}, {{0, 255, 255}, {0, 0, 0}, {0, 0, 0}}, {{0, 0, 0}, {0, 255, 255}, {0, 0, 0}}, {{0, 0, 0}, {0, 0, 0}, {0, 0, 0}}, {{0, 255, 255}, {0, 0, 0}, {0, 255, 255}}, {{0, 0, 0}, {0, 255, 255}, {0, 0, 0}}}},
	{Tempo: 14, Frames: [][3]rabbit.RGB{{{0, 0, 0}, {0, 255, 255}, {0, 255, 255}}, {{0, 255, 255}, {0, 255, 255}, {0, 0, 0}}, {{0, 255, 255}, {0, 255, 255}, {0, 255, 255}}, {{0, 255, 255}, {0, 255, 255}, {0, 255, 255}}, {{0, 255, 255}, {0, 255, 255}, {0, 255, 255}}, {{0, 255, 255}, {0, 255, 255}, {0, 255, 255}}, {{0, 255, 255}, {0, 0, 0}, {0, 255, 255}}, {{0, 0, 0}, {0, 0, 0}, {0, 255, 255}}, {{0, 0, 0}, {0, 0, 0}, {0, 0, 0}}, {{0, 0, 0}, {0, 255, 255}, {0, 0, 0}}, {{0, 0, 0}, {0, 255, 255}, {0, 255, 255}}, {{0, 255, 255}, {0, 255, 255}, {0, 255, 255}}, {{0, 255, 255}, {0, 255, 255}, {0, 255, 255}}, {{0, 255, 255}, {0, 255, 255}, {0, 255, 255}}, {{0, 255, 255}, {0, 255, 255}, {0, 0, 0}}, {{0, 255, 255}, {0, 0, 0}, {0, 255, 255}}, {{0, 0, 0}, {0, 255, 255}, {0, 255, 255}}}},
	{Tempo: 42, Frames: [][3]rabbit.RGB{{{0, 255, 255}, {0, 255, 255}, {0, 255, 255}}, {{0, 255, 255}, {0, 255, 255}, {0, 255, 255}}, {{0, 255, 255}, {0, 255, 255}, {0, 255, 255}}, {{0, 0, 0}, {0, 0, 0}, {0, 0, 0}}}},
}

// Info returns the idle animation (nil removes it). visual: "always",
// "alert" (hide when good) or "nothing".
func Info(visual string, r *Result) *rabbit.Animation {
	if r == nil || visual == "nothing" || visual == "alert" && r.Level == Good {
		return nil
	}
	return animations[r.Level]
}

// Message builds the spoken report (core "message" args); nil r = no data.
// PyNab ships no airquality/no-data-error.mp3, so the core falls back to the
// system error sound.
func Message(r *Result) rabbit.Command {
	audio := "airquality/no-data-error.mp3;system/abort.wav"
	if r != nil {
		audio = "airquality/" + [3]string{"bad", "moderate", "good"}[r.Level] + ".mp3"
	}
	return rabbit.Command{Action: rabbit.Message, Cancelable: true, Signature: &rabbit.Item{Audio: []string{"airquality/signature.mp3"}}, Body: []rabbit.Item{{Audio: []string{audio}}}}
}
