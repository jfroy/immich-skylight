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
	"text/template"
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
	tags   []string // resolved Immich tag IDs (apply to all frames)
	frames []string // resolved Skylight frame IDs, in configured order
	// frameTags maps Immich tag ID -> Skylight frame ID for per-frame tags.
	frameTags map[string]string

	ready   atomic.Bool
	trigger chan struct{}
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
	s := &Syncer{cfg: cfg, st: o.State, log: o.Logger, rec: o.Metrics, trigger: make(chan struct{}, 1)}

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
	var targets []skylight.Frame
	for _, id := range s.frames {
		for _, f := range frames {
			if f.ID == id {
				targets = append(targets, f)
				s.log.Info("target frame", "id", f.ID, "name", f.Name)
			}
		}
	}
	if err := s.setupFrameTags(ctx, targets); err != nil {
		return nil, fail(span, err)
	}
	s.ready.Store(true)
	return s, nil
}

// setupFrameTags renders the tag template for each target frame and makes sure
// the tags exist in Immich.
func (s *Syncer) setupFrameTags(ctx context.Context, frames []skylight.Frame) error {
	s.frameTags = map[string]string{}
	if s.cfg.FrameTagTemplate == "" {
		return nil
	}
	tmpl, err := template.New("frame_tag").Parse(s.cfg.FrameTagTemplate)
	if err != nil {
		return fmt.Errorf("IMMICH_FRAME_TAG_TEMPLATE: %w", err)
	}
	paths := make([]string, 0, len(frames))
	for _, f := range frames {
		var b strings.Builder
		if err := tmpl.Execute(&b, f); err != nil {
			return fmt.Errorf("rendering frame tag for %q: %w", f.Name, err)
		}
		p := strings.Trim(strings.TrimSpace(b.String()), "/")
		if p == "" {
			return fmt.Errorf("frame tag template rendered empty for frame %q", f.Name)
		}
		paths = append(paths, p)
	}
	tags, err := s.im.UpsertTags(ctx, paths)
	if err != nil {
		return err
	}
	for i, t := range tags {
		if prev, dup := s.frameTags[t.ID]; dup {
			return fmt.Errorf("frame tag %q resolves to both frame %s and %s; make the template unique per frame", t.Value, prev, frames[i].ID)
		}
		s.frameTags[t.ID] = frames[i].ID
		s.log.Info("frame tag", "frame", frames[i].Name, "tag", t.Value, "tag_id", t.ID)
	}
	return nil
}

// Trigger requests an immediate sync pass from Run. It never blocks; if a
// pass is already queued the request is coalesced.
func (s *Syncer) Trigger() {
	select {
	case s.trigger <- struct{}{}:
	default:
	}
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
		case <-s.trigger:
			s.log.Info("sync triggered by webhook")
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

	desired, assets, err := s.desired(ctx)
	if err != nil {
		return err
	}
	selected = len(desired)

	tracked, err := s.st.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("reading state: %w", err)
	}

	// Diff desired (asset -> frames) against tracked (asset -> frame -> msgs).
	type job struct {
		asset  immich.Asset
		frames []string
	}
	var uploads []job
	for _, a := range assets { // Search order is newest-first; reversed below
		want := desired[a.ID]
		have := tracked[a.ID].Messages
		var missing []string
		for _, f := range s.frames {
			if want[f] && len(have[f]) == 0 {
				missing = append(missing, f)
			}
		}
		if len(missing) > 0 {
			uploads = append(uploads, job{a, missing})
		}
	}
	span.SetAttributes(attribute.Int("selected", len(desired)), attribute.Int("pending", len(uploads)))
	log.InfoContext(ctx, "sync pass", "selected", len(desired), "pending_uploads", len(uploads))

	var sent, failed int
	for i := len(uploads) - 1; i >= 0; i-- { // oldest first so the frame stays chronological
		j := uploads[i]
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.upload(ctx, j.asset, j.frames, tracked[j.asset.ID]); err != nil {
			failed++
			log.WarnContext(ctx, "upload failed", "asset", j.asset.ID, "file", j.asset.OriginalFileName, "err", err)
			continue
		}
		sent++
	}

	var removed int
	if s.cfg.RemoveUnselected {
		// Re-read: uploads above changed per-frame records (e.g. an asset moved
		// between frames), and removal must not act on the stale snapshot.
		if tracked, err = s.st.Snapshot(ctx); err != nil {
			return fmt.Errorf("re-reading state: %w", err)
		}
		removed = s.removeUnselected(ctx, desired, tracked)
	}

	span.SetAttributes(attribute.Int("sent", sent), attribute.Int("failed", failed), attribute.Int("removed", removed))
	log.InfoContext(ctx, "sync pass done", "sent", sent, "failed", failed, "removed", removed, "took", time.Since(start).Round(time.Millisecond))
	if failed > 0 && sent == 0 && len(uploads) > 0 {
		return fmt.Errorf("all %d uploads failed", failed)
	}
	return nil
}

