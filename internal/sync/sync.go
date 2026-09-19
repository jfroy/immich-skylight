// Package sync pushes selected Immich assets to Skylight frames and keeps a
// record of what has been sent.
package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/jfroy/immich-skylight/internal/config"
	"github.com/jfroy/immich-skylight/internal/httpx"
	"github.com/jfroy/immich-skylight/internal/immich"
	"github.com/jfroy/immich-skylight/internal/metrics"
	"github.com/jfroy/immich-skylight/internal/skylight"
	"github.com/jfroy/immich-skylight/internal/state"
)

var tracer = otel.Tracer("github.com/jfroy/immich-skylight/internal/sync")

// Syncer coordinates one or more sync passes.
type Syncer struct {
	cfg    *config.Config
	im     *immich.Client
	sky    *skylight.Client
	st     *state.State
	log    *slog.Logger
	rec    *metrics.Recorder
	tags   []string // resolved Immich tag IDs
	frames []string // resolved Skylight frame IDs

	ready atomic.Bool
}

// Options for New.
type Options struct {
	Config  *config.Config
	State   *state.State
	Logger  *slog.Logger
	Metrics *metrics.Recorder
	// SkylightBaseURL overrides the Skylight API base (tests).
	SkylightBaseURL string
}

// New wires up clients and resolves tags and frames. It fails fast on bad
// credentials or unknown tag/frame names.
func New(ctx context.Context, o Options) (*Syncer, error) {
	ctx, span := tracer.Start(ctx, "sync.Setup")
	defer span.End()

	cfg := o.Config
	s := &Syncer{cfg: cfg, st: o.State, log: o.Logger, rec: o.Metrics}

	imHTTP := httpx.NewClient(o.Metrics, func(*http.Request) string { return "immich" }, 5*time.Minute)
	s.im = immich.New(cfg.ImmichURL, cfg.ImmichAPIKey, imHTTP)

	skyHTTP := httpx.NewClient(o.Metrics, nil, 5*time.Minute)
	var err error
	s.sky, err = skylight.New(skylight.Options{
		Email:       cfg.SkylightEmail,
		Password:    cfg.SkylightPassword,
		Fingerprint: o.State.Fingerprint,
		Store:       &tokenStore{o.State},
		HTTPClient:  skyHTTP,
		Logger:      o.Logger,
		Metrics:     o.Metrics,
		BaseURL:     o.SkylightBaseURL,
	})
	if err != nil {
		return nil, err
	}

	if err := s.im.Ping(ctx); err != nil {
		return nil, fail(span, err)
	}
	if s.tags, err = s.im.ResolveTags(ctx, cfg.Tags); err != nil {
		return nil, fail(span, err)
	}
	if err := s.sky.Authenticate(ctx); err != nil {
		return nil, fail(span, err)
	}
	frames, err := s.sky.Frames(ctx)
	if err != nil {
		return nil, fail(span, fmt.Errorf("listing skylight frames: %w", err))
	}
	if s.frames, err = selectFrames(frames, cfg.FrameIDs, cfg.FrameNames); err != nil {
		return nil, fail(span, err)
	}
	for _, f := range frames {
		if contains(s.frames, f.ID) {
			s.log.Info("target frame", "id", f.ID, "name", f.Name)
		}
	}
	s.ready.Store(true)
	return s, nil
}

