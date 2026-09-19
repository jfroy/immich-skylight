// Package skylight adapts github.com/sebrandon1/go-skylight for this daemon:
// it owns credential lifecycle (headless login, refresh-token rotation,
// persistence, 401 recovery) and exposes just the operations the sync needs.
//
// All protocol details (login flow, endpoints, S3 presign) live in go-skylight
// so fixes there are picked up by bumping the dependency.
package skylight

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sebrandon1/go-skylight/lib"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

var tracer = otel.Tracer("github.com/jfroy/immich-skylight/internal/skylight")

// TokenStore persists OAuth tokens between runs.
type TokenStore interface {
	Tokens() (access, refresh string, expiry time.Time)
	SaveTokens(access, refresh string, expiry time.Time) error
}

// AuthMetrics receives authentication events.
type AuthMetrics interface {
	Auth(ctx context.Context, kind, result string)
}

// Frame is a Skylight frame.
type Frame = lib.Frame

// Options configures New.
type Options struct {
	Email, Password string
	// Fingerprint is a stable per-installation UUID identifying this device.
	Fingerprint string
	Store       TokenStore
	HTTPClient  *http.Client
	Logger      *slog.Logger
	Metrics     AuthMetrics
	// BaseURL overrides the API base (tests).
	BaseURL string
}

// Client is a concurrency-safe, self-healing Skylight client.
type Client struct {
	o    Options
	opts []lib.ClientOption

	mu     sync.Mutex
	c      *lib.Client
	expiry time.Time
}

