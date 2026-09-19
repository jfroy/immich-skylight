// Package sync pushes selected Immich assets to Skylight frames and keeps a
// record of what has been sent.
package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jfroy/immich-skylight/internal/config"
	"github.com/jfroy/immich-skylight/internal/immich"
	"github.com/jfroy/immich-skylight/internal/skylight"
	"github.com/jfroy/immich-skylight/internal/state"
)

// Syncer coordinates one or more sync passes.
type Syncer struct {
	cfg   *config.Config
	im    *immich.Client
	sky   *skylight.Client
	st    *state.State
	log   *slog.Logger
	tags  []string // resolved Immich tag IDs
	frame []string // resolved Skylight frame IDs
}

// New wires up clients and resolves tags and frames. It fails fast on bad
// credentials or unknown tag/frame names.
func New(ctx context.Context, cfg *config.Config, st *state.State, log *slog.Logger) (*Syncer, error) {
	s := &Syncer{cfg: cfg, st: st, log: log}
	s.im = immich.New(cfg.ImmichURL, cfg.ImmichAPIKey)
	s.sky = skylight.NewClient(cfg.SkylightEmail, cfg.SkylightPassword, st.Fingerprint, &tokenStore{st})

	if err := s.im.Ping(ctx); err != nil {
		return nil, err
	}
	tags, err := s.im.ResolveTags(ctx, cfg.Tags)
	if err != nil {
		return nil, err
	}
	s.tags = tags

	if err := s.sky.Authenticate(ctx); err != nil {
		return nil, fmt.Errorf("skylight login: %w", err)
	}
	frames, err := s.sky.Frames(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing skylight frames: %w", err)
	}
	s.frame, err = selectFrames(frames, cfg.FrameIDs, cfg.FrameNames)
	if err != nil {
		return nil, err
	}
	for _, f := range frames {
		if contains(s.frame, f.ID) {
			log.Info("target frame", "id", f.ID, "name", f.Name)
		}
	}
	return s, nil
}

