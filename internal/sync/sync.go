// Package sync pushes selected Immich assets to Skylight frames and keeps a
// record of what has been sent.
package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"path/filepath"
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
	var err error

	imClassify := func(*http.Request) string { return "immich" }
	s.im, err = immich.New(cfg.ImmichURL, cfg.ImmichAPIKey, func(rt http.RoundTripper) http.RoundTripper {
		return httpx.NewTransport(rt, o.Metrics, imClassify)
	}, 5*time.Minute)
	if err != nil {
		return nil, fail(span, err)
	}

	skyHTTP := httpx.NewClient(o.Metrics, nil, 5*time.Minute)
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

// setupFrameTags reconciles per-frame Immich tags with the target frames:
//
//   - a frame with no recorded tag gets one upserted from the template;
//   - a frame whose recorded tag no longer matches the rendered path is
//     renamed in place when only the leaf changed (frame renamed in
//     Skylight), otherwise a new tag is upserted and the old one left alone
//     with a warning;
//   - recorded tags for frames that are no longer targets are deleted when
//     PRUNE_FRAME_TAGS is set, else reported.
func (s *Syncer) setupFrameTags(ctx context.Context, frames []skylight.Frame) error {
	s.frameTags = map[string]string{}
	if s.cfg.FrameTagTemplate == "" {
		return nil
	}
	ctx, span := tracer.Start(ctx, "sync.SetupFrameTags")
	defer span.End()

	tmpl, err := template.New("frame_tag").Parse(s.cfg.FrameTagTemplate)
	if err != nil {
		return fmt.Errorf("IMMICH_FRAME_TAG_TEMPLATE: %w", err)
	}
	recorded, err := s.st.FrameTags(ctx)
	if err != nil {
		return fmt.Errorf("reading frame tags: %w", err)
	}
	existing, err := s.im.Tags(ctx)
	if err != nil {
		return err
	}
	byID := map[string]immich.Tag{}
	for _, t := range existing {
		byID[t.ID] = t
	}

	var toUpsert []string
	var upsertFrames []skylight.Frame
	seenValue := map[string]string{}
	for _, f := range frames {
		var b strings.Builder
		if err := tmpl.Execute(&b, f); err != nil {
			return fmt.Errorf("rendering frame tag for %q: %w", f.Name, err)
		}
		want := strings.Trim(strings.TrimSpace(b.String()), "/")
		if want == "" {
			return fmt.Errorf("frame tag template rendered empty for frame %q", f.Name)
		}
		if other, dup := seenValue[want]; dup {
			return fmt.Errorf("frame tag %q resolves to both frame %s and %s; make the template unique per frame", want, other, f.ID)
		}
		seenValue[want] = f.ID

		rec, have := recorded[f.ID]
		cur, alive := byID[rec.TagID]
		switch {
		case have && alive && strings.EqualFold(cur.Value, want):
			s.frameTags[cur.ID] = f.ID
			if cur.Value != rec.TagValue {
				_ = s.st.SetFrameTag(ctx, state.FrameTag{FrameID: f.ID, TagID: cur.ID, TagValue: cur.Value})
			}
		case have && alive && parentOf(cur.Value) == parentOf(want):
			// Frame renamed: rename our tag so its photos follow.
			if err := s.im.RenameTag(ctx, cur.ID, leafOf(want)); err != nil {
				return fmt.Errorf("renaming frame tag %q -> %q (needs tag.update): %w", cur.Value, want, err)
			}
			s.log.InfoContext(ctx, "frame tag renamed", "frame", f.Name, "from", cur.Value, "to", want)
			if err := s.st.SetFrameTag(ctx, state.FrameTag{FrameID: f.ID, TagID: cur.ID, TagValue: want}); err != nil {
				return err
			}
			s.frameTags[cur.ID] = f.ID
		default:
			if have && alive {
				n, _ := s.im.TagAssetCount(ctx, cur.ID)
				s.log.WarnContext(ctx, "frame tag path changed beyond a rename; leaving old tag in place",
					"frame", f.Name, "old", cur.Value, "new", want, "assets_on_old_tag", n)
			}
			toUpsert = append(toUpsert, want)
			upsertFrames = append(upsertFrames, f)
		}
	}

	if len(toUpsert) > 0 {
		tags, err := s.im.UpsertTags(ctx, toUpsert)
		if err != nil {
			return err
		}
		for i, t := range tags {
			f := upsertFrames[i]
			s.frameTags[t.ID] = f.ID
			if err := s.st.SetFrameTag(ctx, state.FrameTag{FrameID: f.ID, TagID: t.ID, TagValue: t.Value}); err != nil {
				return err
			}
			s.log.InfoContext(ctx, "frame tag", "frame", f.Name, "tag", t.Value, "tag_id", t.ID)
		}
	}

	// Stale records: frames no longer targeted.
	targets := map[string]bool{}
	for _, f := range frames {
		targets[f.ID] = true
	}
	for frameID, rec := range recorded {
		if targets[frameID] {
			continue
		}
		cur, alive := byID[rec.TagID]
		if !alive {
			_ = s.st.ForgetFrameTag(ctx, frameID)
			continue
		}
		if !s.cfg.PruneFrameTags {
			n, _ := s.im.TagAssetCount(ctx, cur.ID)
			s.log.WarnContext(ctx, "frame is no longer a target; its tag remains (set PRUNE_FRAME_TAGS=true to delete)",
				"frame_id", frameID, "tag", cur.Value, "assets", n)
			continue
		}
		if err := s.im.DeleteTag(ctx, cur.ID); err != nil {
			return fmt.Errorf("pruning frame tag %q (needs tag.delete): %w", cur.Value, err)
		}
		s.log.InfoContext(ctx, "frame tag pruned", "frame_id", frameID, "tag", cur.Value)
		if err := s.st.ForgetFrameTag(ctx, frameID); err != nil {
			return err
		}
	}
	return nil
}

func parentOf(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[:i]
	}
	return ""
}

func leafOf(path string) string {
	return path[strings.LastIndex(path, "/")+1:]
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
	if s.cfg.UseCaption {
		caption = strings.TrimSpace(a.Description)
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
		if r == immich.RenditionOriginal && skylight.SupportedExt(mimeByName(a.OriginalFileName)) == "" {
			lastErr = fmt.Errorf("original %s is not a type skylight accepts", filepath.Ext(a.OriginalFileName))
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

// mimeByName guesses a MIME type from a filename extension (used only to skip
// downloading originals Skylight cannot display; the server's Content-Type
// decides what is actually uploaded).
func mimeByName(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".mov":
		return "video/quicktime"
	}
	return mime.TypeByExtension(ext)
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
