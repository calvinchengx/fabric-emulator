package store

import (
	"database/sql"
	"encoding/json"
	"errors"
)

// CreateItem inserts an item, optionally with definition parts (stored
// verbatim so getDefinition round-trips exactly what was written).
func (s *Store) CreateItem(it *Item, parts []DefinitionPart) error {
	it.CreatedAt = s.Now()
	if it.ID == "" {
		it.ID = NewID()
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`INSERT INTO items (id, workspace_id, type, display_name, description, created_at) VALUES (?,?,?,?,?,?)`,
		it.ID, it.WorkspaceID, it.Type, it.DisplayName, it.Description, it.CreatedAt); err != nil {
		return err
	}
	if len(parts) > 0 {
		blob, err := json.Marshal(parts)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(
			`INSERT INTO item_definitions (item_id, parts_json) VALUES (?,?)`, it.ID, string(blob)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetItem fetches one item scoped to a workspace.
func (s *Store) GetItem(workspaceID, id string) (*Item, error) {
	it := &Item{}
	err := s.db.QueryRow(
		`SELECT id, workspace_id, type, display_name, description, created_at FROM items WHERE workspace_id = ? AND id = ?`,
		workspaceID, id).Scan(&it.ID, &it.WorkspaceID, &it.Type, &it.DisplayName, &it.Description, &it.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return it, err
}

// GetItemByID fetches an item without workspace scoping (LRO results).
func (s *Store) GetItemByID(id string) (*Item, error) {
	it := &Item{}
	err := s.db.QueryRow(
		`SELECT id, workspace_id, type, display_name, description, created_at FROM items WHERE id = ?`, id).
		Scan(&it.ID, &it.WorkspaceID, &it.Type, &it.DisplayName, &it.Description, &it.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return it, err
}

// ListItems returns a workspace's items, optionally filtered by type.
func (s *Store) ListItems(workspaceID, itemType string) ([]*Item, error) {
	q := `SELECT id, workspace_id, type, display_name, description, created_at FROM items WHERE workspace_id = ?`
	args := []any{workspaceID}
	if itemType != "" {
		q += ` AND type = ?`
		args = append(args, itemType)
	}
	q += ` ORDER BY created_at, id`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Item
	for rows.Next() {
		it := &Item{}
		if err := rows.Scan(&it.ID, &it.WorkspaceID, &it.Type, &it.DisplayName, &it.Description, &it.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// UpdateItem applies displayName/description changes.
func (s *Store) UpdateItem(it *Item) error {
	res, err := s.db.Exec(
		`UPDATE items SET display_name = ?, description = ? WHERE workspace_id = ? AND id = ?`,
		it.DisplayName, it.Description, it.WorkspaceID, it.ID)
	if err != nil {
		return err
	}
	return oneRow(res)
}

// DeleteItem removes an item (definition cascades).
func (s *Store) DeleteItem(workspaceID, id string) error {
	res, err := s.db.Exec(`DELETE FROM items WHERE workspace_id = ? AND id = ?`, workspaceID, id)
	if err != nil {
		return err
	}
	return oneRow(res)
}

// GetDefinition returns an item's stored definition parts (nil when the item
// has no definition).
func (s *Store) GetDefinition(itemID string) ([]DefinitionPart, error) {
	var blob string
	err := s.db.QueryRow(`SELECT parts_json FROM item_definitions WHERE item_id = ?`, itemID).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var parts []DefinitionPart
	if err := json.Unmarshal([]byte(blob), &parts); err != nil {
		return nil, err
	}
	return parts, nil
}

// SetDefinition replaces an item's definition parts.
func (s *Store) SetDefinition(itemID string, parts []DefinitionPart) error {
	blob, err := json.Marshal(parts)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO item_definitions (item_id, parts_json) VALUES (?,?)
		 ON CONFLICT(item_id) DO UPDATE SET parts_json = excluded.parts_json`,
		itemID, string(blob))
	return err
}