// Run performs sync passes until ctx is cancelled.
func (s *Syncer) Run(ctx context.Context) error {
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		if err := s.Once(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.log.Error("sync pass failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Once performs a single sync pass.
func (s *Syncer) Once(ctx context.Context) error {
	start := time.Now()
	assets, err := s.im.Search(ctx, immich.SearchOptions{
		Favorites:    s.cfg.Favorites,
		TagIDs:       s.tags,
		IncludeVideo: s.cfg.IncludeVideo,
	})
	if err != nil {
		return err
	}

	selected := make(map[string]bool, len(assets))
	var pending []immich.Asset
	for _, a := range assets {
		selected[a.ID] = true
		if _, ok := s.st.Sent[a.ID]; !ok {
			pending = append(pending, a)
		}
	}
	s.log.Info("sync pass", "selected", len(assets), "already_sent", len(assets)-len(pending), "pending", len(pending))

	var sent, failed int
	for i := len(pending) - 1; i >= 0; i-- { // oldest first so the frame shows them chronologically
		a := pending[i]
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.upload(ctx, a); err != nil {
			failed++
			s.log.Warn("upload failed", "asset", a.ID, "file", a.OriginalFileName, "err", err)
			if errors.Is(err, skylight.ErrUnauthorized) {
				return err
			}
			continue
		}
		sent++
		if err := s.st.Save(); err != nil {
			return fmt.Errorf("saving state: %w", err)
		}
	}

	var removed int
	if s.cfg.RemoveUnselected {
		removed = s.removeUnselected(ctx, selected)
	}

	s.log.Info("sync pass done", "sent", sent, "failed", failed, "removed", removed, "took", time.Since(start).Round(time.Millisecond))
	return s.st.Save()
}

func (s *Syncer) upload(ctx context.Context, a immich.Asset) error {
	data, mime, ext, err := s.fetch(ctx, a)
	if err != nil {
		return err
	}
	caption := ""
	if s.cfg.UseCaption && a.ExifInfo != nil {
		caption = strings.TrimSpace(a.ExifInfo.Description)
	}

	if s.cfg.DryRun {
		s.log.Info("dry-run: would upload", "asset", a.ID, "file", a.OriginalFileName, "ext", ext, "bytes", len(data), "caption", caption)
		return nil
	}
	res, err := s.sky.Upload(ctx, s.frame, ext, mime, data, caption)
	if err != nil {
		return err
	}
	s.st.MarkSent(a.ID, state.Sent{
		MessageIDs: res.MessageIDs,
		FrameIDs:   s.frame,
		Checksum:   a.Checksum,
		SentAt:     time.Now(),
	})
	s.log.Info("uploaded", "asset", a.ID, "file", a.OriginalFileName, "ext", ext, "bytes", len(data), "message_ids", res.MessageIDs)
	return nil
}

// fetch downloads the configured rendition, falling back to Immich's preview
// when the original/fullsize is unavailable or not a format Skylight accepts.
func (s *Syncer) fetch(ctx context.Context, a immich.Asset) (data []byte, mime, ext string, err error) {
	var chain []immich.Rendition
	switch s.cfg.ImageSource {
	case config.SourceOriginal:
		chain = []immich.Rendition{immich.RenditionOriginal, immich.RenditionFullsize, immich.RenditionPreview}
	case config.SourceFullsize:
		chain = []immich.Rendition{immich.RenditionFullsize, immich.RenditionPreview}
	default:
		chain = []immich.Rendition{immich.RenditionPreview}
	}
	// Videos only exist as originals on Skylight's side of things.
	if a.Type == "VIDEO" {
		chain = []immich.Rendition{immich.RenditionOriginal}
	}

	var lastErr error
	for _, r := range chain {
		if r == immich.RenditionOriginal && skylight.SupportedExt(a.OriginalMimeType) == "" {
			lastErr = fmt.Errorf("original type %s not supported by skylight", a.OriginalMimeType)
			continue
		}
		data, mime, err = s.im.Download(ctx, a.ID, r)
		if err != nil {
			lastErr = fmt.Errorf("download %s: %w", r, err)
			continue
		}
		if ext = skylight.SupportedExt(mime); ext == "" {
			lastErr = fmt.Errorf("rendition %s has unsupported type %q", r, mime)
			continue
		}
		return data, mime, ext, nil
	}
	return nil, "", "", lastErr
}

// removeUnselected deletes photos from the frame that are no longer selected
// in Immich (un-favorited / untagged / deleted).
func (s *Syncer) removeUnselected(ctx context.Context, selected map[string]bool) int {
	byFrame := map[string][]int{}
	var stale []string
	for id, rec := range s.st.Sent {
		if selected[id] {
			continue
		}
		stale = append(stale, id)
		frames := rec.FrameIDs
		if len(frames) == 0 {
			frames = s.frame
		}
		// One message ID is returned per target frame, in order.
		for i, mid := range rec.MessageIDs {
			f := frames[0]
			if i < len(frames) {
				f = frames[i]
			}
			byFrame[f] = append(byFrame[f], mid)
		}
	}
	if len(stale) == 0 {
		return 0
	}
	if s.cfg.DryRun {
		s.log.Info("dry-run: would remove", "assets", len(stale))
		return 0
	}
	for f, ids := range byFrame {
		if err := s.sky.DeleteMessages(ctx, f, ids); err != nil {
			s.log.Warn("removing photos from frame failed", "frame", f, "count", len(ids), "err", err)
			return 0
		}
	}
	for _, id := range stale {
		s.st.Forget(id)
	}
	return len(stale)
}

func selectFrames(all []skylight.Frame, ids, names []string) ([]string, error) {
	if len(all) == 0 {
		return nil, errors.New("skylight account has no frames")
	}
	if len(ids) == 0 && len(names) == 0 {
		if len(all) == 1 {
			return []string{all[0].ID}, nil
		}
		var opts []string
		for _, f := range all {
			opts = append(opts, fmt.Sprintf("%s (%s)", f.Name, f.ID))
		}
		return nil, fmt.Errorf("multiple frames found, set SKYLIGHT_FRAME_IDS or SKYLIGHT_FRAME_NAMES: %s", strings.Join(opts, ", "))
	}
	var out []string
	for _, want := range ids {
		found := false
		for _, f := range all {
			if f.ID == want {
				out, found = append(out, f.ID), true
			}
		}
		if !found {
			return nil, fmt.Errorf("skylight frame id %q not found", want)
		}
	}
	for _, want := range names {
		found := false
		for _, f := range all {
			if strings.EqualFold(f.Name, want) && !contains(out, f.ID) {
				out, found = append(out, f.ID), true
			}
		}
		if !found {
			return nil, fmt.Errorf("skylight frame named %q not found", want)
		}
	}
	return out, nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// tokenStore adapts state.State to skylight.TokenStore.
type tokenStore struct{ st *state.State }

func (t *tokenStore) Tokens() (string, string, time.Time) {
	tk := t.st.Tokens
	return tk.AccessToken, tk.RefreshToken, tk.Expiry
}

func (t *tokenStore) SaveTokens(access, refresh string, expiry time.Time) error {
	t.st.SetTokens(state.Tokens{AccessToken: access, RefreshToken: refresh, Expiry: expiry})
	return t.st.Save()
}
