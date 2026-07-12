package store

import (
	"database/sql"
	"errors"
)

// Shortcut is a OneLake symlink: a named entry inside an item's managed
// folder (path) whose reads resolve into a target OneLake location. No data
// is copied — resolution happens per request.
type Shortcut struct {
	ItemID          string `json:"-"`
	Path            string `json:"path"`
	Name            string `json:"name"`
	TargetWorkspace string `json:"-"`
	TargetItem      string `json:"-"`
	TargetPath      string `json:"-"`
	CreatedAt       int64  `json:"-"`
}

// CreateShortcut stores a shortcut (unique per item+path+name).
func (s *Store) CreateShortcut(sc *Shortcut) error {
	sc.CreatedAt = s.Now()
	_, err := s.db.Exec(`
INSERT INTO shortcuts (item_id, path, name, target_workspace, target_item, target_path, created_at)
VALUES (?,?,?,?,?,?,?)`,
		sc.ItemID, sc.Path, sc.Name, sc.TargetWorkspace, sc.TargetItem, sc.TargetPath, sc.CreatedAt)
	return err
}

// GetShortcut fetches one shortcut by its item+path+name.
func (s *Store) GetShortcut(itemID, path, name string) (*Shortcut, error) {
	sc := &Shortcut{}
	err := s.db.QueryRow(`
SELECT item_id, path, name, target_workspace, target_item, target_path, created_at
FROM shortcuts WHERE item_id = ? AND path = ? AND name = ?`, itemID, path, name).
		Scan(&sc.ItemID, &sc.Path, &sc.Name, &sc.TargetWorkspace, &sc.TargetItem, &sc.TargetPath, &sc.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return sc, err
}

// ListShortcuts returns an item's shortcuts.
func (s *Store) ListShortcuts(itemID string) ([]*Shortcut, error) {
	rows, err := s.db.Query(`
SELECT item_id, path, name, target_workspace, target_item, target_path, created_at
FROM shortcuts WHERE item_id = ? ORDER BY rowid`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Shortcut
	for rows.Next() {
		sc := &Shortcut{}
		if err := rows.Scan(&sc.ItemID, &sc.Path, &sc.Name, &sc.TargetWorkspace, &sc.TargetItem, &sc.TargetPath, &sc.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// DeleteShortcut removes a shortcut (never touches the target data).
func (s *Store) DeleteShortcut(itemID, path, name string) error {
	res, err := s.db.Exec(`DELETE FROM shortcuts WHERE item_id = ? AND path = ? AND name = ?`, itemID, path, name)
	if err != nil {
		return err
	}
	return oneRow(res)
}

// ShortcutFor returns the shortcut whose (path/name) is the longest prefix of
// relPath within the item, plus the remainder after the shortcut name. Used
// by the data plane to resolve a read through a shortcut. Returns nil when no
// shortcut matches.
func (s *Store) ShortcutFor(itemID, relPath string) (*Shortcut, string, error) {
	shortcuts, err := s.ListShortcuts(itemID)
	if err != nil {
		return nil, "", err
	}
	for _, sc := range shortcuts {
		prefix := sc.Path + "/" + sc.Name
		if relPath == prefix {
			return sc, "", nil
		}
		if len(relPath) > len(prefix) && relPath[:len(prefix)+1] == prefix+"/" {
			return sc, relPath[len(prefix)+1:], nil
		}
	}
	return nil, "", nil
}
