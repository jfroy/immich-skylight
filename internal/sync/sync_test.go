package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/jfroy/immich-skylight/internal/config"
	"github.com/jfroy/immich-skylight/internal/metrics"
	"github.com/jfroy/immich-skylight/internal/skylight"
	"github.com/jfroy/immich-skylight/internal/state"
)

// fakeImmich serves the handful of endpoints the syncer uses.
type fakeImmich struct {
	mu        sync.Mutex
	favorites []map[string]any
	tagged    []map[string]any
	downloads int
}

func asset(id, mime, typ string) map[string]any {
	return map[string]any{
		"id": id, "type": typ, "originalMimeType": mime, "originalFileName": id + ".x",
		"checksum": "sum-" + id, "isFavorite": true, "fileCreatedAt": time.Now(),
		"exifInfo": map[string]any{"description": "caption " + id},
	}
}

func (f *fakeImmich) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/users/me", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "key" {
			http.Error(w, "nope", 401)
			return
		}
		fmt.Fprint(w, `{"email":"me@x"}`)
	})
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"id":"tag-1","name":"Skylight","value":"Family/Skylight"}]`)
	})
	mux.HandleFunc("/api/search/metadata", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		items := f.favorites
		if _, ok := body["tagIds"]; ok {
			items = f.tagged
		}
		f.mu.Unlock()
		if body["page"].(float64) > 1 {
			items = nil
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"assets": map[string]any{"items": items, "nextPage": nil}})
	})
	mux.HandleFunc("/api/assets/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.downloads++
		f.mu.Unlock()
		parts := strings.Split(r.URL.Path, "/")
		id := parts[3]
		switch {
		case strings.HasSuffix(r.URL.Path, "/original"):
			if id == "heic" {
				w.Header().Set("Content-Type", "image/heic")
			} else {
				w.Header().Set("Content-Type", "image/jpeg")
			}
			fmt.Fprint(w, "ORIGINAL-"+id)
		default:
			if r.URL.Query().Get("size") == "fullsize" && id == "nofull" {
				http.Error(w, "not generated", 404)
				return
			}
			w.Header().Set("Content-Type", "image/jpeg")
			fmt.Fprintf(w, "%s-%s", strings.ToUpper(r.URL.Query().Get("size")), id)
		}
	})
	return mux
}

// fakeSkylight emulates login, frames, presigned upload and delete.
type fakeSkylight struct {
	mu       sync.Mutex
	logins   int
	refresh  int
	uploads  map[int]string // message id -> body
	deleted  []int
	captions []string
	nextID   int
	expireIn int
	revoked  string // bearer token that should be rejected
}

func (f *fakeSkylight) handler(t *testing.T, self func() string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/session/new", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "skylightcloud_session", Value: "anon", Path: "/"})
		fmt.Fprint(w, `<html><meta name="csrf-token" content="CSRF123"><form><input name="authenticity_token" value="CSRF123"></form></html>`)
	})
	mux.HandleFunc("/auth/session", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("authenticity_token") != "CSRF123" || r.Form.Get("password") != "pw" {
			http.Error(w, "bad", 422)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "skylightcloud_session", Value: "authed", Path: "/"})
		w.Header().Set("Location", "/oauth/authorize")
		w.WriteHeader(302)
	})
	mux.HandleFunc("/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("skylightcloud_session")
		if err != nil || c.Value != "authed" || r.URL.Query().Get("client_id") != "skylight-mobile" {
			http.Error(w, "not logged in", 401)
			return
		}
		w.Header().Set("Location", "https://ourskylight.com/welcome?code=CODE1")
		w.WriteHeader(302)
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			if r.Form.Get("code") != "CODE1" {
				http.Error(w, "bad code", 400)
				return
			}
			f.logins++
		case "refresh_token":
			if r.Form.Get("refresh_token") != "R1" {
				http.Error(w, "bad refresh", 400)
				return
			}
			f.refresh++
		}
		exp := f.expireIn
		if exp == 0 {
			exp = 3600
		}
		fmt.Fprintf(w, `{"access_token":"A%d","refresh_token":"R1","expires_in":%d,"token_type":"Bearer"}`, f.logins+f.refresh, exp)
	})
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		f.mu.Lock()
		revoked := f.revoked
		f.mu.Unlock()
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !strings.HasPrefix(tok, "A") || (revoked != "" && tok == revoked) {
			http.Error(w, `{"errors":["Invalid token"]}`, 401)
			return false
		}
		return true
	}
	mux.HandleFunc("/api/frames", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"111","attributes":{"name":"Kitchen"}},{"id":"222","attributes":{"name":"Office"}}]}`)
	})
	mux.HandleFunc("/api/upload_url", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		var body struct {
			Ext      string   `json:"ext"`
			FrameIDs []string `json:"frame_ids"`
			Caption  string   `json:"caption"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Ext != "jpg" || len(body.FrameIDs) != 1 || body.FrameIDs[0] != "111" {
			t.Errorf("bad upload_url body: %+v", body)
		}
		f.mu.Lock()
		f.nextID++
		id := f.nextID
		f.captions = append(f.captions, body.Caption)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"url": self() + "/s3/" + fmt.Sprint(id), "message_ids": []int{id}, "frame_names": []string{"Kitchen"},
		}})
	})
	mux.HandleFunc("/s3/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "method", 405)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var id int
		fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/s3/"), "%d", &id)
		f.mu.Lock()
		if f.uploads == nil {
			f.uploads = map[int]string{}
		}
		f.uploads[id] = string(b)
		f.mu.Unlock()
		w.WriteHeader(200)
	})
	mux.HandleFunc("/api/frames/111/messages/destroy_multiple", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) || r.Method != http.MethodDelete {
			return
		}
		var body struct {
			IDs []int `json:"message_ids"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.deleted = append(f.deleted, body.IDs...)
		f.mu.Unlock()
		w.WriteHeader(204)
	})
	return mux
}