// Ready reports whether setup completed (used by the readiness probe).
func (s *Syncer) Ready() bool { return s != nil && s.ready.Load() }

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
func (s *Syncer) Once(ctx context.Context) (err error) {
	ctx, span := tracer.Start(ctx, "sync.Pass", trace.WithNewRoot())
	start := time.Now()
	selected := 0
	defer func() {
		result := "success"
		if err != nil {
			result = "failure"
			fail(span, err)
		}
		if s.rec != nil {
			tracked, _ := s.st.Count(context.WithoutCancel(ctx))
			s.rec.SyncFinished(ctx, result, time.Since(start), selected, tracked)
		}
		span.End()
	}()
	log := s.log.With("trace_id", span.SpanContext().TraceID().String())

	assets, err := s.im.Search(ctx, immich.SearchOptions{
		Favorites:    s.cfg.Favorites,
		TagIDs:       s.tags,
		IncludeVideo: s.cfg.IncludeVideo,
	})
	if err != nil {
		return err
	}
	selected = len(assets)

	sel := make(map[string]bool, len(assets))
	var pending []immich.Asset
	tracked, err := s.st.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("reading state: %w", err)
	}
	for _, a := range assets {
		sel[a.ID] = true
		if _, ok := tracked[a.ID]; !ok {
			pending = append(pending, a)
		}
	}
	span.SetAttributes(attribute.Int("selected", len(assets)), attribute.Int("pending", len(pending)))
	log.InfoContext(ctx, "sync pass", "selected", len(assets), "already_sent", len(assets)-len(pending), "pending", len(pending))

	var sent, failed int
	for i := len(pending) - 1; i >= 0; i-- { // oldest first so the frame stays chronological
		a := pending[i]
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.upload(ctx, a); err != nil {
			failed++
			log.WarnContext(ctx, "upload failed", "asset", a.ID, "file", a.OriginalFileName, "err", err)
			continue
		}
		sent++
	}

	var removed int
	if s.cfg.RemoveUnselected {
		removed = s.removeUnselected(ctx, sel, tracked)
	}

	span.SetAttributes(attribute.Int("sent", sent), attribute.Int("failed", failed), attribute.Int("removed", removed))
	log.InfoContext(ctx, "sync pass done", "sent", sent, "failed", failed, "removed", removed, "took", time.Since(start).Round(time.Millisecond))
	if failed > 0 && sent == 0 && len(pending) > 0 {
		return fmt.Errorf("all %d uploads failed", failed)
	}
	return nil
}

func (s *Syncer) upload(ctx context.Context, a immich.Asset) (err error) {
	ctx, span := tracer.Start(ctx, "sync.UploadAsset", trace.WithAttributes(
		attribute.String("asset_id", a.ID), attribute.String("file", a.OriginalFileName)))
	defer func() { fail(span, err); span.End() }()

	data, ext, rendition, err := s.fetch(ctx, a)
	if err != nil {
		s.record(ctx, "fetch_failed", rendition, 0)
		return err
	}
	span.SetAttributes(attribute.String("rendition", rendition), attribute.String("ext", ext), attribute.Int("bytes", len(data)))

	caption := ""
	if s.cfg.UseCaption && a.ExifInfo != nil {
		caption = strings.TrimSpace(a.ExifInfo.Description)
	}

	if s.cfg.DryRun {
		s.log.InfoContext(ctx, "dry-run: would upload", "asset", a.ID, "file", a.OriginalFileName, "ext", ext, "bytes", len(data), "caption", caption)
		s.record(ctx, "dry_run", rendition, len(data))
		return nil
	}
	results, err := s.sky.Upload(ctx, s.frames, ext, data, caption)
	if len(results) > 0 {
		// Persist whatever landed so partial multi-frame uploads are not repeated.
		rec := state.Sent{Messages: map[string][]int{}, Checksum: a.Checksum, SentAt: time.Now()}
		for _, r := range results {
			rec.Messages[r.FrameID] = r.MessageIDs
		}
		if serr := s.st.MarkSent(context.WithoutCancel(ctx), a.ID, rec); serr != nil {
			return fmt.Errorf("recording upload of %s (message ids %v): %w", a.ID, rec.Messages, errors.Join(serr, err))
		}
	}
	if err != nil {
		s.record(ctx, "upload_failed", rendition, len(data))
		return err
	}
	s.record(ctx, "success", rendition, len(data))
	s.log.InfoContext(ctx, "uploaded", "asset", a.ID, "file", a.OriginalFileName, "ext", ext, "bytes", len(data), "frames", len(results))
	return nil
}

