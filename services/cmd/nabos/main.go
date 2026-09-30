// nabos: web interface, settings, PyNab services, Home Assistant,
// voice assistant and updates for NabOS (docs/protocol-v1.md).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

// version is set at build time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

type Env struct {
	HTTPAddr, DataDir        string
	SoundsDirs, ChorDirs     []string
	Version                  string
	WeatherURL, GeocodingURL string
}

func env(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

func loadEnv() Env {
	v := env("NABOS_VERSION", version)
	return Env{
		HTTPAddr:     env("NABOS_HTTP_ADDR", ":8080"),
		DataDir:      env("NABOS_DATA_DIR", "/data/nabos"),
		SoundsDirs:   strings.Split(env("NABOS_SOUNDS_DIRS", "/usr/share/nabos/sounds:/data/nabos/media/sounds"), ":"),
		ChorDirs:     strings.Split(env("NABOS_CHOREOGRAPHIES_DIRS", "/usr/share/nabos/choreographies:/data/nabos/media/choreographies"), ":"),
		Version:      v,
		WeatherURL:   env("NABOS_WEATHER_URL", "https://api.open-meteo.com/v1/forecast"),
		GeocodingURL: env("NABOS_GEOCODING_URL", "https://geocoding-api.open-meteo.com/v1/search"),
	}
}

func main() {
	// Checked first: --version must not touch data, network or hardware.
	for _, a := range os.Args[1:] {
		switch a {
		case "--version", "-version":
			fmt.Println("nabos", version)
			return
		case "--help", "-h":
			fmt.Println("usage: nabos [--version]\nConfiguration via NABOS_* variables, see docs/protocol-v1.md")
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