func newSyncer(t *testing.T, cfg *config.Config, st *state.State) (*Syncer, error) {
	t.Helper()
	reg := prometheus.NewRegistry()
	rec, err := metrics.New(reg)
	if err != nil {
		t.Fatal(err)
	}
	return New(context.Background(), Options{
		Config: cfg, State: st, Logger: slog.Default(), Metrics: rec,
		SkylightBaseURL: skyBase + "/api",
	})
}

var skyBase string

func setup(t *testing.T, cfgMut func(*config.Config)) (*fakeImmich, *fakeSkylight, *config.Config, *state.State) {
	t.Helper()
	fi := &fakeImmich{}
	imSrv := httptest.NewServer(fi.handler(t))
	t.Cleanup(imSrv.Close)

	fs := &fakeSkylight{}
	var skySrv *httptest.Server
	skySrv = httptest.NewServer(fs.handler(t, func() string { return skySrv.URL }))
	t.Cleanup(skySrv.Close)
	skylight.SetAuthBaseURL(skySrv.URL)
	skyBase = skySrv.URL

	cfg := &config.Config{
		ImmichURL: imSrv.URL, ImmichAPIKey: "key", Favorites: true,
		ImageSource: config.SourcePreview, SkylightEmail: "e", SkylightPassword: "pw",
		FrameNames: []string{"kitchen"}, UseCaption: true, Interval: time.Hour,
		StateFile: filepath.Join(t.TempDir(), "state.db"),
	}
	if cfgMut != nil {
		cfgMut(cfg)
	}
	st, err := state.Open(context.Background(), cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return fi, fs, cfg, st
}

func TestEndToEnd(t *testing.T) {
	fi, fs, cfg, st := setup(t, func(c *config.Config) {
		c.Tags = []string{"Family/Skylight"}
		c.RemoveUnselected = true
	})
	fi.favorites = []map[string]any{asset("a", "image/jpeg", "IMAGE"), asset("vid", "video/mp4", "VIDEO")}
	fi.tagged = []map[string]any{asset("a", "image/jpeg", "IMAGE"), asset("b", "image/heic", "IMAGE")}

	ctx := context.Background()
	s, err := newSyncer(t, cfg, st)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := s.frames; len(got) != 1 || got[0] != "111" {
		t.Fatalf("frame resolution = %v", got)
	}
	if err := s.Once(ctx); err != nil {
		t.Fatalf("Once: %v", err)
	}

	// a and b uploaded (video excluded); de-duplicated across favorites/tags.
	if len(fs.uploads) != 2 {
		t.Fatalf("uploads = %v", fs.uploads)
	}
	for _, body := range fs.uploads {
		if !strings.HasPrefix(body, "PREVIEW-") {
			t.Errorf("expected preview rendition, got %q", body)
		}
	}
	if len(fs.captions) != 2 || !strings.HasPrefix(fs.captions[0], "caption ") {
		t.Errorf("captions = %v", fs.captions)
	}
	if n, _ := st.Count(ctx); n != 2 {
		t.Fatalf("tracked = %d", n)
	}
	if fs.logins != 1 {
		t.Errorf("logins = %d, want 1", fs.logins)
	}

	// Second pass: nothing new, nothing re-sent.
	before := fi.downloads
	if err := s.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if fi.downloads != before || len(fs.uploads) != 2 {
		t.Errorf("second pass re-downloaded/re-uploaded")
	}

	// Un-favorite "a" -> it should be deleted from the frame and forgotten.
	fi.mu.Lock()
	fi.favorites = nil
	fi.tagged = []map[string]any{asset("b", "image/heic", "IMAGE")}
	fi.mu.Unlock()
	if err := s.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.Count(ctx); len(fs.deleted) != 1 || n != 1 {
		t.Errorf("deleted=%v tracked=%d", fs.deleted, n)
	}
	if _, ok, _ := st.Get(ctx, "b"); !ok {
		t.Errorf("b should remain in state")
	}

	// State persisted and reloadable with tokens.
	st.Close()
	st2, err := state.Open(ctx, cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	tk, _ := st2.GetTokens(ctx)
	n2, _ := st2.Count(ctx)
	if tk.RefreshToken != "R1" || n2 != 1 || st2.Fingerprint != st.Fingerprint {
		t.Errorf("persisted state mismatch: tokens=%+v n=%d fp=%s/%s", tk, n2, st2.Fingerprint, st.Fingerprint)
	}
	if rec, _, _ := st2.Get(ctx, "b"); len(rec.Messages["111"]) != 1 {
		t.Errorf("persisted messages mismatch: %+v", rec)
	}
}

func TestRenditionFallback(t *testing.T) {
	fi, fs, cfg, st := setup(t, func(c *config.Config) { c.ImageSource = config.SourceOriginal })
	fi.favorites = []map[string]any{asset("jpg", "image/jpeg", "IMAGE"), asset("heic", "image/heic", "IMAGE"), asset("nofull", "image/x-canon-cr2", "IMAGE")}

	s, err := newSyncer(t, cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	var bodies []string
	for _, b := range fs.uploads {
		bodies = append(bodies, b)
	}
	joined := strings.Join(bodies, ",")
	for _, want := range []string{"ORIGINAL-jpg", "FULLSIZE-heic", "PREVIEW-nofull"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %s in %v", want, bodies)
		}
	}
}

func TestTokenRefreshAndReauth(t *testing.T) {
	fi, fs, cfg, st := setup(t, nil)
	fi.favorites = []map[string]any{asset("a", "image/jpeg", "IMAGE")}
	fs.expireIn = 30 // < 1 minute safety margin -> immediately considered expired on next use

	ctx := context.Background()
	s, err := newSyncer(t, cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if fs.logins != 1 || fs.refresh == 0 {
		t.Errorf("expected password login once then refreshes; logins=%d refresh=%d", fs.logins, fs.refresh)
	}

	// Corrupt the refresh token and expire the session: next call must fall
	// back to password login.
	if err := st.SetTokens(ctx, state.Tokens{AccessToken: "", RefreshToken: "BAD"}); err != nil {
		t.Fatal(err)
	}
	s.sky.ExpireForTest()
	fi.favorites = append(fi.favorites, asset("b", "image/jpeg", "IMAGE"))
	if err := s.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if fs.logins != 2 {
		t.Errorf("expected re-login after bad refresh token; logins=%d", fs.logins)
	}
	if len(fs.uploads) != 2 {
		t.Errorf("uploads=%d", len(fs.uploads))
	}
}

func TestSelectFrames(t *testing.T) {
	all := []skylight.Frame{{ID: "1", Name: "A"}, {ID: "2", Name: "B"}}
	if _, err := selectFrames(all, nil, nil); err == nil {
		t.Error("ambiguous frames should error")
	}
	if got, _ := selectFrames(all[:1], nil, nil); len(got) != 1 || got[0] != "1" {
		t.Errorf("single frame auto-select = %v", got)
	}
	if got, _ := selectFrames(all, []string{"2"}, []string{"a"}); len(got) != 2 {
		t.Errorf("ids+names = %v", got)
	}
	if _, err := selectFrames(all, nil, []string{"Nope"}); err == nil {
		t.Error("unknown name should error")
	}
}

func TestReauthOn401(t *testing.T) {
	fi, fs, cfg, st := setup(t, nil)
	fi.favorites = []map[string]any{asset("a", "image/jpeg", "IMAGE")}
	ctx := context.Background()
	s, err := newSyncer(t, cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	// Revoke the token the client currently holds (issued at login: "A1").
	fs.mu.Lock()
	fs.revoked = "A1"
	fs.mu.Unlock()
	if err := s.Once(ctx); err != nil {
		t.Fatalf("Once after revocation: %v", err)
	}
	if len(fs.uploads) != 1 {
		t.Errorf("expected upload to succeed after re-auth; uploads=%d", len(fs.uploads))
	}
	if fs.refresh == 0 {
		t.Errorf("expected a token refresh after 401")
	}
}
