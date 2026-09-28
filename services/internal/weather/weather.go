// Package weather uses Open-Meteo (open source API,
// no key): info animations, spoken forecasts and location search.
package weather

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/guilhem/nabos/services/internal/config"
)

// Animation is the core "info" animation format.
type Animation struct {
	Tempo  int                 `json:"tempo"`
	Colors []map[string]string `json:"colors"`
}

func anim(tempo int, frames ...[3]string) *Animation {
	a := &Animation{Tempo: tempo}
	for _, f := range frames {
		a.Colors = append(a.Colors, map[string]string{"left": f[0], "center": f[1], "right": f[2]})
	}
	return a
}

const k, y, b = "000000", "ffff00", "0000ff"

// Weather animations using the original Violet color sequences.
var (
	sunny  = anim(25, [3]string{y, y, y}, [3]string{y, y, y}, [3]string{y, y, y}, [3]string{y, y, y}, [3]string{y, y, y}, [3]string{k, k, k}, [3]string{k, k, k}, [3]string{k, k, k})
	cloudy = anim(125, [3]string{k, y, k}, [3]string{b, k, b})
	foggy  = anim(25, [3]string{b, b, b}, [3]string{b, b, b}, [3]string{b, b, b}, [3]string{b, b, b}, [3]string{b, b, b}, [3]string{k, k, k})
	rainy  = anim(20, [3]string{k, k, k}, [3]string{k, b, k}, [3]string{b, k, b}, [3]string{k, k, k}, [3]string{k, k, b}, [3]string{b, k, k}, [3]string{k, k, b}, [3]string{k, b, k}, [3]string{k, k, b})
	snowy  = anim(40, [3]string{b, k, k}, [3]string{k, k, k}, [3]string{k, k, b}, [3]string{k, k, k}, [3]string{k, b, k}, [3]string{k, k, k}, [3]string{k, k, b}, [3]string{k, k, k}, [3]string{k, b, k}, [3]string{k, k, k}, [3]string{b, k, k}, [3]string{k, k, k})
	stormy = anim(25, [3]string{k, b, y}, [3]string{k, k, k}, [3]string{k, k, k}, [3]string{k, k, k}, [3]string{k, k, k}, [3]string{b, y, k}, [3]string{k, b, y}, [3]string{k, k, k}, [3]string{k, k, k}, [3]string{k, y, b}, [3]string{y, b, k})
	// Rain within the hour.
	rainSoon = anim(16, [3]string{k, "003399", k}, [3]string{"003399", k, "003399"}, [3]string{k, k, k}, [3]string{k, "003399", k}, [3]string{"003399", k, "003399"}, [3]string{k, k, k}, [3]string{k, k, k}, [3]string{k, k, k}, [3]string{k, k, k}, [3]string{k, k, k}, [3]string{k, k, k}, [3]string{k, k, k}, [3]string{k, "003399", k}, [3]string{"003399", k, "003399"}, [3]string{k, k, k}, [3]string{k, "003399", k}, [3]string{"003399", k, "003399"}, [3]string{k, k, k})
)

// Class maps a WMO weather code to a sky sound (weather/sky/<class>.mp3)
// and an animation.
func Class(code int) (string, *Animation) {
	switch {
	case code <= 1:
		return "sunny", sunny
	case code <= 3:
		return "cloudy", cloudy
	case code == 45 || code == 48:
		return "foggy", foggy
	case code == 56 || code == 57 || code == 66 || code == 67:
		return "black-ice", snowy
	case code >= 51 && code <= 65:
		return "rainy", rainy
	case code >= 71 && code <= 77 || code == 85 || code == 86:
		return "snowy", snowy
	case code >= 80 && code <= 82:
		return "showery", rainy
	case code >= 95:
		return "stormy", stormy
	}
	return "cloudy", cloudy
}

type Forecast struct {
	Codes    [2]int     // today, tomorrow
	MaxTemps [2]float64 // °C
	RainSoon bool       // precipitation within the next hour
	Fetched  time.Time
}

type Client struct {
	HTTP         *http.Client
	ForecastURL  string
	GeocodingURL string
}

func NewClient(forecastURL, geocodingURL string) *Client {
	return &Client{HTTP: &http.Client{Timeout: 15 * time.Second}, ForecastURL: forecastURL, GeocodingURL: geocodingURL}
}

func (c *Client) getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", req.URL.Host, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}

