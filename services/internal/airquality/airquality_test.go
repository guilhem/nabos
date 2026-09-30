package airquality

import (
	"context"
	"github.com/guilhem/nabos/services/internal/rabbit"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestFetch(t *testing.T) {
	var body, path, token string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, token = r.URL.Path, r.URL.Query().Get("token")
		if r.URL.Path == "/feed/fail/" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Write([]byte(body))
	}))
	defer srv.Close()
	c := NewClient()
	c.FeedURL = srv.URL + "/feed/"
	ctx := context.Background()
	feed := func(aqi string, pm25 string) string {
		iaqi := "{}"
		if pm25 != "" {
			iaqi = `{"pm25":{"v":` + pm25 + `}}`
		}
		return `{"status":"ok","data":{"aqi":` + aqi + `,"city":{"name":"Paris"},"iaqi":` + iaqi + "}}"
	}
	for _, c2 := range []struct {
		body, index string
		level       int
	}{
		{feed("50", "150"), "aqi", Good},
		{feed("51", "10"), "aqi", Moderate},
		{feed("100", ""), "aqi", Moderate},
		{feed("101", ""), "aqi", Bad},
		{feed("10", "101"), "pm25", Bad},
		{feed("10", "50.5"), "pm25", Moderate},
		{feed("75", ""), "pm25", Moderate}, // no PM2.5 at station: AQI
		{feed(`"-"`, ""), "aqi", Bad},      // no reading: worst
		{feed("10", "null"), "pm25", Bad},
		{feed("10", "20"), "other", Good},
	} {
		body = c2.body
		r, err := c.Fetch(ctx, Query{Token: "t/k+n", Index: c2.index, Located: true, Latitude: 48.8566, Longitude: -2.35})
		if err != nil || r.Level != c2.level || r.City != "Paris" {
			t.Errorf("%s %s: %+v %v, want level %d", c2.body, c2.index, r, err, c2.level)
		}
	}
	if path != "/feed/geo:48.8566;-2.35/" || token != "t/k+n" {
		t.Errorf("request %s token %q", path, token)
	}
	if _, err := c.Fetch(ctx, Query{Token: "k"}); err != nil || path != "/feed/here/" {
		t.Errorf("IP fallback: %v %s", err, path)
	}
	body = `{"status":"error","data":"Invalid key s3cret"}`
	if _, err := c.Fetch(ctx, Query{Token: "s3cret"}); err == nil || !strings.Contains(err.Error(), "Invalid key") || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("WAQI error: %v", err)
	}
	for _, b := range []string{"not json", `{"status":"ok","data":"x"}`} {
		body = b
		if _, err := c.Fetch(ctx, Query{Token: "k"}); err == nil {
			t.Errorf("accepted %s", b)
		}
	}
	c.FeedURL = srv.URL + "/feed/fail/"
	if _, err := c.Fetch(ctx, Query{Token: "s3cret"}); err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("HTTP error: %v", err)
	}
	c.FeedURL = "http://127.0.0.1:1/feed/"
	if _, err := c.Fetch(ctx, Query{Token: "s3cret"}); err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("dial error: %v", err)
	}
	if _, err := c.Fetch(ctx, Query{}); err == nil {
		t.Error("empty token accepted")
	}
}

func TestInfoAndMessage(t *testing.T) {
	for _, c := range []struct {
		visual string
		r      *Result
		tempo  uint32
	}{
		{"always", &Result{Level: Good}, 42},
		{"always", &Result{Level: Moderate}, 14},
		{"alert", &Result{Level: Good}, 0},
		{"alert", &Result{Level: Bad}, 14},
		{"nothing", &Result{Level: Bad}, 0},
		{"always", nil, 0},
	} {
		a := Info(c.visual, c.r)
		if (a == nil) != (c.tempo == 0) || a != nil && a.Tempo != c.tempo {
			t.Errorf("%s %+v: %+v", c.visual, c.r, a)
		}
	}
	if n := [3]int{len(animations[Bad].Frames), len(animations[Moderate].Frames), len(animations[Good].Frames)}; n != [3]int{16, 17, 4} {
		t.Errorf("frames %v", n)
	}
	m := Message(&Result{Level: Moderate})
	if m.Action != rabbit.Message || m.Signature == nil || !reflect.DeepEqual(m.Signature.Audio, []string{"airquality/signature.mp3"}) || !reflect.DeepEqual(m.Body, []rabbit.Item{{Audio: []string{"airquality/moderate.mp3"}}}) {
		t.Fatalf("message %+v", m)
	}
	if m := Message(nil); !reflect.DeepEqual(m.Body, []rabbit.Item{{Audio: []string{"airquality/no-data-error.mp3;system/abort.wav"}}}) {
		t.Errorf("no data %v", m)
	}
	// The fallback must exist in the shipped assets.
	if _, err := os.Stat("../../../assets/sounds/system/abort.wav"); err != nil {
		t.Error(err)
	}
}
