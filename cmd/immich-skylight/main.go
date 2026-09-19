// Command immich-skylight syncs favorite/tagged Immich photos to Skylight frames.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/jfroy/immich-skylight/internal/config"
	"github.com/jfroy/immich-skylight/internal/metrics"
	"github.com/jfroy/immich-skylight/internal/observability"
	"github.com/jfroy/immich-skylight/internal/server"
	"github.com/jfroy/immich-skylight/internal/skylight"
	"github.com/jfroy/immich-skylight/internal/state"
	isync "github.com/jfroy/immich-skylight/internal/sync"
)

// Set via -ldflags at build time.
var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

const usage = `immich-skylight — share Immich favorites/tagged photos to a Skylight frame

Usage:
  immich-skylight run       Sync continuously every SYNC_INTERVAL (default)
  immich-skylight sync      Run a single sync pass and exit
  immich-skylight frames    Log in to Skylight and list frames (IDs and names)
  immich-skylight version   Print version information

Configuration is via environment variables; see README.md.
`

func main() {
	cmd := "run"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	case "version":
		fmt.Printf("immich-skylight %s (%s) built %s\n", version, commit, date)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cmd); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cmd string) error {
	cfg, err := config.Load()
	if err != nil {
		if cmd != "frames" {
			return err
		}
		// `frames` only needs Skylight credentials; tolerate missing Immich config.
		cfg = &config.Config{
			SkylightEmail:    os.Getenv("SKYLIGHT_EMAIL"),
			SkylightPassword: os.Getenv("SKYLIGHT_PASSWORD"),
			StateFile:        envOr("STATE_FILE", "/data/state.db"),
			LogLevel:         envOr("LOG_LEVEL", "info"),
			ServiceName:      envOr("OTEL_SERVICE_NAME", "immich-skylight"),
		}
	}

	registry := prometheus.NewRegistry()
	obs, err := observability.Init(ctx, observability.Options{
		ServiceName:    cfg.ServiceName,
		ServiceVersion: version,
		Level:          observability.ParseLevel(cfg.LogLevel),
	})
	if err != nil {
		return fmt.Errorf("initializing observability: %w", err)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := obs.Shutdown(sctx); err != nil {
			slog.Error("observability shutdown", "err", err)
		}
	}()
	log := obs.Logger.With("version", version)

	rec, err := metrics.New(registry)
	if err != nil {
		return fmt.Errorf("registering metrics: %w", err)
	}

	st, err := state.Open(ctx, cfg.StateFile)
	if err != nil {
		return err
	}
	defer st.Close()

	if cmd == "frames" {
		return listFrames(ctx, cfg, st, log, rec)
	}

	var syncer *isync.Syncer
	var ready func() bool = func() bool { return syncer.Ready() }

	// Start the HTTP listener before setup so liveness works during a slow start.
	srv := server.New(cfg.HTTPAddr, registry, ready)
	srvErr := make(chan error, 1)
	go func() {
		log.Info("http listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
		}
	}()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	setup := func() (*isync.Syncer, error) {
		return isync.New(ctx, isync.Options{Config: cfg, State: st, Logger: log, Metrics: rec})
	}
	if cmd == "run" {
		// Stay alive (liveness OK, readiness 503) while dependencies come up.
		syncer, err = retrySetup(ctx, log, setup)
	} else {
		syncer, err = setup()
	}
	if err != nil {
		return err
	}

	done := make(chan error, 1)
	go func() {
		switch cmd {
		case "sync":
			done <- syncer.Once(ctx)
		case "run":
			log.Info("starting daemon", "interval", cfg.Interval, "state", cfg.StateFile, "dry_run", cfg.DryRun)
			done <- syncer.Run(ctx)
		default:
			done <- fmt.Errorf("unknown command %q\n%s", cmd, usage)
		}
	}()

	select {
	case err := <-srvErr:
		return fmt.Errorf("http server: %w", err)
	case err := <-done:
		return err
	}
}

// retrySetup runs setup with exponential backoff until it succeeds or ctx ends.
func retrySetup(ctx context.Context, log *slog.Logger, setup func() (*isync.Syncer, error)) (*isync.Syncer, error) {
	delay := 5 * time.Second
	for {
		s, err := setup()
		if err == nil {
			return s, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		log.Error("setup failed; retrying", "err", err, "retry_in", delay)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		if delay < 5*time.Minute {
			delay *= 2
		}
	}
}

func listFrames(ctx context.Context, cfg *config.Config, st *state.State, log *slog.Logger, rec *metrics.Recorder) error {
	c, err := skylight.New(skylight.Options{
		Email: cfg.SkylightEmail, Password: cfg.SkylightPassword, Fingerprint: st.Fingerprint,
		Store: memStore{st}, Logger: log, Metrics: rec,
	})
	if err != nil {
		return err
	}
	frames, err := c.Frames(ctx)
	if err != nil {
		return err
	}
	if len(frames) == 0 {
		log.Warn("no frames found on this account")
		return nil
	}
	fmt.Printf("%-12s %s\n", "ID", "NAME")
	for _, f := range frames {
		fmt.Printf("%-12s %s\n", f.ID, f.Name)
	}
	return nil
}

// memStore reads persisted tokens but never writes, so `frames` has no side effects.
type memStore struct{ st *state.State }

func (m memStore) Tokens() (string, string, time.Time) {
	t, err := m.st.GetTokens(context.Background())
	if err != nil {
		return "", "", time.Time{}
	}
	return t.AccessToken, t.RefreshToken, t.Expiry
}

func (m memStore) SaveTokens(string, string, time.Time) error { return nil }

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
