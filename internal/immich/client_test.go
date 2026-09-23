package immich

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, h http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(Options{BaseURL: srv.URL, APIKey: "k", MaxRetries: 3})
	if err != nil {
		t.Fatal(err)
	}
	return c, srv
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Options{BaseURL: "https://x", APIKey: ""}); err == nil {
		t.Error("missing key accepted")
	}
	for _, u := range []string{"", "photos.example.com", "://bad"} {
		if _, err := New(Options{BaseURL: u, APIKey: "k"}); err == nil {
			t.Errorf("bad url %q accepted", u)
		}
	}
}

func TestHeadersAndBasePath(t *testing.T) {
	var got http.Header
	var path string
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, path = r.Header.Clone(), r.URL.Path
		fmt.Fprint(w, `{"id":"u1","email":"a@b"}`)
	}))
	// Base URL with a sub-path must be preserved.
	c2, err := New(Options{BaseURL: srv.URL + "/immich/", APIKey: "k", UserAgent: "ua/1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Me(context.Background()); err != nil {
		t.Fatal(err)
	}
	if path != "/immich/api/users/me" {
		t.Errorf("path = %s", path)
	}
	if got.Get("x-api-key") != "k" || got.Get("User-Agent") != "ua/1" || got.Get("Accept") != "application/json" {
		t.Errorf("headers = %v", got)
	}
	_ = c
}

func TestErrorParsingAndSentinels(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/users/me":
			http.Error(w, `{"message":"Invalid API key","error":"Unauthorized","statusCode":401}`, 401)
		case "/api/tags/gone":
			http.Error(w, `{"message":["a","b"],"statusCode":404}`, 404)
		case "/api/tags/plain":
			http.Error(w, "nope", 403)
		}
	}))
	ctx := context.Background()

	_, err := c.Me(ctx)
	var e *Error
	if !errors.As(err, &e) || e.StatusCode != 401 || e.Message != "Invalid API key" || !errors.Is(err, ErrUnauthorized) {
		t.Errorf("401: %v", err)
	}
	if err := c.RenameTag(ctx, "gone", "x"); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "a; b") {
		t.Errorf("404 array message: %v", err)
	}
	if err := c.RenameTag(ctx, "plain", "x"); !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "nope") {
		t.Errorf("403 plain body: %v", err)
	}
	// Deleting an already-deleted tag is fine.
	if err := c.DeleteTag(ctx, "gone"); err != nil {
		t.Errorf("delete missing tag: %v", err)
	}
	if err := c.RenameTag(ctx, "x", "a/b"); err == nil {
		t.Error("slash in tag name accepted")
	}
}

func TestRetryTransientWithRetryAfter(t *testing.T) {
	var n atomic.Int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch n.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "0")
			http.Error(w, "busy", 429)
		case 2:
			http.Error(w, "boom", 503)
		default:
			fmt.Fprint(w, `{"id":"u1"}`)
		}
	}))
	start := time.Now()
	if _, err := c.Me(context.Background()); err != nil {
		t.Fatalf("expected success after retries: %v", err)
	}
	if n.Load() != 3 {
		t.Errorf("attempts = %d", n.Load())
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("retries took too long: %s", time.Since(start))
	}
}

func TestNoRetryOn4xxAndBoundedRetries(t *testing.T) {
	var n atomic.Int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		if r.URL.Path == "/api/tags" {
			http.Error(w, "bad", 400)
			return
		}
		http.Error(w, "down", 500)
	}))
	ctx := context.Background()
	if _, err := c.Tags(ctx); err == nil || n.Load() != 1 {
		t.Errorf("400 should not retry: err=%v attempts=%d", err, n.Load())
	}
	n.Store(0)
	_, err := c.Me(ctx)
	var e *Error
	if !errors.As(err, &e) || e.StatusCode != 500 || n.Load() != 4 { // 1 + MaxRetries
		t.Errorf("500: err=%v attempts=%d", err, n.Load())
	}
}

func TestRetryRespectsContext(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		http.Error(w, "busy", 503)
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := c.Me(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected deadline error, got %v", err)
	}
}

func TestAssetsCursorPagination(t *testing.T) {
	var bodies []map[string]any
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		items := []map[string]any{{"id": "a", "type": "IMAGE"}, {"id": "v", "type": "VIDEO"}, {"id": "t", "type": "IMAGE", "isTrashed": true}}
		var next any = "c2"
		if b["cursor"] == "c2" {
			items = []map[string]any{{"id": "b", "type": "IMAGE"}}
			next = nil
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"assets": map[string]any{"items": items, "nextCursor": next}})
	}))
	got, err := Collect(c.Assets(context.Background(), Query{IsFavorite: true}))
	if err != nil {
		t.Fatal(err)
	}
	if ids := idsOf(got); ids != "a,b" { // video + trashed filtered
		t.Errorf("ids = %s", ids)
	}
	if len(bodies) != 2 || bodies[0]["isFavorite"] != true || bodies[1]["cursor"] != "c2" || bodies[1]["page"] != nil {
		t.Errorf("request bodies = %v", bodies)
	}
}

