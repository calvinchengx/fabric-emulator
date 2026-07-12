package store

import (
	"database/sql"
	"errors"
	"strings"
)

// OneLakePath is a file or directory within an item's OneLake storage.
type OneLakePath struct {
	WorkspaceID string
	ItemID      string
	RelPath     string // within the item, e.g. Files/raw/a.txt
	IsDir       bool
	Content     []byte
	CreatedAt   int64
	ETag        string
	ModifiedAt  int64
}

// ErrPathExists is returned by conditional creates (If-None-Match: *) when
// the path already exists — the put-if-absent primitive Delta's commit
// protocol relies on for _delta_log atomicity.
var ErrPathExists = errors.New("path already exists")

// newETag stamps a fresh opaque ETag.
func newETag() string { return `"` + NewID() + `"` }

// CreateOneLakePath creates or overwrites a file/directory. When ifNoneMatch
// is true the create is conditional: an existing path is ErrPathExists and
// nothing is written. Parent directories are implicit, like ADLS.
func (s *Store) CreateOneLakePath(p *OneLakePath, ifNoneMatch bool) error {
	p.CreatedAt = s.Now()
	p.ModifiedAt = p.CreatedAt
	p.ETag = newETag()
	if ifNoneMatch {
		res, err := s.db.Exec(`
INSERT INTO onelake_paths (workspace_id, item_id, rel_path, is_dir, content, created_at, etag, modified_at)
VALUES (?,?,?,?,?,?,?,?)
ON CONFLICT(item_id, rel_path) DO NOTHING`,
			p.WorkspaceID, p.ItemID, p.RelPath, p.IsDir, p.Content, p.CreatedAt, p.ETag, p.ModifiedAt)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrPathExists
		}
		return nil
	}
	_, err := s.db.Exec(`
INSERT INTO onelake_paths (workspace_id, item_id, rel_path, is_dir, content, created_at, etag, modified_at)
VALUES (?,?,?,?,?,?,?,?)
ON CONFLICT(item_id, rel_path) DO UPDATE SET
	is_dir = excluded.is_dir, content = excluded.content,
	etag = excluded.etag, modified_at = excluded.modified_at`,
		p.WorkspaceID, p.ItemID, p.RelPath, p.IsDir, p.Content, p.CreatedAt, p.ETag, p.ModifiedAt)
	return err
}

