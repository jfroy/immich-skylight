// Package skylight is a minimal client for the private Skylight app API,
// covering login, frame discovery and photo upload/delete.
package skylight

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// apiBase is a var so tests can point at a fake server.
var apiBase = "https://app.ourskylight.com/api"

const (
	apiVersion = "2026-03-01"
	apiUA      = "SkylightMobile (web)"
)

// TokenStore lets the client persist rotated tokens.
type TokenStore interface {
	// Tokens returns the currently stored tokens (may be empty).
	Tokens() (access, refresh string, expiry time.Time)
	// SaveTokens persists a new token set.
	SaveTokens(access, refresh string, expiry time.Time) error
}

// Client is an authenticated Skylight API client that transparently refreshes
// or re-logs-in when tokens expire.
type Client struct {
	email, password string
	fingerprint     string
	store           TokenStore
	http            *http.Client

	mu sync.Mutex
}

// NewClient builds a client. Tokens are read from and written to store.
func NewClient(email, password, fingerprint string, store TokenStore) *Client {
	return &Client{
		email:       email,
		password:    password,
		fingerprint: fingerprint,
		store:       store,
		http:        &http.Client{Timeout: 5 * time.Minute},
	}
}

// SetBaseURL points the client at a different host (testing). base is the
// scheme+host, e.g. "http://127.0.0.1:1234".
func SetBaseURL(base string) {
	base = strings.TrimRight(base, "/")
	authBase = base
	apiBase = base + "/api"
}

// ErrUnauthorized signals a 401 from the API.
var ErrUnauthorized = errors.New("skylight: unauthorized")

// Frame is a Skylight frame the user has access to.
type Frame struct {
	ID   string
	Name string
}

// UploadResult is returned after a successful upload.
type UploadResult struct {
	MessageIDs []int    `json:"message_ids"`
	FrameNames []string `json:"frame_names"`
	Key        string   `json:"key"`
}

// Authenticate makes sure a valid access token is available, logging in with
// credentials if necessary.
func (c *Client) Authenticate(ctx context.Context) error {
	_, err := c.accessToken(ctx, false)
	return err
}

// Frames lists all frames.
func (c *Client) Frames(ctx context.Context) ([]Frame, error) {
	var resp struct {
		Data []struct {
			ID         string `json:"id"`
			Attributes struct {
				Name string `json:"name"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/frames", nil, &resp); err != nil {
		return nil, err
	}
	out := make([]Frame, 0, len(resp.Data))
	for _, d := range resp.Data {
		out = append(out, Frame{ID: d.ID, Name: d.Attributes.Name})
	}
	return out, nil
}

// SupportedExt maps a MIME type to the extension Skylight expects, or "" if
// the type cannot be uploaded.
func SupportedExt(mime string) string {
	switch strings.ToLower(mime) {
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

// Upload sends media to one or more frames. ext must come from SupportedExt.
func (c *Client) Upload(ctx context.Context, frameIDs []string, ext, mime string, data []byte, caption string) (*UploadResult, error) {
	var resp struct {
		Data struct {
			URL string `json:"url"`
			UploadResult
		} `json:"data"`
	}
	body := map[string]any{"ext": ext, "frame_ids": frameIDs}
	if caption != "" {
		body["caption"] = caption
	}
	if err := c.do(ctx, http.MethodPost, "/upload_url", body, &resp); err != nil {
		return nil, fmt.Errorf("requesting upload url: %w", err)
	}
	if resp.Data.URL == "" {
		return nil, errors.New("skylight returned an empty upload url")
	}

	put, err := http.NewRequestWithContext(ctx, http.MethodPut, resp.Data.URL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	put.Header.Set("Content-Type", mime)
	put.ContentLength = int64(len(data))
	r, err := c.http.Do(put)
	if err != nil {
		return nil, fmt.Errorf("uploading to storage: %w", err)
	}
	defer r.Body.Close()
	io.Copy(io.Discard, r.Body) //nolint:errcheck
	if r.StatusCode != http.StatusOK && r.StatusCode != http.StatusNoContent {
		return nil, fmt.Errorf("storage upload failed with status %d", r.StatusCode)
	}
	res := resp.Data.UploadResult
	return &res, nil
}

// DeleteMessages removes photos from a frame by message ID.
func (c *Client) DeleteMessages(ctx context.Context, frameID string, messageIDs []int) error {
	if len(messageIDs) == 0 {
		return nil
	}
	return c.do(ctx, http.MethodDelete, "/frames/"+frameID+"/messages/destroy_multiple",
		map[string]any{"message_ids": messageIDs}, nil)
}

// do performs an authenticated JSON request, retrying once after re-auth on 401.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var payload []byte
	if in != nil {
		var err error
		if payload, err = json.Marshal(in); err != nil {
			return err
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := c.accessToken(ctx, attempt > 0)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, method, apiBase+path, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", apiUA)
		req.Header.Set("Skylight-Api-Version", apiVersion)
		if in != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			if attempt == 0 {
				continue
			}
			return fmt.Errorf("%w: %s", ErrUnauthorized, strings.TrimSpace(string(body)))
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return fmt.Errorf("skylight %s %s: http %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(body)))
		}
		if out == nil || len(body) == 0 {
			return nil
		}
		return json.Unmarshal(body, out)
	}
	return ErrUnauthorized
}

// accessToken returns a valid access token. With force=true the cached token
// is discarded and a refresh/login is performed.
func (c *Client) accessToken(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	access, refresh, expiry := c.store.Tokens()
	now := time.Now()
	if !force && access != "" && now.Before(expiry) {
		return access, nil
	}

	var tr *TokenResponse
	var err error
	if refresh != "" {
		tr, err = Refresh(ctx, refresh, c.fingerprint)
		if err != nil {
			err = fmt.Errorf("refresh failed (%v); falling back to password login", err)
		}
	}
	if tr == nil {
		var lerr error
		tr, lerr = Login(ctx, c.email, c.password, c.fingerprint)
		if lerr != nil {
			if err != nil {
				return "", fmt.Errorf("%v: %w", err, lerr)
			}
			return "", lerr
		}
	}
	if serr := c.store.SaveTokens(tr.AccessToken, tr.RefreshToken, tr.Expiry(now)); serr != nil {
		return "", fmt.Errorf("saving tokens: %w", serr)
	}
	return tr.AccessToken, nil
}
