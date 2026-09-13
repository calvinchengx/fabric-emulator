package store

// Item permissions: access to one item, independent of the workspace.
//
// TWO SOURCES, ONE ANSWER. A principal's access to an item is what its workspace
// role implies UNIONED with what was granted on the item directly — "if Veronica
// decides to remove Marta's item permissions from the report, Marta will still be
// able to view the report in the workspace" (permission-model.md). Only the direct
// half is stored; the role half is computed on every read, so a role change can
// never leave a stale copy of the access it used to imply.
//
// See docs/57-item-permissions.md for the sources behind each table below.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
)

// Item permission names — the ItemPermissions enum of the REST reference.
const (
	PermRead    = "Read"
	PermWrite   = "Write"
	PermReshare = "Reshare"
	PermExplore = "Explore"
	PermExecute = "Execute"
)

// Additional (workload) permissions, reported beside ItemPermissions as
// `additionalPermissions`.
const (
	PermReadAll  = "ReadAll"
	PermReadData = "ReadData"
)

// ItemAccess is one principal's DIRECT grant on one item.
type ItemAccess struct {
	ItemID        string
	PrincipalID   string
	PrincipalType string
	Permissions   []string
	Additional    []string
}

// Access is a principal's EFFECTIVE access to an item: role-implied ∪ direct.
type Access struct {
	Permissions []string
	Additional  []string
	// Role is the principal's workspace role on the item's workspace, "" for
	// none. Carried because several consumers decide on it before permissions.
	Role string
	// Direct reports whether any of this came from a grant on the item.
	Direct bool
}

// Has reports whether the access includes a permission, item or additional.
func (a Access) Has(name string) bool {
	for _, set := range [][]string{a.Permissions, a.Additional} {
		for _, p := range set {
			if p == name {
				return true
			}
		}
	}
	return false
}

// InheritedAccess is what a workspace role implies on an item of a type.
//
// Semantic models follow Put Dataset User's statement of inheritance: "For folder
// admins and members, the ReadWriteReshareExplore permission on the folder's
// datasets is inherited. For folder contributors, the ReadWriteExplore
// permission … For folder viewers, the Read permission." Every other item follows
// the Roles in workspaces table: ReadData for every role, ReadAll, Write and
// Execute from Contributor, and resharing for Admin and Member only.
func InheritedAccess(role, itemType string) (permissions, additional []string) {
	rank := RoleRank(role)
	if rank < 0 {
		return nil, nil
	}
	if itemType == "SemanticModel" {
		permissions = []string{PermRead}
		if rank >= RoleRank(RoleContributor) {
			permissions = append(permissions, PermWrite, PermExplore)
		}
		if rank >= RoleRank(RoleMember) {
			permissions = append(permissions, PermReshare)
		}
		return sortedSet(permissions), nil
	}
	permissions = []string{PermRead}
	additional = []string{PermReadData}
	if rank >= RoleRank(RoleContributor) {
		permissions = append(permissions, PermWrite, PermExecute)
		additional = append(additional, PermReadAll)
	}
	if rank >= RoleRank(RoleMember) {
		permissions = append(permissions, PermReshare)
	}
	return sortedSet(permissions), sortedSet(additional)
}

// PutItemAccess replaces a principal's direct grant on an item.
//
// REPLACE, NOT MERGE, for the same reason dataAccessRoles replaces: a merge would
// keep a permission the caller believes it removed, which is the direction that
// keeps access alive after someone revoked it.
func (s *Store) PutItemAccess(g ItemAccess) error {
	// Marshalling a []string cannot fail, so there is no error branch to keep.
	perms, _ := json.Marshal(sortedSet(g.Permissions))
	additional, _ := json.Marshal(sortedSet(g.Additional))
	_, err := s.db.Exec(`
INSERT INTO item_access (item_id, principal_id, principal_type, permissions, additional_permissions)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (item_id, principal_id) DO UPDATE SET
	principal_type = excluded.principal_type,
	permissions = excluded.permissions,
	additional_permissions = excluded.additional_permissions`,
		g.ItemID, g.PrincipalID, g.PrincipalType, string(perms), string(additional))
	return err
}

// DeleteItemAccess removes a principal's direct grant. ErrNotFound when there
// was none, because "revoked" and "never granted" are different answers to a
// caller auditing what it just did.
func (s *Store) DeleteItemAccess(itemID, principalID string) error {
	res, err := s.db.Exec(`DELETE FROM item_access WHERE item_id = ? AND principal_id = ?`,
		itemID, principalID)
	if err != nil {
		return err
	}
	return oneRow(res)
}

// ListItemAccess returns every direct grant on an item, ordered by principal.
func (s *Store) ListItemAccess(itemID string) ([]ItemAccess, error) {
	rows, err := s.db.Query(`
SELECT principal_id, principal_type, permissions, additional_permissions
FROM item_access WHERE item_id = ? ORDER BY principal_id`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ItemAccess{}
	for rows.Next() {
		g, err := scanItemAccess(rows, itemID)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// GetItemAccess returns a principal's direct grant, or nil when there is none.
func (s *Store) GetItemAccess(itemID, principalID string) (*ItemAccess, error) {
	row := s.db.QueryRow(`
SELECT principal_id, principal_type, permissions, additional_permissions
FROM item_access WHERE item_id = ? AND principal_id = ?`, itemID, principalID)
	g, err := scanItemAccess(row, itemID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

type scanner interface{ Scan(dest ...any) error }

// scanItemAccess reads one row. A permission list that will not decode is an
// ERROR, not an empty grant: silently reading it as nothing would narrow
// someone's access and look like a revoke nobody made.
func scanItemAccess(row scanner, itemID string) (ItemAccess, error) {
	g := ItemAccess{ItemID: itemID}
	var perms, additional string
	if err := row.Scan(&g.PrincipalID, &g.PrincipalType, &perms, &additional); err != nil {
		return ItemAccess{}, err
	}
	if err := json.Unmarshal([]byte(perms), &g.Permissions); err != nil {
		return ItemAccess{}, err
	}
	if err := json.Unmarshal([]byte(additional), &g.Additional); err != nil {
		return ItemAccess{}, err
	}
	return g, nil
}

// EffectiveItemAccess is a principal's access to an item: what its workspace
// role implies, unioned with its direct grant.
func (s *Store) EffectiveItemAccess(it *Item, principalID string) (Access, error) {
	role, err := s.RoleOf(it.WorkspaceID, principalID)
	if err != nil {
		return Access{}, err
	}
	g, err := s.GetItemAccess(it.ID, principalID)
	if err != nil {
		return Access{}, err
	}
	return MergeAccess(role, it.Type, g), nil
}

// MergeAccess is the union itself, for a caller that already holds the role and
// the grant — listing every principal on an item should not query per principal,
// and must not compute the union a second, separate way. g may be nil.
func MergeAccess(role, itemType string, g *ItemAccess) Access {
	perms, additional := InheritedAccess(role, itemType)
	a := Access{Role: role, Permissions: perms, Additional: additional}
	if g != nil {
		a.Direct = true
		a.Permissions = sortedSet(append(a.Permissions, g.Permissions...))
		a.Additional = sortedSet(append(a.Additional, g.Additional...))
	}
	return a
}

// sortedSet de-duplicates and sorts, so a stored or reported permission list has
// one spelling however it was built.
func sortedSet(xs []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, x := range xs {
		if x != "" && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}
