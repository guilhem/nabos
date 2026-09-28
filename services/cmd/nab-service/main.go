// nab-service: web interface, settings, PyNab services, Home Assistant,
// voice assistant and updates for NabOS (docs/protocol-v1.md).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
)

// version is set at build time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

type Env struct {
	MQTTHost, HTTPAddr, DataDir      string
	MQTTPort                         int
	SoundsDirs                       []string
	Version, UpdateRepo, UpdateAsset string
	GitHubAPI, GitHubDownload        string
	LVAURL, LVAUnit                  string
	WeatherURL, GeocodingURL         string
	TimesyncFile, TimesyncClock      string
	NetProbe                         string
}

func env(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

func loadEnv() Env {
	port, err := strconv.Atoi(env("NABOS_MQTT_PORT", "1883"))
	if err != nil {
		port = 1883
	}
	v := env("NABOS_VERSION", version)
	return Env{
		MQTTHost:       env("NABOS_MQTT_HOST", "127.0.0.1"),
		MQTTPort:       port,
		HTTPAddr:       env("NABOS_HTTP_ADDR", ":8080"),
		DataDir:        env("NABOS_DATA_DIR", "/data/nabos"),
		SoundsDirs:     strings.Split(env("NABOS_SOUNDS_DIRS", "/usr/share/nabos/sounds:/data/nabos/media/sounds"), ":"),
		Version:        v,
		UpdateRepo:     env("NABOS_UPDATE_REPO", ""),
		UpdateAsset:    env("NABOS_UPDATE_ASSET", ""),
		GitHubAPI:      env("NABOS_GITHUB_API", "https://api.github.com"),
		GitHubDownload: env("NABOS_GITHUB_DOWNLOAD", "https://github.com"),
		LVAURL:         env("NABOS_LVA_URL", "ws://127.0.0.1:6055"),
		LVAUnit:        env("NABOS_LVA_UNIT", "linux-voice-assistant.service"),
		WeatherURL:     env("NABOS_WEATHER_URL", "https://api.open-meteo.com/v1/forecast"),
		GeocodingURL:   env("NABOS_GEOCODING_URL", "https://geocoding-api.open-meteo.com/v1/search"),
		TimesyncFile:   env("NABOS_TIMESYNC_FILE", "/run/systemd/timesync/synchronized"),
		TimesyncClock:  env("NABOS_TIMESYNC_CLOCK", "/var/lib/systemd/timesync/clock"),
		NetProbe:       env("NABOS_NET_PROBE", "api.github.com:443"),
	}
}

func main() {
	// Checked first: --version must not touch data, network or hardware.
	for _, a := range os.Args[1:] {
		switch a {
		case "--version", "-version":
			fmt.Println("nab-service", version)
			return
		case "--help", "-h":
			fmt.Println("usage: nab-service [--version]\nConfiguration via NABOS_* variables, see docs/protocol-v1.md")
			return
		}
	}
	level := slog.LevelInfo
	if os.Getenv("NABOS_LOG") == "debug" {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	app, err := NewApp(loadEnv())
	if err != nil {
		slog.Error("startup failed", "err", err)
		os.Exit(1)
	}
	if err := app.Run(ctx); err != nil {
		slog.Error("stopped", "err", err)
		os.Exit(1)
	}
}