func TestAssetsLegacyPagePagination(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		page, _ := b["page"].(float64)
		items := []map[string]any{{"id": fmt.Sprintf("p%d", int(page)), "type": "IMAGE"}}
		var next any = fmt.Sprint(int(page) + 1)
		if page == 3 {
			next = nil
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"assets": map[string]any{"items": items, "nextPage": next}})
	}))
	got, err := Collect(c.Assets(context.Background(), Query{TagIDs: []string{"t"}, IncludeVideo: true}))
	if err != nil || idsOf(got) != "p1,p2,p3" {
		t.Errorf("legacy pagination: %v %s", err, idsOf(got))
	}
}

func TestAssetsEarlyBreakAndError(t *testing.T) {
	var calls atomic.Int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			http.Error(w, `{"message":"kaboom"}`, 400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"assets": map[string]any{
			"items": []map[string]any{{"id": "a", "type": "IMAGE"}, {"id": "b", "type": "IMAGE"}}, "nextCursor": "more"}})
	}))
	seen := 0
	for a, err := range c.Assets(context.Background(), Query{}) {
		if err != nil {
			t.Fatal(err)
		}
		seen++
		if a.ID == "a" {
			break // consumer stops; no second request should be made
		}
	}
	if seen != 1 || calls.Load() != 1 {
		t.Errorf("early break: seen=%d calls=%d", seen, calls.Load())
	}
	// Error mid-stream is yielded once and stops iteration.
	var errs int
	for _, err := range c.Assets(context.Background(), Query{}) {
		if err != nil {
			errs++
			if !strings.Contains(err.Error(), "kaboom") {
				t.Errorf("unexpected err: %v", err)
			}
		}
	}
	if errs != 1 {
		t.Errorf("errors yielded = %d", errs)
	}
}

func TestUpsertTagsOrdersByInput(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"id":"2","name":"B","value":"X/B"},{"id":"1","name":"A","value":"x/a"}]`)
	}))
	tags, err := c.UpsertTags(context.Background(), []string{"X/A", "X/B"})
	if err != nil || len(tags) != 2 || tags[0].ID != "1" || tags[1].ID != "2" {
		t.Errorf("upsert order: %v %v", tags, err)
	}
	if _, err := c.UpsertTags(context.Background(), []string{"X/C"}); err == nil {
		t.Error("missing tag in response not reported")
	}
}

func TestDownload(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/original"):
			w.Header().Set("Content-Type", "image/heic")
			fmt.Fprint(w, "ORIG")
		case r.URL.Query().Get("size") == "preview":
			w.Header().Set("Content-Type", "image/jpeg; charset=binary")
			fmt.Fprint(w, "PREVIEW")
		case strings.HasSuffix(r.URL.Path, "/video/playback"):
			w.Header().Set("Content-Type", "video/mp4")
			w.(http.Flusher).Flush() // force chunked: no Content-Length
			fmt.Fprint(w, strings.Repeat("v", 500))
		case r.URL.Query().Get("size") == "fullsize":
			w.Header().Set("Content-Type", "image/jpeg")
			w.Header().Set("Content-Length", "1000")
			fmt.Fprint(w, strings.Repeat("x", 1000))
		}
	}))
	ctx := context.Background()
	b, err := c.Download(ctx, "id", RenditionPreview, 0)
	if err != nil || string(b.Data) != "PREVIEW" || b.ContentType != "image/jpeg" {
		t.Errorf("preview: %+v %v", b, err)
	}
	b, err = c.Download(ctx, "id", RenditionOriginal, 0)
	if err != nil || string(b.Data) != "ORIG" || b.ContentType != "image/heic" {
		t.Errorf("original: %+v %v", b, err)
	}
	// Content-Length over the limit is rejected before reading.
	if _, err := c.Download(ctx, "id", RenditionFullsize, 100); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversized (content-length): %v", err)
	}
	// Chunked body over the limit is rejected while reading.
	if _, err := c.Download(ctx, "id", RenditionPlayback, 10); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversized (streamed): %v", err)
	}
	b, err = c.Download(ctx, "id", RenditionPlayback, 0)
	if err != nil || b.ContentType != "video/mp4" {
		t.Errorf("playback: %+v %v", b, err)
	}
}

func idsOf(as []Asset) string {
	ids := make([]string, len(as))
	for i, a := range as {
		ids[i] = a.ID
	}
	return strings.Join(ids, ",")
}
