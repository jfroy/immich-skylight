// Package immich is a small, dependency-free client for the parts of the
// Immich API this daemon uses: connectivity check, tag lookup and management,
// metadata search, and rendition download.
//
// Requests carry the API key in x-api-key. Transient failures (429, 5xx,
// transport errors) on idempotent calls are retried with exponential backoff
// and jitter, honoring Retry-After. Non-2xx responses surface as *Error with
// the server's message; errors.Is(err, ErrUnauthorized) etc. work through
// wrapping.
package immich

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"math/rand/v2"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

var tracer = otel.Tracer("github.com/jfroy/immich-skylight/internal/immich")

// Sentinel errors matched via errors.Is against *Error.
var (
	ErrUnauthorized = errors.New("immich: unauthorized")
	ErrForbidden    = errors.New("immich: forbidden")
	ErrNotFound     = errors.New("immich: not found")
)

// Error is a non-2xx response.
type Error struct {
	Method, Path string
	StatusCode   int
	// Message is Immich's error message when the body was its standard
	// {"message","error","statusCode"} shape, else the (truncated) raw body.
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("immich: %s %s: %d %s", e.Method, e.Path, e.StatusCode, e.Message)
}

// Is maps status codes onto the sentinel errors.
func (e *Error) Is(target error) bool {
	switch target {
	case ErrUnauthorized:
		return e.StatusCode == http.StatusUnauthorized
	case ErrForbidden:
		return e.StatusCode == http.StatusForbidden
	case ErrNotFound:
		return e.StatusCode == http.StatusNotFound
	}
	return false
}

// Options configures New.
type Options struct {
	// BaseURL is the server root, e.g. https://photos.example.com.
	BaseURL string
	APIKey  string
	// HTTPClient is used for all requests. Its Transport may add tracing and
	// metrics. If nil, a client with sane timeouts is created.
	HTTPClient *http.Client
	// UserAgent is sent with every request.
	UserAgent string
	// MaxRetries bounds retries of transient failures (default 3).
	MaxRetries int
	// MaxDownloadBytes caps a single rendition download (default 512 MiB).
	MaxDownloadBytes int64
}

// Client talks to one Immich server.
type Client struct {
	base       *url.URL
	apiKey     string
	http       *http.Client
	ua         string
	maxRetries int
	maxBytes   int64
}

// New validates options and returns a client. No network calls are made.
func New(o Options) (*Client, error) {
	if o.APIKey == "" {
		return nil, errors.New("immich: API key is required")
	}
	base, err := url.Parse(strings.TrimRight(o.BaseURL, "/"))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("immich: invalid base URL %q", o.BaseURL)
	}
	hc := o.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}
	return &Client{
		base:       base,
		apiKey:     o.APIKey,
		http:       hc,
		ua:         cmpOr(o.UserAgent, "immich-skylight"),
		maxRetries: cmpOr(o.MaxRetries, 3),
		maxBytes:   cmpOr(o.MaxDownloadBytes, 512<<20),
	}, nil
}

func cmpOr[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}
	return v
}

// Asset is the subset of AssetResponseDto the sync needs.
type Asset struct {
	ID               string    `json:"id"`
	Type             string    `json:"type"` // IMAGE or VIDEO
	OriginalMimeType string    `json:"originalMimeType"`
	OriginalFileName string    `json:"originalFileName"`
	Checksum         string    `json:"checksum"`
	IsFavorite       bool      `json:"isFavorite"`
	IsTrashed        bool      `json:"isTrashed"`
	FileCreatedAt    time.Time `json:"fileCreatedAt"`
	ExifInfo         struct {
		Description string `json:"description"`
	} `json:"exifInfo"`
}

// Description returns the user-visible caption, if any.
func (a Asset) Description() string { return strings.TrimSpace(a.ExifInfo.Description) }

// Tag mirrors TagResponseDto.
type Tag struct {
	ID       string `json:"id"`
	Name     string `json:"name"`  // leaf
	Value    string `json:"value"` // full path, e.g. "Skylight/Kitchen"
	ParentID string `json:"parentId"`
}

// User is the subset of UserResponseDto used by Me.
type User struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

// Rendition identifies a downloadable version of an asset.
type Rendition string

const (
	RenditionOriginal Rendition = "original"
	RenditionFullsize Rendition = "fullsize"
	RenditionPreview  Rendition = "preview"
)

// Blob is a downloaded rendition.
type Blob struct {
	Data        []byte
	ContentType string // media type without parameters, e.g. "image/jpeg"
}

// Me returns the user owning the API key; use it as a connectivity check.
func (c *Client) Me(ctx context.Context) (User, error) {
	var u User
	err := c.do(ctx, http.MethodGet, "/api/users/me", nil, &u)
	return u, err
}

// Tags lists all tags visible to the key.
func (c *Client) Tags(ctx context.Context) ([]Tag, error) {
	var tags []Tag
	err := c.do(ctx, http.MethodGet, "/api/tags", nil, &tags)
	return tags, err
}