// desired computes, for every selected asset, the set of frames it should be
// on: favorites and IMMICH_TAGS go to all target frames; a per-frame tag adds
// only its frame. Assets are returned in search order (newest first).
func (s *Syncer) desired(ctx context.Context) (map[string]map[string]bool, []immich.Asset, error) {
	desired := map[string]map[string]bool{}
	var order []immich.Asset
	add := func(a immich.Asset, frames []string) {
		set, ok := desired[a.ID]
		if !ok {
			set = map[string]bool{}
			desired[a.ID] = set
			order = append(order, a)
		}
		for _, f := range frames {
			set[f] = true
		}
	}

	if s.cfg.Favorites || len(s.tags) > 0 {
		assets, err := s.im.Search(ctx, immich.SearchOptions{
			Favorites: s.cfg.Favorites, TagIDs: s.tags, IncludeVideo: s.cfg.IncludeVideo,
		})
		if err != nil {
			return nil, nil, err
		}
		for _, a := range assets {
			add(a, s.frames)
		}
	}
	for tagID, frameID := range s.frameTags {
		assets, err := s.im.SearchByTag(ctx, tagID, s.cfg.IncludeVideo)
		if err != nil {
			return nil, nil, fmt.Errorf("searching frame tag: %w", err)
		}
		for _, a := range assets {
			add(a, []string{frameID})
		}
	}
	return desired, order, nil
}

func (s *Syncer) upload(ctx context.Context, a immich.Asset, frames []string, existing state.Sent) (err error) {
	ctx, span := tracer.Start(ctx, "sync.UploadAsset", trace.WithAttributes(
		attribute.String("asset_id", a.ID), attribute.String("file", a.OriginalFileName),
		attribute.StringSlice("frame_ids", frames)))
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
	results, err := s.sky.Upload(ctx, frames, ext, data, caption)
	if len(results) > 0 {
		// Persist whatever landed (merged with frames already present) so
		// partial multi-frame uploads are not repeated.
		rec := state.Sent{Messages: map[string][]int{}, Checksum: a.Checksum, SentAt: time.Now()}
		for f, ids := range existing.Messages {
			rec.Messages[f] = ids
		}
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

// removeUnselected deletes photos from frames they are no longer desired on
// (un-favorited / untagged / deleted in Immich, or moved to a different frame).
func (s *Syncer) removeUnselected(ctx context.Context, desired map[string]map[string]bool, tracked map[string]state.Sent) int {
	ctx, span := tracer.Start(ctx, "sync.RemoveUnselected")
	defer span.End()

	byFrame := map[string][]int{}  // frame -> message ids to delete
	stale := map[string][]string{} // asset -> frames to drop from state
	for id, rec := range tracked {
		for f, ids := range rec.Messages {
			if desired[id][f] {
				continue
			}
			byFrame[f] = append(byFrame[f], ids...)
			stale[id] = append(stale[id], f)
		}
	}
	span.SetAttributes(attribute.Int("stale_assets", len(stale)))
	if len(stale) == 0 {
		return 0
	}
	if s.cfg.DryRun {
		s.log.InfoContext(ctx, "dry-run: would remove", "assets", len(stale))
		return 0
	}
	failedFrames := map[string]bool{}
	for f, ids := range byFrame {
		if err := s.sky.Delete(ctx, f, ids); err != nil {
			s.log.WarnContext(ctx, "removing photos from frame failed", "frame", f, "count", len(ids), "err", err)
			if s.rec != nil {
				s.rec.Removed(ctx, "failure", len(ids))
			}
			fail(span, err)
			failedFrames[f] = true
		}
	}
	removed := 0
	for id, frames := range stale {
		rec := tracked[id]
		for _, f := range frames {
			if !failedFrames[f] {
				delete(rec.Messages, f)
			}
		}
		var err error
		if len(rec.Messages) == 0 {
			err = s.st.Forget(context.WithoutCancel(ctx), id)
		} else {
			err = s.st.MarkSent(context.WithoutCancel(ctx), id, rec)
		}
		if err != nil {
			s.log.ErrorContext(ctx, "updating state after removal failed", "asset", id, "err", err)
			continue
		}
		removed++
	}
	if s.rec != nil && removed > 0 {
		s.rec.Removed(ctx, "success", removed)
	}
	return removed
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
