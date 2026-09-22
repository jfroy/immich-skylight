// Package immich adapts github.com/simulot/immich-go's client to the small
// surface this daemon needs: connectivity check, tag lookup/upsert, metadata
// search, and rendition download.
//
// Everything immich-go covers goes through it. A few endpoints it lacks are
// minimal direct calls using the same API key and instrumented transport:
// Download (immich-go only exposes the original, and this daemon needs the
// server-side preview/fullsize renditions plus the response Content-Type),
// RenameTag and DeleteTag.
package immich

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	immichgo "github.com/simulot/immich-go/immich"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

var tracer = otel.Tracer("github.com/jfroy/immich-skylight/internal/immich")

// Client talks to an Immich server using an API key.
type Client struct {
	ic      *immichgo.ImmichClient
	baseURL string
	apiKey  string
	http    *http.Client
}

// New creates a client. baseURL is the server root (e.g. https://photos.example.com).
// rt, if non-nil, wraps the transport used for all requests (tracing/metrics).
func New(baseURL, apiKey string, rt func(http.RoundTripper) http.RoundTripper, timeout time.Duration) (*Client, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	var deco immichgo.RoundTripperDecorator
	if rt != nil {
		deco = rt
	}
	ic, err := immichgo.NewImmichClient(baseURL, apiKey,
		// immich-go's option assigns InsecureSkipVerify = arg, so false = verify.
		immichgo.OptionVerifySSL(false),
		immichgo.OptionConnectionTimeout(timeout),
		// nil decorator resets to the plain transport, so this is safe unconditionally.
		immichgo.OptionSetAPITrace(deco),
	)
	if err != nil {
		return nil, fmt.Errorf("creating immich client: %w", err)
	}

	var base http.RoundTripper = http.DefaultTransport.(*http.Transport).Clone()
	if rt != nil {
		base = rt(base)
	}
	return &Client{
		ic:      ic,
		baseURL: baseURL,
		apiKey:  apiKey,
		http:    &http.Client{Transport: base, Timeout: timeout},
	}, nil
}

// Asset is the subset of an Immich asset the sync needs.
type Asset struct {
	ID               string
	Type             string // IMAGE or VIDEO
	OriginalFileName string
	Checksum         string
	IsFavorite       bool
	FileCreatedAt    time.Time
	Description      string
}

func fromGo(a *immichgo.Asset) Asset {
	return Asset{
		ID:               a.ID,
		Type:             a.Type,
		OriginalFileName: a.OriginalFileName,
		Checksum:         a.Checksum,
		IsFavorite:       a.IsFavorite,
		FileCreatedAt:    a.FileCreatedAt.Time,
		Description:      a.ExifInfo.Description,
	}
}

// Tag mirrors Immich's TagResponseDto.
type Tag = immichgo.TagSimplified

// Rendition identifies a downloadable version of an asset.
type Rendition string

const (
	RenditionOriginal Rendition = "original"
	RenditionFullsize Rendition = "fullsize"
	RenditionPreview  Rendition = "preview"
)

// Ping verifies connectivity and the API key.
func (c *Client) Ping(ctx context.Context) error {
	if _, err := c.ic.ValidateConnection(ctx); err != nil {
		return fmt.Errorf("immich auth check failed: %w", err)
	}
	return nil
}

// Tags returns all tags visible to the API key.
func (c *Client) Tags(ctx context.Context) ([]Tag, error) {
	return c.ic.GetAllTags(ctx)
}

