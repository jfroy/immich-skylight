// Package config loads runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ImageSource selects which rendition of an Immich asset is uploaded.
type ImageSource string

const (
	// SourcePreview is Immich's JPEG preview (~1440px). Always available, handles HEIC/RAW.
	SourcePreview ImageSource = "preview"
	// SourceFullsize is Immich's full-resolution web-friendly rendition (falls back to preview).
	SourceFullsize ImageSource = "fullsize"
	// SourceOriginal uploads the original file when Skylight supports its format,
	// otherwise falls back to preview.
	SourceOriginal ImageSource = "original"
)

// Config is the full runtime configuration.
type Config struct {
	// Immich
	ImmichURL    string
	ImmichAPIKey string
	Favorites    bool
	Tags         []string
	ImageSource  ImageSource
	IncludeVideo bool

	// Skylight
	SkylightEmail    string
	SkylightPassword string
	FrameIDs         []string
	FrameNames       []string
	UseCaption       bool
	RemoveUnselected bool

	// Runtime
	Interval    time.Duration
	StateFile   string
	DryRun      bool
	LogLevel    string
	HTTPAddr    string // metrics/health listener
	ServiceName string
}

// Load reads configuration from the environment. Missing required values are
// reported as a single aggregated error.
func Load() (*Config, error) {
	c := &Config{
		ImmichURL:        strings.TrimRight(env("IMMICH_URL", ""), "/"),
		ImmichAPIKey:     env("IMMICH_API_KEY", ""),
		Favorites:        envBool("IMMICH_FAVORITES", true),
		Tags:             envList("IMMICH_TAGS"),
		ImageSource:      ImageSource(strings.ToLower(env("IMMICH_IMAGE_SOURCE", string(SourcePreview)))),
		IncludeVideo:     envBool("INCLUDE_VIDEOS", false),
		SkylightEmail:    env("SKYLIGHT_EMAIL", ""),
		SkylightPassword: env("SKYLIGHT_PASSWORD", ""),
		FrameIDs:         envList("SKYLIGHT_FRAME_IDS"),
		FrameNames:       envList("SKYLIGHT_FRAME_NAMES"),
		UseCaption:       envBool("SKYLIGHT_CAPTION", true),
		RemoveUnselected: envBool("REMOVE_UNSELECTED", false),
		StateFile:        env("STATE_FILE", "/data/state.json"),
		DryRun:           envBool("DRY_RUN", false),
		LogLevel:         env("LOG_LEVEL", "info"),
		HTTPAddr:         env("HTTP_ADDR", ":8080"),
		ServiceName:      env("OTEL_SERVICE_NAME", "immich-skylight"),
	}

	var errs []error
	if c.ImmichURL == "" {
		errs = append(errs, errors.New("IMMICH_URL is required"))
	}
	if c.ImmichAPIKey == "" {
		errs = append(errs, errors.New("IMMICH_API_KEY is required"))
	}
	if c.SkylightEmail == "" || c.SkylightPassword == "" {
		errs = append(errs, errors.New("SKYLIGHT_EMAIL and SKYLIGHT_PASSWORD are required"))
	}
	if !c.Favorites && len(c.Tags) == 0 {
		errs = append(errs, errors.New("nothing selected: set IMMICH_FAVORITES=true and/or IMMICH_TAGS"))
	}
	switch c.ImageSource {
	case SourcePreview, SourceFullsize, SourceOriginal:
	default:
		errs = append(errs, fmt.Errorf("IMMICH_IMAGE_SOURCE must be preview, fullsize or original (got %q)", c.ImageSource))
	}

	iv := env("SYNC_INTERVAL", "15m")
	d, err := time.ParseDuration(iv)
	if err != nil || d <= 0 {
		errs = append(errs, fmt.Errorf("SYNC_INTERVAL must be a positive duration (got %q)", iv))
	}
	c.Interval = d

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return strings.TrimSpace(v)
	}
	return def
}

func envBool(key string, def bool) bool {
	v := env(key, "")
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envList(key string) []string {
	var out []string
	for _, p := range strings.Split(env(key, ""), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
