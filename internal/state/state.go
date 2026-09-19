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
	// Messages maps Skylight frame ID -> message IDs created on that frame.
	Messages map[string][]int `json:"messages"`
	Checksum string           `json:"checksum,omitempty"`
	SentAt   time.Time        `json:"sent_at"`
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
	mu   sync.RWMutex
}

// Load reads the state file, creating a fresh state if it does not exist.
func Load(path string) (*State, error) {
	s := &State{Version: 2, Sent: map[string]Sent{}, path: path}
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
	s.mu.RLock()
	b, err := json.MarshalIndent(s, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Has reports whether an asset has been sent.
func (s *State) Has(assetID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.Sent[assetID]
	return ok
}

// Snapshot returns a copy of the sent map.
func (s *State) Snapshot() map[string]Sent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]Sent, len(s.Sent))
	for k, v := range s.Sent {
		out[k] = v
	}
	return out
}

// Count returns the number of tracked assets.
func (s *State) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.Sent)
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

// GetTokens returns the stored Skylight tokens.
func (s *State) GetTokens() Tokens {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Tokens
}

// newUUID returns a random RFC 4122 v4 UUID string.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
