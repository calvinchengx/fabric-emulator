package store

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
