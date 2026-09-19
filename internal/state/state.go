// Package state persists sync progress and Skylight credentials to a JSON file.
package state

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Sent records an asset that has been uploaded to Skylight.
type Sent struct {
	MessageIDs []int     `json:"message_ids"`
	FrameIDs   []string  `json:"frame_ids"`
	Checksum   string    `json:"checksum,omitempty"`
	SentAt     time.Time `json:"sent_at"`
}

// Tokens holds the Skylight OAuth credentials.
type Tokens struct {
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expiry,omitempty"`
}

// State is the on-disk document.
type State struct {
	Version     int             `json:"version"`
	Fingerprint string          `json:"device_fingerprint"`
	Tokens      Tokens          `json:"skylight_tokens"`
	Sent        map[string]Sent `json:"sent"` // Immich asset ID -> upload record

	path string
	mu   sync.Mutex
}

// Load reads the state file, creating a fresh state if it does not exist.
func Load(path string) (*State, error) {
	s := &State{Version: 1, Sent: map[string]Sent{}, path: path}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("reading state %s: %w", path, err)
	default:
		if err := json.Unmarshal(b, s); err != nil {
			return nil, fmt.Errorf("parsing state %s: %w", path, err)
		}
		if s.Sent == nil {
			s.Sent = map[string]Sent{}
		}
	}
	if s.Fingerprint == "" {
		s.Fingerprint = newUUID()
	}
	return s, nil
}

// Save atomically writes the state to disk.
func (s *State) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// MarkSent records an uploaded asset.
func (s *State) MarkSent(assetID string, rec Sent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Sent[assetID] = rec
}

// Forget removes an asset record.
func (s *State) Forget(assetID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Sent, assetID)
}

// SetTokens replaces the stored Skylight tokens.
func (s *State) SetTokens(t Tokens) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Tokens = t
}

// newUUID returns a random RFC 4122 v4 UUID string.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
