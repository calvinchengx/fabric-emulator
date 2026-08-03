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
	// ProducerWarehouse is a T-SQL write the emulator's own TDS front saw the
	// engine accept — the warehouse equivalent of NotebookObserved, and how a
	// dbt-built gold layer reaches the graph (internal/server/warehouselineage.go).
	ProducerWarehouse = "Warehouse"
	// ProducerDirectLake is a semantic model's declared binding to a lakehouse
	// table: the model reads that Delta at query time, so the edge states what
	// the emulator itself does to serve a query (internal/api/modellineage.go).
	ProducerDirectLake = "DirectLake"
	// ProducerReported is a movement an engine CLAIMED, outside any job — an
	// interactive Spark session or a plain script. The emulator did not watch
	// this one happen, and the producer says so.
	ProducerReported = "Reported"
)

// What a lineage edge's SOURCE ref names.
//
// A medallion does not begin in Fabric. It begins at a vendor's REST API, a
// production database, a change stream — and until these existed every edge
// endpoint had to be a (workspace, item, path) triple, so the first hop could
// only ever be drawn from a file already sitting in Files/landing. The system
// that PUT it there could not be said at all.
const (
	// SourceKindItem: SourceItemID is a Fabric item, SourcePath a path in it.
	SourceKindItem = "item"
	// SourceKindConnection: SourceItemID is a CONNECTION id, and the source is
	// the system that connection addresses. SourceWorkspaceID and SourcePath
	// are empty — a REST API has neither.
	//
	// A connection and not a free-form URI because the connection already
	// exists: the pipeline created it to hold the vendor's credential, it
	// carries a display name and a connectivity type, and it is what the
	// ingesting client actually authenticated through. Naming it keeps the
	// design's rule — an edge records what happened, never a guess about it.
	SourceKindConnection = "connection"
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
	Producer string `json:"producer"`
	// SourceKind types the source ref: SourceKindItem (the default, and what
	// every edge before connections was) or SourceKindConnection.
	SourceKind string `json:"sourceKind,omitempty"`
	CreatedAt  int64  `json:"-"`
}

// SourceIsConnection reports whether this edge starts outside Fabric.
func (e *LineageEdge) SourceIsConnection() bool { return e.SourceKind == SourceKindConnection }

// nullIfEmpty maps "no job" to SQL NULL, which the foreign key accepts and ''
// would not. See relaxLineageJobFK.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *Store) CreateLineageEdge(e *LineageEdge) error {
	if e.ID == "" {
		e.ID = NewID()
	}
	if e.Producer == "" {
		e.Producer = ProducerCopy
	}
	if e.SourceKind == "" {
		e.SourceKind = SourceKindItem
	}
	e.CreatedAt = s.Now()
	// A bare ON CONFLICT, not a named target. Two uniqueness rules apply here —
	// the table's UNIQUE for job-produced edges, and the partial index that
	// stands in for it where job_id is NULL (see relaxLineageJobFK) — and a
	// named target only ever suppresses one of them, turning a re-reported
	// job-less edge into a constraint error rather than a no-op.
	res, err := s.db.Exec(`INSERT INTO lineage_edges
(id, workspace_id, job_id, activity_name, source_workspace_id, source_item_id, source_path, target_workspace_id, target_item_id, target_path, producer, source_kind, created_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT DO NOTHING`,
		e.ID, e.WorkspaceID, nullIfEmpty(e.JobID), e.ActivityName, e.SourceWorkspaceID, e.SourceItemID, e.SourcePath,
		e.TargetWorkspaceID, e.TargetItemID, e.TargetPath, e.Producer, e.SourceKind, e.CreatedAt)
	if err != nil {
		return err
	}
	// Announce only a NEW edge. The conflict clause makes re-recording harmless,
	// and a re-run of a pipeline replays every edge it already had — publishing
	// those would fill a watching client's log with movements that did not
	// happen this time.
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		s.publish(Event{
			Kind: KindLineage, WorkspaceID: e.WorkspaceID, ItemID: e.TargetItemID,
			SourceItemID: e.SourceItemID, SourcePath: e.SourcePath, SourceKind: e.SourceKind,
			TargetPath: e.TargetPath, Producer: e.Producer,
			JobID: e.JobID, ActivityName: e.ActivityName,
		})
	}
	return nil
}

