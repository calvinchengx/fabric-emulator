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

// ListJobInstances returns the most recent job instances, newest first.
func (s *Store) ListJobInstances(limit int) ([]*JobInstance, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(
		`SELECT id, item_id, job_type, invoke_type, created_at, complete_at, cancelled, fail_with
		 FROM job_instances ORDER BY created_at DESC, id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*JobInstance
	for rows.Next() {
		j := &JobInstance{}
		if err := rows.Scan(&j.ID, &j.ItemID, &j.JobType, &j.InvokeType, &j.CreatedAt, &j.CompleteAt, &j.Cancelled, &j.FailWith); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ListAllLineageEdges returns recent lineage edges across every workspace,
// newest first. The API-facing ListLineageEdges is workspace-scoped because it
// is behind RBAC; the portal has no principal and shows the whole tenant.
func (s *Store) ListAllLineageEdges(limit int) ([]*LineageEdge, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.Query(`
SELECT id, workspace_id, COALESCE(job_id, ''), activity_name, source_workspace_id, source_item_id, source_path,
       target_workspace_id, target_item_id, target_path, producer, created_at
FROM lineage_edges ORDER BY rowid DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*LineageEdge
	for rows.Next() {
		e := &LineageEdge{}
		if err := rows.Scan(&e.ID, &e.WorkspaceID, &e.JobID, &e.ActivityName,
			&e.SourceWorkspaceID, &e.SourceItemID, &e.SourcePath,
			&e.TargetWorkspaceID, &e.TargetItemID, &e.TargetPath, &e.Producer, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
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
