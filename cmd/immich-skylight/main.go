// Command immich-skylight syncs favorite/tagged Immich photos to Skylight frames.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jfroy/immich-skylight/internal/config"
	"github.com/jfroy/immich-skylight/internal/skylight"
	"github.com/jfroy/immich-skylight/internal/state"
	isync "github.com/jfroy/immich-skylight/internal/sync"
)

const usage = `immich-skylight — share Immich favorites/tagged photos to a Skylight frame

Usage:
  immich-skylight run      Sync continuously every SYNC_INTERVAL (default)
  immich-skylight sync     Run a single sync pass and exit
  immich-skylight frames   Log in to Skylight and list frames (IDs and names)

Configuration is via environment variables; see README.md.
`

func main() {
	cmd := "run"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	if cmd == "-h" || cmd == "--help" || cmd == "help" {
		fmt.Print(usage)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := realMain(ctx, cmd); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func realMain(ctx context.Context, cmd string) error {
	log := newLogger(os.Getenv("LOG_LEVEL"))
	slog.SetDefault(log)

	if cmd == "frames" {
		return listFrames(ctx, log)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	st, err := state.Load(cfg.StateFile)
	if err != nil {
		return err
	}
	s, err := isync.New(ctx, cfg, st, log)
	if err != nil {
		return err
	}

	switch cmd {
	case "sync":
		return s.Once(ctx)
	case "run":
		log.Info("starting daemon", "interval", cfg.Interval, "state", cfg.StateFile, "dry_run", cfg.DryRun)
		return s.Run(ctx)
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// listFrames needs only Skylight credentials, so it bypasses full config validation.
func listFrames(ctx context.Context, log *slog.Logger) error {
	email, pw := os.Getenv("SKYLIGHT_EMAIL"), os.Getenv("SKYLIGHT_PASSWORD")
	if email == "" || pw == "" {
		return errors.New("SKYLIGHT_EMAIL and SKYLIGHT_PASSWORD are required")
	}
	stPath := os.Getenv("STATE_FILE")
	if stPath == "" {
		stPath = "/data/state.json"
	}
	st, err := state.Load(stPath)
	if err != nil {
		return err
	}
	c := skylight.NewClient(email, pw, st.Fingerprint, memStore{st})
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

// memStore keeps tokens in the loaded state but does not persist them, so the
// `frames` command has no side effects on disk.
type memStore struct{ st *state.State }

func (m memStore) Tokens() (string, string, time.Time) {
	t := m.st.Tokens
	return t.AccessToken, t.RefreshToken, t.Expiry
}

func (m memStore) SaveTokens(a, r string, e time.Time) error {
	m.st.SetTokens(state.Tokens{AccessToken: a, RefreshToken: r, Expiry: e})
	return nil
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