// RenameLineagePath moves the edges that BUILT one path onto its new name.
//
// It exists for the build-then-swap a warehouse tool performs: dbt materialises
// into `x__dbt_temp` and renames to `x`. Without this the graph would name the
// scaffold and never the table anyone queries.
//
// Only the TARGET end is rewritten, and the asymmetry is the whole subtlety.
// Rebuilding a table that already exists, dbt renames the OLD one out of the
// way first:
//
//	rename x            -> x__dbt_backup
//	rename x__dbt_temp  -> x
//	drop   x__dbt_backup
//
// Rewriting sources too meant step 1 quietly repointed every edge that READ x —
// a downstream aggregate's inputs — at the backup, which step 3 then dropped.
// The medallion showed it plainly: `fct_orders__dbt_backup -> fct_daily_revenue`,
// an edge from a table that no longer exists.
//
// Targets alone is also sufficient: nothing is ever built FROM the temp name,
// so the temp only ever appears as a target. And an edge records where data
// came from at the time it moved — a later rename of the source does not
// rewrite that history.
func (s *Store) RenameLineagePath(itemID, from, to string) error {
	if from == to {
		return nil
	}
	_, err := s.db.Exec(
		`UPDATE OR REPLACE lineage_edges SET target_path = ? WHERE target_item_id = ? AND target_path = ?`,
		to, itemID, from)
	return err
}

// DeleteLineageEdgesInto retires the edges that produced a path, for when the
// object is dropped. Edges FROM it are left alone: they describe movements that
// really happened, and the table existing now is a different question.
func (s *Store) DeleteLineageEdgesInto(itemID, path string) error {
	_, err := s.db.Exec(`DELETE FROM lineage_edges WHERE target_item_id = ? AND target_path = ?`, itemID, path)
	return err
}

func (s *Store) ListLineageEdges(workspaceID string) ([]*LineageEdge, error) {
	rows, err := s.db.Query(`SELECT id, workspace_id, COALESCE(job_id, ''), activity_name, source_workspace_id, source_item_id, source_path,
target_workspace_id, target_item_id, target_path, producer, source_kind, created_at FROM lineage_edges WHERE workspace_id = ? ORDER BY rowid`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*LineageEdge
	for rows.Next() {
		e := &LineageEdge{}
		if err := rows.Scan(&e.ID, &e.WorkspaceID, &e.JobID, &e.ActivityName, &e.SourceWorkspaceID, &e.SourceItemID,
			&e.SourcePath, &e.TargetWorkspaceID, &e.TargetItemID, &e.TargetPath, &e.Producer, &e.SourceKind, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) GetLineageEdge(id string) (*LineageEdge, error) {
	e := &LineageEdge{}
	err := s.db.QueryRow(`SELECT id, workspace_id, COALESCE(job_id, ''), activity_name, source_workspace_id, source_item_id, source_path,
target_workspace_id, target_item_id, target_path, producer, source_kind, created_at FROM lineage_edges WHERE id = ?`, id).Scan(
		&e.ID, &e.WorkspaceID, &e.JobID, &e.ActivityName, &e.SourceWorkspaceID, &e.SourceItemID, &e.SourcePath,
		&e.TargetWorkspaceID, &e.TargetItemID, &e.TargetPath, &e.Producer, &e.SourceKind, &e.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return e, err
}

// PublishQuery reports a read of a semantic model — the Power BI end of a
// flow. It is an event and not an edge on purpose: a query moves no data into
// anything, and lineage_edges records movement.
func (s *Store) PublishQuery(workspaceID, itemID, dataset string, queries int, status string) {
	s.publish(Event{
		Kind: KindQuery, WorkspaceID: workspaceID, ItemID: itemID,
		Dataset: dataset, Queries: queries, Status: status,
	})
}
