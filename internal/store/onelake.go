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
}

// CreateOneLakePath creates a file (empty) or directory. Parent directories
// are implicit, like ADLS.
func (s *Store) CreateOneLakePath(p *OneLakePath) error {
	p.CreatedAt = s.Now()
	_, err := s.db.Exec(`
INSERT INTO onelake_paths (workspace_id, item_id, rel_path, is_dir, content, created_at)
VALUES (?,?,?,?,?,?)
ON CONFLICT(item_id, rel_path) DO UPDATE SET is_dir = excluded.is_dir, content = excluded.content`,
		p.WorkspaceID, p.ItemID, p.RelPath, p.IsDir, p.Content, p.CreatedAt)
	return err
}

// GetOneLakePath fetches one path.
func (s *Store) GetOneLakePath(itemID, relPath string) (*OneLakePath, error) {
	p := &OneLakePath{}
	err := s.db.QueryRow(`
SELECT workspace_id, item_id, rel_path, is_dir, content, created_at
FROM onelake_paths WHERE item_id = ? AND rel_path = ?`, itemID, relPath).
		Scan(&p.WorkspaceID, &p.ItemID, &p.RelPath, &p.IsDir, &p.Content, &p.CreatedAt)
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
	_, err = s.db.Exec(`UPDATE onelake_paths SET content = ? WHERE item_id = ? AND rel_path = ?`,
		content, itemID, relPath)
	return int64(len(content)), err
}

// ListOneLakePaths returns paths under prefix ("" = whole item). Non-recursive
// listings collapse deeper entries to their first-level directory.
func (s *Store) ListOneLakePaths(itemID, prefix string, recursive bool) ([]*OneLakePath, error) {
	rows, err := s.db.Query(`
SELECT workspace_id, item_id, rel_path, is_dir, content, created_at
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
		if err := rows.Scan(&p.WorkspaceID, &p.ItemID, &p.RelPath, &p.IsDir, &p.Content, &p.CreatedAt); err != nil {
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
