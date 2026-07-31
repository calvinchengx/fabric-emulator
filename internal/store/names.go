package store

import (
	"errors"
	"strings"
)

// ErrNameConflict is returned when a write violates display-name uniqueness.
// The API checks names up front for a clean 409, but the database constraint
// is the real guarantee (concurrent creates can both pass a pre-check), so
// stores translate the constraint violation into this sentinel.
var ErrNameConflict = errors.New("display name already in use")

// nameConflict maps a UNIQUE-index violation on one of the display-name
// indexes to ErrNameConflict; other errors pass through untouched.
func nameConflict(err error) error {
	if err == nil {
		return nil
	}
	m := err.Error()
	if strings.Contains(m, "UNIQUE constraint failed") &&
		(strings.Contains(m, "ux_workspaces_display_name") ||
			strings.Contains(m, "ux_items_ws_name_type") ||
			strings.Contains(m, "workspaces.display_name") ||
			strings.Contains(m, "items.display_name") ||
			strings.Contains(m, "domains.display_name")) {
		return errors.Join(ErrNameConflict, err)
	}
	return err
}

// Display-name uniqueness. Real Fabric rejects duplicates, and the emulator
// must too — name collisions silently break every name-addressed contract
// (OneLake paths, git logical ids, the FABRIC_TARGET toggle, catalog ingest).
//
// Scope differs by entity:
//   - workspaces: unique tenant-wide (REST reference; fabric-docs covers
//     workspace naming portal-side only).
//   - items: unique per (workspace, type) — names ARE reusable across types,
//     which is exactly why OneLake addresses items as `name.Type`
//     ("you can reuse item names across multiple item types",
//     onelake-access-api.md).
//
// Both are case-insensitive, matching how the OneLake resolver already
// compares names.

// WorkspaceNameTaken reports whether another workspace holds this display
// name. exceptID (may be empty) is the workspace being renamed, so keeping
// its own name never conflicts.
func (s *Store) WorkspaceNameTaken(displayName, exceptID string) (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(1) FROM workspaces WHERE display_name = ? COLLATE NOCASE AND id <> ?`,
		displayName, exceptID).Scan(&n)
	return n > 0, err
}

// ItemNameTaken reports whether another item of the same type in this
// workspace holds this display name. exceptID (may be empty) is the item
// being renamed.
func (s *Store) ItemNameTaken(workspaceID, displayName, itemType, exceptID string) (bool, error) {
	var n int
	err := s.db.QueryRow(`
SELECT COUNT(1) FROM items
WHERE workspace_id = ? AND display_name = ? COLLATE NOCASE
  AND type = ? COLLATE NOCASE AND id <> ?`,
		workspaceID, displayName, itemType, exceptID).Scan(&n)
	return n > 0, err
}
