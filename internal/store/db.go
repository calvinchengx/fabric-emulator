// Package store is the emulator's persistence layer: pure-Go SQLite
// (modernc.org/sqlite, no CGO), one database for workspaces, items,
// definitions, role assignments, and operations. All timestamps flow through
// Now (the controllable clock) so LRO completion is deterministic.
package store

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	"path/filepath"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
	_ "modernc.org/sqlite"
)

// Store wraps the database plus the emulator clock.
type Store struct {
	db    *sql.DB
	Clock *clock.Clock
}

// Open opens (creating if needed) the database in dataDir; an empty dataDir
// uses an in-memory database (tests, ephemeral runs).
func Open(dataDir string, ck *clock.Clock) (*Store, error) {
	dsn := ":memory:"
	if dataDir != "" {
		dsn = filepath.Join(dataDir, "fabric-emulator.db")
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// modernc/sqlite serializes writes; a single conn avoids table locks.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, Clock: ck}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Now returns the current emulator time (epoch seconds).
func (s *Store) Now() int64 { return s.Clock.Now() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS workspaces (
	id TEXT PRIMARY KEY,
	display_name TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	capacity_id TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS role_assignments (
	id TEXT PRIMARY KEY,
	workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	principal_id TEXT NOT NULL,
	principal_type TEXT NOT NULL,
	role TEXT NOT NULL,
	UNIQUE (workspace_id, principal_id)
);
CREATE TABLE IF NOT EXISTS items (
	id TEXT PRIMARY KEY,
	workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	type TEXT NOT NULL,
	display_name TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS item_definitions (
	item_id TEXT PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE,
	parts_json TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS operations (
	id TEXT PRIMARY KEY,
	kind TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	complete_at INTEGER NOT NULL,
	result_ref TEXT NOT NULL DEFAULT '',
	fail_with TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS connections (
	id TEXT PRIMARY KEY,
	display_name TEXT NOT NULL,
	connectivity_type TEXT NOT NULL DEFAULT '',
	details_json TEXT NOT NULL DEFAULT '{}',
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS git_connections (
	workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
	provider_json TEXT NOT NULL,
	remote_key TEXT NOT NULL,
	branch TEXT NOT NULL,
	cred_source TEXT NOT NULL,
	connection_id TEXT NOT NULL DEFAULT '',
	initialized INTEGER NOT NULL DEFAULT 0,
	synced_commit TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS git_remote_items (
	remote_key TEXT NOT NULL,
	branch TEXT NOT NULL,
	logical_id TEXT NOT NULL,
	item_type TEXT NOT NULL,
	display_name TEXT NOT NULL,
	parts_json TEXT NOT NULL,
	PRIMARY KEY (remote_key, branch, item_type, display_name)
);
CREATE TABLE IF NOT EXISTS git_remote_heads (
	remote_key TEXT NOT NULL,
	branch TEXT NOT NULL,
	commit_hash TEXT NOT NULL,
	PRIMARY KEY (remote_key, branch)
);
CREATE TABLE IF NOT EXISTS onelake_paths (
	workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	item_id TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	rel_path TEXT NOT NULL, -- path within the item, e.g. Files/raw/a.txt
	is_dir INTEGER NOT NULL DEFAULT 0,
	content BLOB NOT NULL DEFAULT x'',
	created_at INTEGER NOT NULL,
	PRIMARY KEY (item_id, rel_path)
);
CREATE TABLE IF NOT EXISTS workspace_identities (
	workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
	identity_id TEXT NOT NULL, -- entra service principal object id
	app_id TEXT NOT NULL,      -- the sub/appid in tokens the identity mints
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS folders (
	id TEXT PRIMARY KEY,
	workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	display_name TEXT NOT NULL,
	parent_id TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	UNIQUE (workspace_id, parent_id, display_name)
);
CREATE TABLE IF NOT EXISTS job_instances (
	id TEXT PRIMARY KEY,
	item_id TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	job_type TEXT NOT NULL,
	invoke_type TEXT NOT NULL DEFAULT 'Manual',
	created_at INTEGER NOT NULL,
	complete_at INTEGER NOT NULL,
	cancelled INTEGER NOT NULL DEFAULT 0,
	fail_with TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS capacities (
	id TEXT PRIMARY KEY,
	display_name TEXT NOT NULL,
	sku TEXT NOT NULL,
	region TEXT NOT NULL,
	state TEXT NOT NULL
);
PRAGMA foreign_keys = ON;
`)
	if err != nil {
		return err
	}
	return s.seedCapacity()
}

// NewID returns a random lowercase UUIDv4 — the id format Fabric uses for
// workspaces, items, and operations.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