func (c *Client) Fetch(ctx context.Context, lat, lon float64) (*Forecast, error) {
	q := url.Values{}
	q.Set("latitude", strconv.FormatFloat(lat, 'f', 4, 64))
	q.Set("longitude", strconv.FormatFloat(lon, 'f', 4, 64))
	q.Set("daily", "weather_code,temperature_2m_max")
	q.Set("minutely_15", "precipitation")
	q.Set("forecast_minutely_15", "4")
	q.Set("forecast_days", "2")
	q.Set("timezone", "auto")
	var r struct {
		Daily struct {
			Code []int     `json:"weather_code"`
			Max  []float64 `json:"temperature_2m_max"`
		} `json:"daily"`
		Minutely struct {
			Precipitation []*float64 `json:"precipitation"`
		} `json:"minutely_15"`
	}
	if err := c.getJSON(ctx, c.ForecastURL+"?"+q.Encode(), &r); err != nil {
		return nil, err
	}
	if len(r.Daily.Code) < 2 || len(r.Daily.Max) < 2 {
		return nil, errors.New("incomplete forecast")
	}
	f := &Forecast{Codes: [2]int{r.Daily.Code[0], r.Daily.Code[1]}, MaxTemps: [2]float64{r.Daily.Max[0], r.Daily.Max[1]}, Fetched: time.Now()}
	for _, p := range r.Minutely.Precipitation {
		if p != nil && *p > 0 {
			f.RainSoon = true
		}
	}
	return f, nil
}

type Place struct {
	Label     string
	Latitude  float64
	Longitude float64
}

func (c *Client) Geocode(ctx context.Context, name, lang string) (*Place, error) {
	q := url.Values{}
	q.Set("name", name)
	q.Set("count", "1")
	q.Set("language", lang)
	var r struct {
		Results []struct {
			Name      string  `json:"name"`
			Country   string  `json:"country"`
			Admin1    string  `json:"admin1"`
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
		} `json:"results"`
	}
	if err := c.getJSON(ctx, c.GeocodingURL+"?"+q.Encode(), &r); err != nil {
		return nil, err
	}
	if len(r.Results) == 0 {
		return nil, fmt.Errorf("unknown place %q", name)
	}
	p := r.Results[0]
	label := p.Name
	if p.Admin1 != "" {
		label += ", " + p.Admin1
	}
	if p.Country != "" {
		label += ", " + p.Country
	}
	return &Place{Label: label, Latitude: p.Latitude, Longitude: p.Longitude}, nil
}

// Infos returns the idle animations to show: weather and rain (nil removes).
func Infos(cfg config.Weather, f *Forecast) (weatherInfo, rainInfo *Animation) {
	if f == nil {
		return nil, nil
	}
	if cfg.Animation == "weather_and_rain" || cfg.Animation == "weather_only" {
		_, weatherInfo = Class(f.Codes[0])
	}
	if (cfg.Animation == "weather_and_rain" || cfg.Animation == "rain_only") && f.RainSoon {
		rainInfo = rainSoon
	}
	return
}

// Message builds the spoken forecast (core "message" args). day: 0 today, 1 tomorrow.
func Message(cfg config.Weather, f *Forecast, day int) map[string]any {
	sig := map[string]any{"audio": []string{"weather/signature.mp3"}}
	var audio []string
	switch {
	case cfg.Location == "":
		audio = []string{"weather/no-location-error.mp3"}
	case f == nil:
		audio = []string{"weather/no-data-error.mp3"}
	default:
		class, _ := Class(f.Codes[day])
		t, unit := f.MaxTemps[day], "degree.mp3"
		if cfg.Unit == "fahrenheit" {
			t, unit = t*1.8+32, "degree_f.mp3"
		}
		audio = []string{
			[]string{"weather/today.mp3", "weather/tomorrow.mp3"}[day],
			"weather/sky/" + class + ".mp3",
			fmt.Sprintf("weather/temp/%d.mp3", int(math.Round(t))),
			"weather/" + unit,
		}
	}
	return map[string]any{"signature": sig, "body": []any{map[string]any{"audio": audio}}}
}

// NextAnnouncement for frequencies 1 and 2 (random like weather).
func NextAnnouncement(freq int, now time.Time, rnd func(n int) int) time.Time {
	switch freq {
	case 1:
		return now.Add(time.Duration(40+rnd(31)) * time.Minute)
	case 2:
		return now.Add(time.Duration(100+rnd(91)) * time.Minute)
	}
	return time.Time{}
}
