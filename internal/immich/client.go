// Package immich is a minimal client for the parts of the Immich API needed
// to select and download assets.
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

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

var tracer = otel.Tracer("github.com/jfroy/immich-skylight/internal/immich")

// Client talks to an Immich server using an API key.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// New creates a client. baseURL is the server root (e.g. https://photos.example.com).
// hc may be nil, in which case a default client is used.
func New(baseURL, apiKey string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    hc,
	}
}

// Asset is the subset of AssetResponseDto we care about.
type Asset struct {
	ID               string    `json:"id"`
	Type             string    `json:"type"` // IMAGE or VIDEO
	OriginalMimeType string    `json:"originalMimeType"`
	OriginalFileName string    `json:"originalFileName"`
	Checksum         string    `json:"checksum"`
	IsFavorite       bool      `json:"isFavorite"`
	FileCreatedAt    time.Time `json:"fileCreatedAt"`
	ExifInfo         *struct {
		Description string `json:"description"`
	} `json:"exifInfo,omitempty"`
}

// Tag mirrors TagResponseDto.
type Tag struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Value string `json:"value"` // full path, e.g. "Family/Skylight"
}

// Rendition identifies a downloadable version of an asset.
type Rendition string

const (
	RenditionOriginal Rendition = "original"
	RenditionFullsize Rendition = "fullsize"
	RenditionPreview  Rendition = "preview"
)

// Ping verifies connectivity and the API key.
func (c *Client) Ping(ctx context.Context) error {
	var me struct {
		Email string `json:"email"`
	}
	if err := c.getJSON(ctx, "/api/users/me", &me); err != nil {
		return fmt.Errorf("immich auth check failed: %w", err)
	}
	return nil
}

// Tags returns all tags visible to the API key.
func (c *Client) Tags(ctx context.Context) ([]Tag, error) {
	var tags []Tag
	if err := c.getJSON(ctx, "/api/tags", &tags); err != nil {
		return nil, err
	}
	return tags, nil
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
	var tags []Tag
	err := c.putJSON(ctx, "/api/tags", map[string]any{"tags": paths}, &tags)
	if err != nil {
		recordErr(span, err)
		return nil, fmt.Errorf("upserting immich tags: %w", err)
	}
	// The response is not guaranteed to be ordered; re-order by path.
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
	all, err := c.searchAll(ctx, map[string]any{"tagIds": []string{tagID}})
	if err != nil {
		recordErr(span, err)
		return nil, err
	}
	var out []Asset
	for _, a := range all {
		if a.Type == "VIDEO" && !includeVideo {
			continue
		}
		out = append(out, a)
	}
	return out, nil
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
	var out []Asset
	add := func(assets []Asset) {
		for _, a := range assets {
			if seen[a.ID] {
				continue
			}
			if a.Type == "VIDEO" && !opts.IncludeVideo {
				continue
			}
			seen[a.ID] = true
			out = append(out, a)
		}
	}

	if opts.Favorites {
		a, err := c.searchAll(ctx, map[string]any{"isFavorite": true})
		if err != nil {
			return nil, fmt.Errorf("searching favorites: %w", err)
		}
		add(a)
	}
	if len(opts.TagIDs) > 0 {
		a, err := c.searchAll(ctx, map[string]any{"tagIds": opts.TagIDs})
		if err != nil {
			return nil, fmt.Errorf("searching tags: %w", err)
		}
		add(a)
	}
	return out, nil
}

// searchAll pages through POST /api/search/metadata.
func (c *Client) searchAll(ctx context.Context, filter map[string]any) ([]Asset, error) {
	var all []Asset
	page := 1
	for {
		body := map[string]any{
			"page":        page,
			"size":        1000,
			"withDeleted": false,
			"withExif":    true,
			"order":       "desc",
		}
		for k, v := range filter {
			body[k] = v
		}
		var resp struct {
			Assets struct {
				Items    []Asset `json:"items"`
				NextPage *string `json:"nextPage"`
			} `json:"assets"`
		}
		if err := c.postJSON(ctx, "/api/search/metadata", body, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Assets.Items...)
		if resp.Assets.NextPage == nil || *resp.Assets.NextPage == "" || len(resp.Assets.Items) == 0 {
			return all, nil
		}
		page++
	}
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
	var path string
	if r == RenditionOriginal {
		path = "/api/assets/" + url.PathEscape(id) + "/original"
	} else {
		path = "/api/assets/" + url.PathEscape(id) + "/thumbnail?size=" + string(r)
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, "", err
	}
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
	data, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	ct := resp.Header.Get("Content-Type")
	if i := strings.Index(ct, ";"); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	return data, ct, nil
}

// HTTPError is a non-2xx response.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("immich http %d: %s", e.Status, e.Body)
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	return req, nil
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	return c.doJSON(req, out)
}

func (c *Client) postJSON(ctx context.Context, path string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodPost, path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.doJSON(req, out)
}

func (c *Client) putJSON(ctx context.Context, path string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodPut, path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.doJSON(req, out)
}

func (c *Client) doJSON(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &HTTPError{Status: resp.StatusCode, Body: string(body)}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

func recordErr(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
}
