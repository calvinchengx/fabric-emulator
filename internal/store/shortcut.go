package store

import (
	"database/sql"
	"errors"
	"sort"
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
	TargetType      string `json:"-"`
	TargetLocation  string `json:"-"`
	// TargetTable is the final path segment for targets that name a table
	// rather than a folder — today only Dataverse, whose target carries a
	// `tableName` alongside its `deltaLakeFolder`. Kept separate from
	// TargetPath so the DTO can echo the two documented fields back exactly
	// instead of guessing where one ends and the other begins; the read path
	// simply appends it, so it is empty and inert for every other type.
	TargetTable  string `json:"-"`
	ConnectionID string `json:"-"`
	CreatedAt    int64  `json:"-"`
}

// CreateShortcut stores a shortcut (unique per item+path+name).
func (s *Store) CreateShortcut(sc *Shortcut) error {
	sc.CreatedAt = s.Now()
	_, err := s.db.Exec(`
INSERT INTO shortcuts (item_id, path, name, target_workspace, target_item, target_path, target_type, target_location, target_table, connection_id, created_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		sc.ItemID, sc.Path, sc.Name, sc.TargetWorkspace, sc.TargetItem, sc.TargetPath,
		sc.TargetType, sc.TargetLocation, sc.TargetTable, sc.ConnectionID, sc.CreatedAt)
	return err
}

// GetShortcut fetches one shortcut by its item+path+name.
func (s *Store) GetShortcut(itemID, path, name string) (*Shortcut, error) {
	sc := &Shortcut{}
	err := s.db.QueryRow(`
SELECT item_id, path, name, target_workspace, target_item, target_path, target_type, target_location, target_table, connection_id, created_at
FROM shortcuts WHERE item_id = ? AND path = ? AND name = ?`, itemID, path, name).
		Scan(&sc.ItemID, &sc.Path, &sc.Name, &sc.TargetWorkspace, &sc.TargetItem, &sc.TargetPath,
			&sc.TargetType, &sc.TargetLocation, &sc.TargetTable, &sc.ConnectionID, &sc.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return sc, err
}

// ListShortcuts returns an item's shortcuts.
func (s *Store) ListShortcuts(itemID string) ([]*Shortcut, error) {
	rows, err := s.db.Query(`
SELECT item_id, path, name, target_workspace, target_item, target_path, target_type, target_location, target_table, connection_id, created_at
FROM shortcuts WHERE item_id = ? ORDER BY rowid`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Shortcut
	for rows.Next() {
		sc := &Shortcut{}
		if err := rows.Scan(&sc.ItemID, &sc.Path, &sc.Name, &sc.TargetWorkspace, &sc.TargetItem, &sc.TargetPath,
			&sc.TargetType, &sc.TargetLocation, &sc.TargetTable, &sc.ConnectionID, &sc.CreatedAt); err != nil {
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

// externalTargetTypes is the ONE list of shortcut target types that resolve
// outside OneLake. It lives in store, next to the Shortcut it describes,
// because two packages were re-deriving the same question by literal.
//
// The count matters: this predicate was already extracted once, in
// internal/onelake, for the narrower "may I write here" question — and the
// pattern was not followed, so four other sites across two packages kept
// asking "is this external at all" with an inline `TargetType == "ADLSGen2"
// || TargetType == "AmazonS3"`. An extracted predicate that its own neighbours
// ignore is the shape that drifts: the survivors are exactly the sites nobody
// updates when a type is added.
//
// Adding a target type means adding it here, once.
var externalTargetTypes = map[string]bool{
	"ADLSGen2":  true,
	"AmazonS3":  true,
	"Dataverse": true,
}

// IsExternalTarget reports whether this shortcut resolves outside OneLake, so
// its reads, writes and deletes belong to the target rather than to us.
//
// Note this is NOT "may I write to it": a target can be external and still be
// read-only (S3 and Dataverse both are). That question is externalWritable in
// internal/onelake, deliberately separate and narrower — collapsing the two is
// what would let a read-only target accept a write.
func (sc *Shortcut) IsExternalTarget() bool {
	return sc != nil && externalTargetTypes[sc.TargetType]
}

// ExternalTargetTypes returns the registered external target types, sorted.
//
// Exported so tests in other packages can enumerate list-vs-list agreement
// over PAIRS rather than restating the list — a second copy written by hand is
// the bug this whole predicate exists to remove, and a test carrying its own
// copy would go green precisely when the two lists drifted apart.
func ExternalTargetTypes() []string {
	out := make([]string, 0, len(externalTargetTypes))
	for t := range externalTargetTypes {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