// ResolveTags maps names or full paths to tag IDs, case-insensitively.
func (c *Client) ResolveTags(ctx context.Context, wanted []string) ([]string, error) {
	if len(wanted) == 0 {
		return nil, nil
	}
	tags, err := c.Tags(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(wanted))
	for _, w := range wanted {
		found := false
		for _, t := range tags {
			if strings.EqualFold(t.Value, w) || strings.EqualFold(t.Name, w) {
				ids = append(ids, t.ID)
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("immich: tag %q not found", w)
		}
	}
	return ids, nil
}

// UpsertTags creates hierarchical tags by path if missing and returns them in
// input order.
func (c *Client) UpsertTags(ctx context.Context, paths []string) ([]Tag, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	ctx, span := tracer.Start(ctx, "immich.UpsertTags", trace.WithAttributes(attribute.StringSlice("paths", paths)))
	defer span.End()
	var got []Tag
	if err := c.do(ctx, http.MethodPut, "/api/tags", map[string]any{"tags": paths}, &got); err != nil {
		return nil, recordErr(span, err)
	}
	out := make([]Tag, 0, len(paths))
	for _, p := range paths {
		i := indexFunc(got, func(t Tag) bool { return strings.EqualFold(t.Value, p) })
		if i < 0 {
			return nil, recordErr(span, fmt.Errorf("immich: upsert did not return tag %q", p))
		}
		out = append(out, got[i])
	}
	return out, nil
}

// RenameTag changes a tag's leaf name; its parent cannot change.
func (c *Client) RenameTag(ctx context.Context, id, name string) error {
	if strings.Contains(name, "/") {
		return fmt.Errorf("immich: tag name %q must not contain '/'", name)
	}
	ctx, span := tracer.Start(ctx, "immich.RenameTag", trace.WithAttributes(attribute.String("tag_id", id), attribute.String("name", name)))
	defer span.End()
	return recordErr(span, c.do(ctx, http.MethodPut, "/api/tags/"+url.PathEscape(id), map[string]any{"name": name}, nil))
}

// DeleteTag deletes a tag; Immich unlinks it from its assets. Deleting a tag
// that is already gone is not an error.
func (c *Client) DeleteTag(ctx context.Context, id string) error {
	ctx, span := tracer.Start(ctx, "immich.DeleteTag", trace.WithAttributes(attribute.String("tag_id", id)))
	defer span.End()
	err := c.do(ctx, http.MethodDelete, "/api/tags/"+url.PathEscape(id), nil, nil)
	if errors.Is(err, ErrNotFound) {
		err = nil
	}
	return recordErr(span, err)
}

// Query selects assets for Assets. Zero-valued fields are omitted.
type Query struct {
	IsFavorite bool
	TagIDs     []string
	// IncludeVideo keeps VIDEO assets; IMAGE assets are always included.
	IncludeVideo bool
}

// Assets streams every non-trashed asset matching q, newest first, fetching
// pages lazily. Iteration stops at the first error, which is yielded with a
// zero Asset.
func (c *Client) Assets(ctx context.Context, q Query) iter.Seq2[Asset, error] {
	return func(yield func(Asset, error) bool) {
		ctx, span := tracer.Start(ctx, "immich.Assets", trace.WithAttributes(
			attribute.Bool("favorites", q.IsFavorite), attribute.Int("tag_count", len(q.TagIDs))))
		defer span.End()

		type page struct {
			Assets struct {
				Items      []Asset `json:"items"`
				NextCursor *string `json:"nextCursor"`
				NextPage   *string `json:"nextPage"` // pre-3.2 servers
			} `json:"assets"`
		}
		// page is deprecated since Immich 3.2 (cursor replaces it) but harmless
		// and keeps pre-3.2 servers working.
		body := map[string]any{"page": 1, "size": 1000, "withExif": true, "withDeleted": false}
		if q.IsFavorite {
			body["isFavorite"] = true
		}
		if len(q.TagIDs) > 0 {
			body["tagIds"] = q.TagIDs
		}
		pageNo, total := 1, 0
		for {
			var p page
			if err := c.do(ctx, http.MethodPost, "/api/search/metadata", body, &p); err != nil {
				yield(Asset{}, recordErr(span, err))
				return
			}
			for _, a := range p.Assets.Items {
				if a.IsTrashed || (a.Type == "VIDEO" && !q.IncludeVideo) {
					continue
				}
				total++
				if !yield(a, nil) {
					span.SetAttributes(attribute.Int("yielded", total))
					return
				}
			}
			switch {
			case p.Assets.NextCursor != nil && *p.Assets.NextCursor != "":
				delete(body, "page")
				body["cursor"] = *p.Assets.NextCursor
			case p.Assets.NextPage != nil && *p.Assets.NextPage != "" && len(p.Assets.Items) > 0:
				pageNo++
				body["page"] = pageNo
			default:
				span.SetAttributes(attribute.Int("yielded", total))
				return
			}
		}
	}
}

// Collect drains an Assets iterator into a slice.
func Collect(seq iter.Seq2[Asset, error]) ([]Asset, error) {
	var out []Asset
	for a, err := range seq {
		if err != nil {
			return out, err
		}
		out = append(out, a)
	}
	return out, nil
}

// Download fetches a rendition. Renditions other than the original are served
// from /thumbnail?size=...; Immich may redirect fullsize to the original.
func (c *Client) Download(ctx context.Context, id string, r Rendition) (_ *Blob, err error) {
	ctx, span := tracer.Start(ctx, "immich.Download", trace.WithAttributes(
		attribute.String("asset_id", id), attribute.String("rendition", string(r))))
	defer func() { recordErr(span, err); span.End() }()

	path := "/api/assets/" + url.PathEscape(id) + "/original"
	if r != RenditionOriginal {
		path = "/api/assets/" + url.PathEscape(id) + "/thumbnail?size=" + url.QueryEscape(string(r))
	}
	resp, err := c.roundTrip(ctx, http.MethodGet, path, nil, "application/octet-stream")
	if err != nil {
		return nil, err
	}
	defer drain(resp.Body)

	if resp.ContentLength > c.maxBytes {
		return nil, fmt.Errorf("immich: asset %s rendition %s is %d bytes, over the %d byte limit", id, r, resp.ContentLength, c.maxBytes)
	}
	data, err := io.ReadAll(http.MaxBytesReader(nil, resp.Body, c.maxBytes))
	if err != nil {
		return nil, fmt.Errorf("immich: reading %s: %w", path, err)
	}
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	span.SetAttributes(attribute.Int("bytes", len(data)), attribute.String("content_type", ct))
	return &Blob{Data: data, ContentType: ct}, nil
}

// do performs a JSON request and decodes a JSON response into out (if non-nil).
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return fmt.Errorf("immich: encoding %s %s: %w", method, path, err)
		}
	}
	resp, err := c.roundTrip(ctx, method, path, body, "application/json")
	if err != nil {
		return err
	}
	defer drain(resp.Body)
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("immich: decoding %s %s: %w", method, path, err)
	}
	return nil
}