// New creates a client. No network calls are made until first use.
func New(o Options) (*Client, error) {
	if o.Email == "" || o.Password == "" {
		return nil, errors.New("skylight email and password are required")
	}
	if o.Fingerprint == "" {
		return nil, errors.New("skylight device fingerprint is required")
	}
	if o.Store == nil {
		return nil, errors.New("skylight token store is required")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	var opts []lib.ClientOption
	if o.HTTPClient != nil {
		opts = append(opts, lib.WithHTTPClient(o.HTTPClient))
	}
	if o.BaseURL != "" {
		opts = append(opts, lib.WithBaseURL(o.BaseURL))
	}
	opts = append(opts,
		lib.WithLogger(o.Logger.With("component", "go-skylight")),
		lib.WithRetry(3, 500*time.Millisecond, 10*time.Second),
	)
	return &Client{o: o, opts: opts}, nil
}

// Authenticate ensures a usable session exists.
func (c *Client) Authenticate(ctx context.Context) error {
	_, err := c.client(ctx, false)
	return err
}

// Frames lists frames on the account.
func (c *Client) Frames(ctx context.Context) ([]Frame, error) {
	ctx, span := tracer.Start(ctx, "skylight.ListFrames")
	defer span.End()
	var out []Frame
	err := c.withAuthRetry(ctx, func(lc *lib.Client) error {
		var err error
		out, err = lc.ListFrames(ctx)
		return err
	})
	recordErr(span, err)
	return out, err
}

// UploadResult describes an upload to a single frame.
type UploadResult struct {
	FrameID    string
	MessageIDs []int
}

// Upload sends media to each frame. Uploads are performed per frame so a
// partial failure is reported precisely; results for successful frames are
// returned alongside the error.
func (c *Client) Upload(ctx context.Context, frameIDs []string, ext string, data []byte, caption string) ([]UploadResult, error) {
	ctx, span := tracer.Start(ctx, "skylight.Upload", trace.WithAttributes(
		attribute.String("ext", ext),
		attribute.Int("bytes", len(data)),
		attribute.StringSlice("frame_ids", frameIDs),
	))
	defer span.End()

	var results []UploadResult
	var errs []error
	for _, fid := range frameIDs {
		var res *lib.PhotoUploadResponse
		err := c.withAuthRetry(ctx, func(lc *lib.Client) error {
			var err error
			res, err = lc.UploadPhoto(ctx, fid, ext, data, caption)
			return err
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("frame %s: %w", fid, err))
			continue
		}
		results = append(results, UploadResult{FrameID: fid, MessageIDs: res.MessageIDs})
	}
	err := errors.Join(errs...)
	recordErr(span, err)
	return results, err
}

// Delete removes photos from a frame.
func (c *Client) Delete(ctx context.Context, frameID string, messageIDs []int) error {
	if len(messageIDs) == 0 {
		return nil
	}
	ctx, span := tracer.Start(ctx, "skylight.DeletePhotos", trace.WithAttributes(
		attribute.String("frame_id", frameID), attribute.Int("count", len(messageIDs))))
	defer span.End()
	err := c.withAuthRetry(ctx, func(lc *lib.Client) error {
		return lc.DeletePhotos(ctx, frameID, messageIDs)
	})
	recordErr(span, err)
	return err
}

// withAuthRetry runs fn with a valid client, rebuilding the session once if
// the API reports the token as invalid.
func (c *Client) withAuthRetry(ctx context.Context, fn func(*lib.Client) error) error {
	lc, err := c.client(ctx, false)
	if err != nil {
		return err
	}
	err = fn(lc)
	var authErr *lib.AuthError
	if errors.As(err, &authErr) {
		c.o.Logger.Warn("skylight rejected token; re-authenticating", "err", err)
		if lc, err = c.client(ctx, true); err != nil {
			return err
		}
		err = fn(lc)
	}
	return err
}

// client returns a lib.Client with a token believed valid. With force=true
// or an expired token it refreshes (or, failing that, logs in with password).
func (c *Client) client(ctx context.Context, force bool) (*lib.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.c != nil && !force && time.Now().Before(c.expiry) {
		return c.c, nil
	}

	ctx, span := tracer.Start(ctx, "skylight.Authenticate", trace.WithAttributes(attribute.Bool("force", force)))
	defer span.End()

	now := time.Now()
	_, refresh, _ := c.o.Store.Tokens()

	// 1. Refresh token, if we have one.
	if refresh != "" {
		lc, err := lib.NewClientWithRefreshToken(refresh, c.o.Fingerprint, c.opts...)
		if err == nil {
			c.metric(ctx, "refresh", "success")
			return c.adopt(lc, lc.APIToken, lc.RefreshToken, now.Add(defaultTTL))
		}
		c.metric(ctx, "refresh", "failure")
		c.o.Logger.Warn("skylight refresh failed; falling back to password login", "err", redact(err))
	}

	// 2. Password login.
	tok, err := lib.LoginHeadless(c.o.Email, c.o.Password, c.o.Fingerprint)
	if err != nil {
		c.metric(ctx, "login", "failure")
		recordErr(span, err)
		return nil, fmt.Errorf("skylight login: %w", err)
	}
	c.metric(ctx, "login", "success")
	// UserID is not used for bearer-authenticated calls; the fingerprint is a
	// stable stand-in that satisfies the constructor.
	lc, err := lib.NewClientWithToken(c.o.Fingerprint, tok.AccessToken, c.opts...)
	if err != nil {
		recordErr(span, err)
		return nil, err
	}
	lc.RefreshToken = tok.RefreshToken
	ttl := time.Duration(tok.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = defaultTTL
	}
	return c.adopt(lc, tok.AccessToken, tok.RefreshToken, now.Add(ttl))
}

// defaultTTL is used when the token endpoint does not report expires_in
// (NewClientWithRefreshToken discards it). Refresh is cheap, so stay short.
const defaultTTL = 50 * time.Minute

func (c *Client) adopt(lc *lib.Client, access, refresh string, expiry time.Time) (*lib.Client, error) {
	// Refresh a minute early to avoid racing expiry mid-pass.
	c.expiry = expiry.Add(-time.Minute)
	c.c = lc
	if err := c.o.Store.SaveTokens(access, refresh, c.expiry); err != nil {
		return nil, fmt.Errorf("persisting skylight tokens: %w", err)
	}
	return lc, nil
}

func (c *Client) metric(ctx context.Context, kind, result string) {
	if c.o.Metrics != nil {
		c.o.Metrics.Auth(ctx, kind, result)
	}
}

// SupportedExt maps a MIME type to the extension Skylight expects, or "".
func SupportedExt(mime string) string {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "image/jpeg", "image/jpg":
		return "jpg"
	case "image/png":
		return "png"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	case "video/mp4":
		return "mp4"
	case "video/quicktime":
		return "mov"
	}
	return ""
}

func recordErr(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
}

// redact trims response bodies that may echo tokens out of error strings.
func redact(err error) string {
	s := err.Error()
	if i := strings.Index(s, "{"); i > 0 {
		return s[:i] + "{…}"
	}
	return s
}

// SetAuthBaseURL points go-skylight's OAuth/login endpoints at base
// (scheme+host). Intended for tests against a fake server.
func SetAuthBaseURL(base string) {
	base = strings.TrimRight(base, "/")
	lib.AuthSessionURL = base + "/auth/session"
	lib.OAuthAuthorizeURL = base + "/oauth/authorize"
	lib.OAuthURL = base + "/oauth/token"
}

// ExpireForTest marks the current session expired so the next call refreshes.
func (c *Client) ExpireForTest() {
	c.mu.Lock()
	c.expiry = time.Time{}
	c.mu.Unlock()
}
