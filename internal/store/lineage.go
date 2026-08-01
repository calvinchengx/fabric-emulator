package store

import "database/sql"

// Producers of a lineage edge. Every edge records which mechanism observed it,
// so a catalog adapter can tell an activity the emulator executed itself from
// one an engine reported back. Neither is ever inferred from user code.
const (
	ProducerCopy     = "Copy"     // the pipeline executor moved the bytes
	ProducerNotebook = "Notebook" // an engine reported what a notebook read/wrote
	// ProducerNotebookObserved is stronger than a report: the emulator's own
	// data plane saw the request, attributed to the cell that made it.
	ProducerNotebookObserved = "NotebookObserved"
)

// LineageEdge is an exact source-to-target data movement observed while an
// activity executes. Paths are retained so table-level catalog adapters can
// select Tables/<name> edges without guessing from user code.
type LineageEdge struct {
	ID                string `json:"id"`
	WorkspaceID       string `json:"workspaceId"`
	JobID             string `json:"jobInstanceId"`
	ActivityName      string `json:"activityName"`
	SourceWorkspaceID string `json:"sourceWorkspaceId"`
	SourceItemID      string `json:"sourceItemId"`
	SourcePath        string `json:"sourcePath"`
	TargetWorkspaceID string `json:"targetWorkspaceId"`
	TargetItemID      string `json:"targetItemId"`
	TargetPath        string `json:"targetPath"`
	// Producer is what observed this edge: ProducerCopy or ProducerNotebook.
	Producer  string `json:"producer"`
	CreatedAt int64  `json:"-"`
}

func (s *Store) CreateLineageEdge(e *LineageEdge) error {
	if e.ID == "" {
		e.ID = NewID()
	}
	if e.Producer == "" {
		e.Producer = ProducerCopy
	}
	e.CreatedAt = s.Now()
	_, err := s.db.Exec(`INSERT INTO lineage_edges
(id, workspace_id, job_id, activity_name, source_workspace_id, source_item_id, source_path, target_workspace_id, target_item_id, target_path, producer, created_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(job_id, activity_name, source_item_id, source_path, target_item_id, target_path) DO NOTHING`,
		e.ID, e.WorkspaceID, e.JobID, e.ActivityName, e.SourceWorkspaceID, e.SourceItemID, e.SourcePath,
		e.TargetWorkspaceID, e.TargetItemID, e.TargetPath, e.Producer, e.CreatedAt)
	return err
}

func (s *Store) ListLineageEdges(workspaceID string) ([]*LineageEdge, error) {
	rows, err := s.db.Query(`SELECT id, workspace_id, job_id, activity_name, source_workspace_id, source_item_id, source_path,
target_workspace_id, target_item_id, target_path, producer, created_at FROM lineage_edges WHERE workspace_id = ? ORDER BY rowid`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*LineageEdge
	for rows.Next() {
		e := &LineageEdge{}
		if err := rows.Scan(&e.ID, &e.WorkspaceID, &e.JobID, &e.ActivityName, &e.SourceWorkspaceID, &e.SourceItemID,
			&e.SourcePath, &e.TargetWorkspaceID, &e.TargetItemID, &e.TargetPath, &e.Producer, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) GetLineageEdge(id string) (*LineageEdge, error) {
	e := &LineageEdge{}
	err := s.db.QueryRow(`SELECT id, workspace_id, job_id, activity_name, source_workspace_id, source_item_id, source_path,
target_workspace_id, target_item_id, target_path, producer, created_at FROM lineage_edges WHERE id = ?`, id).Scan(
		&e.ID, &e.WorkspaceID, &e.JobID, &e.ActivityName, &e.SourceWorkspaceID, &e.SourceItemID, &e.SourcePath,
		&e.TargetWorkspaceID, &e.TargetItemID, &e.TargetPath, &e.Producer, &e.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return e, err
}
