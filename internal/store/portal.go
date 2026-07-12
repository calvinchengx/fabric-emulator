package store

// Portal queries — read-only listings for the operator portal, which has no
// principal and therefore no RBAC scope (ListWorkspacesFor is the API-facing,
// principal-scoped variant).

// ListAllWorkspaces returns every workspace, newest first.
func (s *Store) ListAllWorkspaces() ([]*Workspace, error) {
	rows, err := s.db.Query(
		`SELECT id, display_name, description, capacity_id FROM workspaces ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Workspace
	for rows.Next() {
		w := &Workspace{Type: "Workspace"}
		if err := rows.Scan(&w.ID, &w.DisplayName, &w.Description, &w.CapacityID); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ListOperations returns the most recent operations, newest first.
func (s *Store) ListOperations(limit int) ([]*Operation, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(
		`SELECT id, kind, created_at, complete_at, result_ref, fail_with
		 FROM operations ORDER BY created_at DESC, id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Operation
	for rows.Next() {
		op := &Operation{}
		if err := rows.Scan(&op.ID, &op.Kind, &op.CreatedAt, &op.CompleteAt, &op.ResultRef, &op.FailWith); err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}