// GetOneLakePath fetches one path.
func (s *Store) GetOneLakePath(itemID, relPath string) (*OneLakePath, error) {
	p := &OneLakePath{}
	err := s.db.QueryRow(`
SELECT workspace_id, item_id, rel_path, is_dir, content, created_at, etag, modified_at
FROM onelake_paths WHERE item_id = ? AND rel_path = ?`, itemID, relPath).
		Scan(&p.WorkspaceID, &p.ItemID, &p.RelPath, &p.IsDir, &p.Content, &p.CreatedAt, &p.ETag, &p.ModifiedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// AppendOneLakePath appends data at position (must equal the current length,
// as in ADLS). Returns the new length.
func (s *Store) AppendOneLakePath(itemID, relPath string, position int64, data []byte) (int64, error) {
	p, err := s.GetOneLakePath(itemID, relPath)
	if err != nil {
		return 0, err
	}
	if p.IsDir {
		return 0, errors.New("cannot append to a directory")
	}
	if position != int64(len(p.Content)) {
		return 0, errors.New("invalid append position")
	}
	content := append(p.Content, data...)
	_, err = s.db.Exec(`UPDATE onelake_paths SET content = ?, etag = ?, modified_at = ? WHERE item_id = ? AND rel_path = ?`,
		content, newETag(), s.Now(), itemID, relPath)
	return int64(len(content)), err
}

// RenameOneLakePath moves a file, or a directory and its subtree, within an
// item (the DFS x-ms-rename-source operation; Hadoop committers depend on
// it). Destination paths are overwritten.
func (s *Store) RenameOneLakePath(itemID, src, dst string) error {
	// The source may be an explicit path or an implicit prefix (a directory
	// with no row of its own); either must cover at least one stored path.
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM onelake_paths WHERE item_id = ? AND (rel_path = ? OR rel_path LIKE ? || '/%')`,
		itemID, src, src).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now, etag := s.Now(), newETag()
	// Overwrite any existing destination subtree, then move.
	if _, err := tx.Exec(`
DELETE FROM onelake_paths WHERE item_id = ? AND (rel_path = ? OR rel_path LIKE ? || '/%')`,
		itemID, dst, dst); err != nil {
		return err
	}
	if _, err := tx.Exec(`
UPDATE onelake_paths SET rel_path = ? || substr(rel_path, ?), etag = ?, modified_at = ?
WHERE item_id = ? AND (rel_path = ? OR rel_path LIKE ? || '/%')`,
		dst, len(src)+1, etag, now, itemID, src, src); err != nil {
		return err
	}
	return tx.Commit()
}

// ListOneLakePaths returns paths under prefix ("" = whole item). Non-recursive
// listings collapse deeper entries to their first-level directory.
func (s *Store) ListOneLakePaths(itemID, prefix string, recursive bool) ([]*OneLakePath, error) {
	rows, err := s.db.Query(`
SELECT workspace_id, item_id, rel_path, is_dir, content, created_at, etag, modified_at
FROM onelake_paths WHERE item_id = ? ORDER BY rel_path`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	prefix = strings.TrimSuffix(prefix, "/")
	var out []*OneLakePath
	seenDirs := map[string]bool{}
	for rows.Next() {
		p := &OneLakePath{}
		if err := rows.Scan(&p.WorkspaceID, &p.ItemID, &p.RelPath, &p.IsDir, &p.Content, &p.CreatedAt, &p.ETag, &p.ModifiedAt); err != nil {
			return nil, err
		}
		rel := p.RelPath
		if prefix != "" {
			if rel == prefix || !strings.HasPrefix(rel, prefix+"/") {
				continue
			}
			rel = strings.TrimPrefix(rel, prefix+"/")
		}
		if !recursive {
			if i := strings.IndexByte(rel, '/'); i >= 0 {
				// Collapse to the first-level directory.
				dir := p.RelPath[:len(p.RelPath)-len(rel)+i]
				if seenDirs[dir] {
					continue
				}
				seenDirs[dir] = true
				out = append(out, &OneLakePath{WorkspaceID: p.WorkspaceID, ItemID: p.ItemID, RelPath: dir, IsDir: true})
				continue
			}
			if p.IsDir {
				// An explicit directory row merges with children that
				// collapse to the same first-level directory.
				if seenDirs[p.RelPath] {
					continue
				}
				seenDirs[p.RelPath] = true
			}
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeleteOneLakePath removes a file, or a directory and everything under it.
func (s *Store) DeleteOneLakePath(itemID, relPath string) error {
	res, err := s.db.Exec(`
DELETE FROM onelake_paths WHERE item_id = ? AND (rel_path = ? OR rel_path LIKE ? || '/%')`,
		itemID, relPath, relPath)
	if err != nil {
		return err
	}
	return oneRow(res)
}

// GetWorkspaceByName resolves a workspace by display name (OneLake's
// name-addressing; GUIDs resolve via GetWorkspace).
func (s *Store) GetWorkspaceByName(name string) (*Workspace, error) {
	w := &Workspace{Type: "Workspace"}
	err := s.db.QueryRow(
		`SELECT id, display_name, description, capacity_id, created_at FROM workspaces WHERE display_name = ?`, name).
		Scan(&w.ID, &w.DisplayName, &w.Description, &w.CapacityID, &w.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return w, err
}

// GetItemByName resolves an item by display name + type (the name.Type
// OneLake addressing).
func (s *Store) GetItemByName(workspaceID, displayName, itemType string) (*Item, error) {
	it := &Item{}
	err := s.db.QueryRow(`
SELECT id, workspace_id, type, display_name, description, created_at
FROM items WHERE workspace_id = ? AND display_name = ? AND type = ? COLLATE NOCASE`,
		workspaceID, displayName, itemType).
		Scan(&it.ID, &it.WorkspaceID, &it.Type, &it.DisplayName, &it.Description, &it.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return it, err
}
