package store

import (
	"database/sql"
	"errors"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// CreateWorkspace inserts a workspace and grants creator the Admin role.
func (s *Store) CreateWorkspace(w *Workspace, creator Principal) error {
	w.Type = "Workspace"
	w.CreatedAt = s.Now()
	if w.ID == "" {
		w.ID = NewID()
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`INSERT INTO workspaces (id, display_name, description, capacity_id, created_at) VALUES (?,?,?,?,?)`,
		w.ID, w.DisplayName, w.Description, w.CapacityID, w.CreatedAt); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO role_assignments (id, workspace_id, principal_id, principal_type, role) VALUES (?,?,?,?,?)`,
		NewID(), w.ID, creator.ID, creator.Type, RoleAdmin); err != nil {
		return err
	}
	return tx.Commit()
}

// GetWorkspace fetches one workspace.
func (s *Store) GetWorkspace(id string) (*Workspace, error) {
	w := &Workspace{Type: "Workspace"}
	err := s.db.QueryRow(
		`SELECT id, display_name, description, capacity_id, created_at FROM workspaces WHERE id = ?`, id).
		Scan(&w.ID, &w.DisplayName, &w.Description, &w.CapacityID, &w.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return w, err
}

// ListWorkspacesFor returns workspaces where the principal holds any role.
func (s *Store) ListWorkspacesFor(principalID string) ([]*Workspace, error) {
	rows, err := s.db.Query(`
SELECT w.id, w.display_name, w.description, w.capacity_id, w.created_at
FROM workspaces w JOIN role_assignments ra ON ra.workspace_id = w.id
WHERE ra.principal_id = ? ORDER BY w.rowid`, principalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Workspace
	for rows.Next() {
		w := &Workspace{Type: "Workspace"}
		if err := rows.Scan(&w.ID, &w.DisplayName, &w.Description, &w.CapacityID, &w.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// UpdateWorkspace applies displayName/description changes.
func (s *Store) UpdateWorkspace(w *Workspace) error {
	res, err := s.db.Exec(
		`UPDATE workspaces SET display_name = ?, description = ?, capacity_id = ? WHERE id = ?`,
		w.DisplayName, w.Description, w.CapacityID, w.ID)
	if err != nil {
		return err
	}
	return oneRow(res)
}

// DeleteWorkspace removes the workspace; items and role assignments cascade.
func (s *Store) DeleteWorkspace(id string) error {
	res, err := s.db.Exec(`DELETE FROM workspaces WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return oneRow(res)
}

// RoleOf returns the principal's role on the workspace ("" when none).
func (s *Store) RoleOf(workspaceID, principalID string) (string, error) {
	var role string
	err := s.db.QueryRow(
		`SELECT role FROM role_assignments WHERE workspace_id = ? AND principal_id = ?`,
		workspaceID, principalID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return role, err
}

// ListRoleAssignments returns all assignments on a workspace.
func (s *Store) ListRoleAssignments(workspaceID string) ([]*RoleAssignment, error) {
	rows, err := s.db.Query(
		`SELECT id, workspace_id, principal_id, principal_type, role FROM role_assignments WHERE workspace_id = ? ORDER BY rowid`,
		workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*RoleAssignment
	for rows.Next() {
		ra := &RoleAssignment{}
		if err := rows.Scan(&ra.ID, &ra.WorkspaceID, &ra.Principal.ID, &ra.Principal.Type, &ra.Role); err != nil {
			return nil, err
		}
		out = append(out, ra)
	}
	return out, rows.Err()
}

// GetRoleAssignment fetches one assignment scoped to a workspace.
func (s *Store) GetRoleAssignment(workspaceID, id string) (*RoleAssignment, error) {
	ra := &RoleAssignment{}
	err := s.db.QueryRow(
		`SELECT id, workspace_id, principal_id, principal_type, role FROM role_assignments WHERE workspace_id = ? AND id = ?`,
		workspaceID, id).Scan(&ra.ID, &ra.WorkspaceID, &ra.Principal.ID, &ra.Principal.Type, &ra.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return ra, err
}

// CreateRoleAssignment grants a role. Duplicate principals on one workspace
// are rejected by the unique index.
func (s *Store) CreateRoleAssignment(ra *RoleAssignment) error {
	if ra.ID == "" {
		ra.ID = NewID()
	}
	_, err := s.db.Exec(
		`INSERT INTO role_assignments (id, workspace_id, principal_id, principal_type, role) VALUES (?,?,?,?,?)`,
		ra.ID, ra.WorkspaceID, ra.Principal.ID, ra.Principal.Type, ra.Role)
	return err
}

// UpdateRoleAssignment changes the role on an existing assignment.
func (s *Store) UpdateRoleAssignment(workspaceID, id, role string) error {
	res, err := s.db.Exec(
		`UPDATE role_assignments SET role = ? WHERE workspace_id = ? AND id = ?`, role, workspaceID, id)
	if err != nil {
		return err
	}
	return oneRow(res)
}

// DeleteRoleAssignment revokes an assignment.
func (s *Store) DeleteRoleAssignment(workspaceID, id string) error {
	res, err := s.db.Exec(
		`DELETE FROM role_assignments WHERE workspace_id = ? AND id = ?`, workspaceID, id)
	if err != nil {
		return err
	}
	return oneRow(res)
}

func oneRow(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
