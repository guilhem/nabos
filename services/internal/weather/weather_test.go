package weather

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/guilhem/nabos/services/internal/config"
)

func TestFetchAndAnnounce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("latitude") != "48.8566" {
			http.Error(w, "bad", 400)
			return
		}
		w.Write([]byte(`{"daily":{"weather_code":[61,0],"temperature_2m_max":[12.6,-3.4]},"minutely_15":{"precipitation":[0,null,0.2,0]}}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, srv.URL)
	f, err := c.Fetch(context.Background(), 48.8566, 2.3522)
	if err != nil {
		t.Fatal(err)
	}
	if !f.RainSoon {
		t.Fatal("rain expected within the hour")
	}
	cfg := config.Defaults().Weather
	cfg.Location = "Paris"
	wi, ri := Infos(cfg, f)
	if wi != rainy || ri != rainSoon {
		t.Fatal("wrong infos")
	}
	m := Message(cfg, f, 1)
	audio := m["body"].([]any)[0].(map[string]any)["audio"].([]string)
	want := []string{"weather/tomorrow.mp3", "weather/sky/sunny.mp3", "weather/temp/-3.mp3", "weather/degree.mp3"}
	for i := range want {
		if audio[i] != want[i] {
			t.Fatalf("got %v", audio)
		}
	}
}

func TestNoInternet(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // connection refused, like a rabbit without Internet
	c := NewClient(url, url)
	if _, err := c.Fetch(context.Background(), 1, 1); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := c.Geocode(context.Background(), "Paris", "fr"); err == nil {
		t.Fatal("expected an error")
	}
	cfg := config.Defaults().Weather
	cfg.Location = "Paris"
	audio := Message(cfg, nil, 0)["body"].([]any)[0].(map[string]any)["audio"].([]string)
	if audio[0] != "weather/no-data-error.mp3" {
		t.Fatalf("got %v", audio)
	}
	if wi, ri := Infos(cfg, nil); wi != nil || ri != nil {
		t.Fatal("no infos without data")
	}
}