// ResolveTags maps user-supplied names/paths to tag IDs. Matching is
// case-insensitive against either the full path ("Family/Skylight") or the
// leaf name ("Skylight").
func (c *Client) ResolveTags(ctx context.Context, wanted []string) ([]string, error) {
	if len(wanted) == 0 {
		return nil, nil
	}
	tags, err := c.Tags(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, w := range wanted {
		found := false
		for _, t := range tags {
			if strings.EqualFold(t.Value, w) || strings.EqualFold(t.Name, w) {
				ids = append(ids, t.ID)
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("immich tag %q not found", w)
		}
	}
	return ids, nil
}

// UpsertTags creates hierarchical tags by path ("Skylight/Kitchen") if they do
// not exist and returns them in input order.
func (c *Client) UpsertTags(ctx context.Context, paths []string) ([]Tag, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	ctx, span := tracer.Start(ctx, "immich.UpsertTags", trace.WithAttributes(attribute.StringSlice("paths", paths)))
	defer span.End()
	tags, err := c.ic.UpsertTags(ctx, paths)
	if err != nil {
		recordErr(span, err)
		return nil, fmt.Errorf("upserting immich tags: %w", err)
	}
	out := make([]Tag, 0, len(paths))
	for _, p := range paths {
		found := false
		for _, t := range tags {
			if strings.EqualFold(t.Value, p) {
				out = append(out, t)
				found = true
				break
			}
		}
		if !found {
			err := fmt.Errorf("immich did not return upserted tag %q", p)
			recordErr(span, err)
			return nil, err
		}
	}
	return out, nil
}

// SearchByTag returns all assets carrying a single tag.
func (c *Client) SearchByTag(ctx context.Context, tagID string, includeVideo bool) ([]Asset, error) {
	ctx, span := tracer.Start(ctx, "immich.SearchByTag", trace.WithAttributes(attribute.String("tag_id", tagID)))
	defer span.End()
	out, err := c.searchAll(ctx, &immichgo.SearchMetadataQuery{TagIds: []string{tagID}}, includeVideo)
	recordErr(span, err)
	return out, err
}

// SearchOptions controls Search.
type SearchOptions struct {
	Favorites    bool     // isFavorite=true
	TagIDs       []string // any of these tags
	IncludeVideo bool
}

// Search returns the union of favorite assets and tagged assets, de-duplicated by ID.
func (c *Client) Search(ctx context.Context, opts SearchOptions) (assets []Asset, err error) {
	ctx, span := tracer.Start(ctx, "immich.Search", trace.WithAttributes(
		attribute.Bool("favorites", opts.Favorites), attribute.Int("tag_count", len(opts.TagIDs))))
	defer func() {
		span.SetAttributes(attribute.Int("result_count", len(assets)))
		recordErr(span, err)
		span.End()
	}()
	seen := map[string]bool{}
	add := func(list []Asset) {
		for _, a := range list {
			if !seen[a.ID] {
				seen[a.ID] = true
				assets = append(assets, a)
			}
		}
	}
	if opts.Favorites {
		a, err := c.searchAll(ctx, &immichgo.SearchMetadataQuery{IsFavorite: true}, opts.IncludeVideo)
		if err != nil {
			return nil, fmt.Errorf("searching favorites: %w", err)
		}
		add(a)
	}
	if len(opts.TagIDs) > 0 {
		a, err := c.searchAll(ctx, &immichgo.SearchMetadataQuery{TagIds: opts.TagIDs}, opts.IncludeVideo)
		if err != nil {
			return nil, fmt.Errorf("searching tags: %w", err)
		}
		add(a)
	}
	return assets, nil
}

// searchAll pages through POST /api/search/metadata via immich-go.
func (c *Client) searchAll(ctx context.Context, q *immichgo.SearchMetadataQuery, includeVideo bool) ([]Asset, error) {
	q.WithExif = true
	q.WithDeleted = false
	q.Size = 1000
	var out []Asset
	err := c.ic.GetAllAssetsWithFilter(ctx, q, func(a *immichgo.Asset) error {
		if a.IsTrashed || (a.Type == "VIDEO" && !includeVideo) {
			return nil
		}
		out = append(out, fromGo(a))
		return nil
	})
	return out, err
}

// RenameTag changes a tag's leaf name (its parent cannot change).
func (c *Client) RenameTag(ctx context.Context, id, name string) error {
	ctx, span := tracer.Start(ctx, "immich.RenameTag", trace.WithAttributes(attribute.String("tag_id", id), attribute.String("name", name)))
	defer span.End()
	err := c.doJSON(ctx, http.MethodPut, "/api/tags/"+url.PathEscape(id), map[string]any{"name": name})
	recordErr(span, err)
	return err
}

// DeleteTag deletes a tag; Immich unlinks it from its assets.
func (c *Client) DeleteTag(ctx context.Context, id string) error {
	ctx, span := tracer.Start(ctx, "immich.DeleteTag", trace.WithAttributes(attribute.String("tag_id", id)))
	defer span.End()
	err := c.doJSON(ctx, http.MethodDelete, "/api/tags/"+url.PathEscape(id), nil)
	recordErr(span, err)
	return err
}

// TagAssetCount returns how many (non-trashed) assets carry the tag.
func (c *Client) TagAssetCount(ctx context.Context, tagID string) (int, error) {
	a, err := c.searchAll(ctx, &immichgo.SearchMetadataQuery{TagIds: []string{tagID}}, true)
	return len(a), err
}

// doJSON performs a small JSON request not covered by immich-go.
func (c *Client) doJSON(ctx context.Context, method, path string, body any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &HTTPError{Status: resp.StatusCode, Body: string(b)}
	}
	return nil
}

// Download fetches the given rendition. It returns the bytes and the
// Content-Type reported by the server.
func (c *Client) Download(ctx context.Context, id string, r Rendition) (data []byte, mime string, err error) {
	ctx, span := tracer.Start(ctx, "immich.Download", trace.WithAttributes(
		attribute.String("asset_id", id), attribute.String("rendition", string(r))))
	defer func() {
		span.SetAttributes(attribute.Int("bytes", len(data)), attribute.String("content_type", mime))
		recordErr(span, err)
		span.End()
	}()
	path := "/api/assets/" + url.PathEscape(id) + "/original"
	if r != RenditionOriginal {
		path = "/api/assets/" + url.PathEscape(id) + "/thumbnail?size=" + string(r)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, "", &HTTPError{Status: resp.StatusCode, Body: string(b)}
	}
	if data, err = io.ReadAll(resp.Body); err != nil {
		return nil, "", err
	}
	mime = resp.Header.Get("Content-Type")
	if i := strings.Index(mime, ";"); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	return data, mime, nil
}

// HTTPError is a non-2xx response from a direct call.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("immich http %d: %s", e.Status, e.Body)
}

func recordErr(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
}
