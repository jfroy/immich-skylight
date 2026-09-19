// Package migrations embeds the SQL schema migrations applied by
// internal/state at startup via golang-migrate.
//
// Files live in sqlite/ and follow golang-migrate's naming convention:
//
//	NNNNNN_description.up.sql
//	NNNNNN_description.down.sql
//
// Create a new pair with `make migration NAME=description`.
package migrations

import "embed"

// SQLite holds the sqlite/ migration files.
//
//go:embed sqlite/*.sql
var SQLite embed.FS