// fetch downloads the configured rendition, falling back to Immich's preview
// when the original/fullsize is unavailable or not a format Skylight accepts.
func (s *Syncer) fetch(ctx context.Context, a immich.Asset) (data []byte, ext, rendition string, err error) {
	var chain []immich.Rendition
	switch s.cfg.ImageSource {
	case config.SourceOriginal:
		chain = []immich.Rendition{immich.RenditionOriginal, immich.RenditionFullsize, immich.RenditionPreview}
	case config.SourceFullsize:
		chain = []immich.Rendition{immich.RenditionFullsize, immich.RenditionPreview}
	default:
		chain = []immich.Rendition{immich.RenditionPreview}
	}
	if a.Type == "VIDEO" {
		chain = []immich.Rendition{immich.RenditionOriginal}
	}

	var lastErr error
	for _, r := range chain {
		rendition = string(r)
		if r == immich.RenditionOriginal && skylight.SupportedExt(a.OriginalMimeType) == "" {
			lastErr = fmt.Errorf("original type %s not supported by skylight", a.OriginalMimeType)
			continue
		}
		var mime string
		data, mime, err = s.im.Download(ctx, a.ID, r)
		if err != nil {
			lastErr = fmt.Errorf("download %s: %w", r, err)
			continue
		}
		if ext = skylight.SupportedExt(mime); ext == "" {
			lastErr = fmt.Errorf("rendition %s has unsupported type %q", r, mime)
			continue
		}
		return data, ext, rendition, nil
	}
	return nil, "", rendition, lastErr
}

// removeUnselected deletes photos from the frame that are no longer selected
// in Immich (un-favorited / untagged / deleted).
func (s *Syncer) removeUnselected(ctx context.Context, selected map[string]bool, tracked map[string]state.Sent) int {
	ctx, span := tracer.Start(ctx, "sync.RemoveUnselected")
	defer span.End()

	byFrame := map[string][]int{}
	var stale []string
	for id, rec := range tracked {
		if selected[id] {
			continue
		}
		stale = append(stale, id)
		for f, ids := range rec.Messages {
			byFrame[f] = append(byFrame[f], ids...)
		}
	}
	span.SetAttributes(attribute.Int("stale", len(stale)))
	if len(stale) == 0 {
		return 0
	}
	if s.cfg.DryRun {
		s.log.InfoContext(ctx, "dry-run: would remove", "assets", len(stale))
		return 0
	}
	for f, ids := range byFrame {
		if err := s.sky.Delete(ctx, f, ids); err != nil {
			s.log.WarnContext(ctx, "removing photos from frame failed", "frame", f, "count", len(ids), "err", err)
			if s.rec != nil {
				s.rec.Removed(ctx, "failure", len(ids))
			}
			fail(span, err)
			return 0
		}
	}
	for _, id := range stale {
		if err := s.st.Forget(context.WithoutCancel(ctx), id); err != nil {
			s.log.ErrorContext(ctx, "forgetting removed asset failed", "asset", id, "err", err)
		}
	}
	if s.rec != nil {
		s.rec.Removed(ctx, "success", len(stale))
	}
	return len(stale)
}

func (s *Syncer) record(ctx context.Context, result, rendition string, n int) {
	if s.rec != nil {
		s.rec.Upload(ctx, result, rendition, n)
	}
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

func fail(span trace.Span, err error) error {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

// tokenStore adapts state.State to skylight.TokenStore.
type tokenStore struct{ st *state.State }

func (t *tokenStore) Tokens() (string, string, time.Time) {
	tk, err := t.st.GetTokens(context.Background())
	if err != nil {
		return "", "", time.Time{}
	}
	return tk.AccessToken, tk.RefreshToken, tk.Expiry
}

func (t *tokenStore) SaveTokens(access, refresh string, expiry time.Time) error {
	return t.st.SetTokens(context.Background(), state.Tokens{AccessToken: access, RefreshToken: refresh, Expiry: expiry})
}