// roundTrip sends one request with retries and returns a 2xx response whose
// body the caller must close. Non-2xx responses become *Error.
func (c *Client) roundTrip(ctx context.Context, method, path string, body []byte, accept string) (*http.Response, error) {
	u := c.base.JoinPath()
	u.Path, u.RawQuery = splitPath(path)
	u.Path = c.base.Path + u.Path

	var lastErr error
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("x-api-key", c.apiKey)
		req.Header.Set("Accept", accept)
		req.Header.Set("User-Agent", c.ua)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.http.Do(req)
		var delay time.Duration
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = fmt.Errorf("immich: %s %s: %w", method, path, err)
		case resp.StatusCode/100 == 2:
			return resp, nil
		default:
			apiErr := newError(method, path, resp)
			drain(resp.Body)
			if !retryable(resp.StatusCode) || attempt >= c.maxRetries {
				return nil, apiErr
			}
			lastErr = apiErr
			delay = retryAfter(resp)
		}
		if attempt >= c.maxRetries {
			return nil, lastErr
		}
		if delay == 0 {
			delay = backoff(attempt)
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), lastErr)
		case <-time.After(delay):
		}
	}
}

func newError(method, path string, resp *http.Response) *Error {
	e := &Error{Method: method, Path: path, StatusCode: resp.StatusCode}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	var body struct {
		Message any `json:"message"`
	}
	if json.Unmarshal(raw, &body) == nil && body.Message != nil {
		switch m := body.Message.(type) {
		case string:
			e.Message = m
		case []any:
			parts := make([]string, 0, len(m))
			for _, p := range m {
				parts = append(parts, fmt.Sprint(p))
			}
			e.Message = strings.Join(parts, "; ")
		}
	}
	if e.Message == "" {
		e.Message = strings.TrimSpace(string(raw))
		if e.Message == "" {
			e.Message = http.StatusText(resp.StatusCode)
		}
	}
	return e
}

func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusRequestTimeout || status >= 500
}

func retryAfter(resp *http.Response) time.Duration {
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return min(time.Duration(secs)*time.Second, 30*time.Second)
	}
	if t, err := http.ParseTime(v); err == nil {
		return min(max(time.Until(t), 0), 30*time.Second)
	}
	return 0
}

// backoff returns 250ms·2^attempt with ±25% jitter, capped at 10s.
func backoff(attempt int) time.Duration {
	d := min(250*time.Millisecond<<attempt, 10*time.Second)
	jitter := time.Duration(rand.Int64N(int64(d)/2)) - d/4
	return d + jitter
}

func splitPath(p string) (path, query string) {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i], p[i+1:]
	}
	return p, ""
}

func drain(rc io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 64<<10))
	_ = rc.Close()
}

func indexFunc[S ~[]E, E any](s S, f func(E) bool) int {
	for i := range s {
		if f(s[i]) {
			return i
		}
	}
	return -1
}

func recordErr(span trace.Span, err error) error {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}
