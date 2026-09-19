// Package state persists sync progress and Skylight credentials in a SQLite
// database (pure-Go driver, no cgo). Every mutation is committed immediately,
// so a crash mid-pass never loses or duplicates an upload record.
//
// The schema is managed by golang-migrate from the SQL files embedded in the
// top-level migrations package; Open applies pending migrations.
package state

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migsqlite "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "modernc.org/sqlite"

	"github.com/jfroy/immich-skylight/migrations"
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
	AccessToken  string
	RefreshToken string
	Expiry       time.Time
}

// State is a handle to the database.
type State struct {
	// Fingerprint is the stable per-installation device UUID.
	Fingerprint string
	// SchemaVersion is the migration version in effect after Open.
	SchemaVersion uint

	db *sql.DB
}

// Open opens (creating if needed) the database at path and applies any
// pending schema migrations.
func Open(ctx context.Context, path string) (*State, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// Pragmas are connection-scoped in modernc.org/sqlite, so set them on the
	// DSN rather than in a migration. journal_mode is also persisted in the file.
	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening state db: %w", err)
	}
	// SQLite has a single writer; one connection keeps behavior predictable.
	db.SetMaxOpenConns(1)

	version, err := Migrate(db)
	if err != nil {
		db.Close()
		return nil, err
	}
	s := &State{db: db, SchemaVersion: version}

	if s.Fingerprint, err = s.meta(ctx, "device_fingerprint"); err != nil {
		db.Close()
		return nil, err
	}
	if s.Fingerprint == "" {
		s.Fingerprint = newUUID()
		if err := s.setMeta(ctx, "device_fingerprint", s.Fingerprint); err != nil {
			db.Close()
			return nil, err
		}
	}
	return s, nil
}

// Migrate applies all pending up-migrations to db and returns the resulting
// schema version.
func Migrate(db *sql.DB) (uint, error) {
	src, err := iofs.New(migrations.SQLite, "sqlite")
	if err != nil {
		return 0, fmt.Errorf("loading migrations: %w", err)
	}
	drv, err := migsqlite.WithInstance(db, &migsqlite.Config{MigrationsTable: "schema_migrations"})
	if err != nil {
		return 0, fmt.Errorf("preparing migration driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "sqlite", drv)
	if err != nil {
		return 0, fmt.Errorf("preparing migrations: %w", err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return 0, fmt.Errorf("applying migrations: %w", err)
	}
	v, dirty, err := m.Version()
	if err != nil {
		return 0, fmt.Errorf("reading schema version: %w", err)
	}
	if dirty {
		return v, fmt.Errorf("schema is dirty at version %d; manual repair required", v)
	}
	return v, nil
}

// Close closes the database.
func (s *State) Close() error { return s.db.Close() }

// Has reports whether an asset has been sent.
func (s *State) Has(ctx context.Context, assetID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sent WHERE asset_id = ?`, assetID).Scan(&n)
	return n > 0, err
}

// Count returns the number of tracked assets.
func (s *State) Count(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sent`).Scan(&n)
	return n, err
}

// Get returns one record.
func (s *State) Get(ctx context.Context, assetID string) (Sent, bool, error) {
	all, err := s.Snapshot(ctx)
	if err != nil {
		return Sent{}, false, err
	}
	rec, ok := all[assetID]
	return rec, ok, nil
}

// Snapshot returns all sent records.
func (s *State) Snapshot(ctx context.Context) (map[string]Sent, error) {
	out := map[string]Sent{}
	rows, err := s.db.QueryContext(ctx, `SELECT asset_id, COALESCE(checksum,''), sent_at FROM sent`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, sum, at string
		if err := rows.Scan(&id, &sum, &at); err != nil {
			rows.Close()
			return nil, err
		}
		t, _ := time.Parse(time.RFC3339Nano, at)
		out[id] = Sent{Messages: map[string][]int{}, Checksum: sum, SentAt: t}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.db.QueryContext(ctx, `SELECT asset_id, frame_id, message_id FROM sent_message ORDER BY asset_id, frame_id, message_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, frame string
		var msg int
		if err := rows.Scan(&id, &frame, &msg); err != nil {
			return nil, err
		}
		if rec, ok := out[id]; ok {
			rec.Messages[frame] = append(rec.Messages[frame], msg)
		}
	}
	return out, rows.Err()
}

// MarkSent records an uploaded asset (replacing any existing record).
func (s *State) MarkSent(ctx context.Context, assetID string, rec Sent) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if rec.SentAt.IsZero() {
			rec.SentAt = time.Now()
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sent WHERE asset_id = ?`, assetID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sent(asset_id, checksum, sent_at) VALUES (?, ?, ?)`,
			assetID, rec.Checksum, rec.SentAt.UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
		for frame, ids := range rec.Messages {
			for _, id := range ids {
				if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO sent_message(asset_id, frame_id, message_id) VALUES (?, ?, ?)`,
					assetID, frame, id); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// Forget removes an asset record (and its messages).
func (s *State) Forget(ctx context.Context, assetID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sent WHERE asset_id = ?`, assetID)
	return err
}

// SetTokens replaces the stored Skylight tokens.
func (s *State) SetTokens(ctx context.Context, t Tokens) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		exp := ""
		if !t.Expiry.IsZero() {
			exp = t.Expiry.UTC().Format(time.RFC3339Nano)
		}
		for k, v := range map[string]string{
			"access_token": t.AccessToken, "refresh_token": t.RefreshToken, "token_expiry": exp,
		} {
			if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
				return err
			}
		}
		return nil
	})
}

// GetTokens returns the stored Skylight tokens.
func (s *State) GetTokens(ctx context.Context) (Tokens, error) {
	var t Tokens
	var err error
	if t.AccessToken, err = s.meta(ctx, "access_token"); err != nil {
		return t, err
	}
	if t.RefreshToken, err = s.meta(ctx, "refresh_token"); err != nil {
		return t, err
	}
	exp, err := s.meta(ctx, "token_expiry")
	if err != nil {
		return t, err
	}
	if exp != "" {
		t.Expiry, _ = time.Parse(time.RFC3339Nano, exp)
	}
	return t, nil
}

func (s *State) meta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *State) setMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (s *State) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// newUUID returns a random RFC 4122 v4 UUID string.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
