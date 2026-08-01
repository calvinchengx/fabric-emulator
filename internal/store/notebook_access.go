package store

// Observed notebook I/O: what a notebook run actually read and wrote through
// the emulator's own data plane, tagged with the cell that did it.
//
// This is evidence, not a report. The emulator IS the storage layer, so it sees
// every request; the notebook runtime only has to say which job and cell it is
// executing, which it already knows because it runs cells one at a time.

// Access directions.
const (
	AccessRead  = "read"
	AccessWrite = "write"
)

// NotebookAccess is one observed dataset touch during a notebook run.
type NotebookAccess struct {
	JobID     string
	CellIndex int
	ItemID    string
	Path      string
	Direction string
}

// RecordNotebookAccess stores an observed touch. Repeats collapse: a Delta
// write hits many files under one table root, and the table is what lineage is
// about.
func (s *Store) RecordNotebookAccess(a *NotebookAccess) error {
	_, err := s.db.Exec(`
INSERT INTO notebook_accesses (job_id, cell_index, item_id, path, direction, created_at)
VALUES (?,?,?,?,?,?)
ON CONFLICT(job_id, cell_index, item_id, path, direction) DO NOTHING`,
		a.JobID, a.CellIndex, a.ItemID, a.Path, a.Direction, s.Now())
	return err
}

// ListNotebookAccesses returns a run's observed touches in insertion order.
func (s *Store) ListNotebookAccesses(jobID string) ([]*NotebookAccess, error) {
	rows, err := s.db.Query(`
SELECT job_id, cell_index, item_id, path, direction FROM notebook_accesses
WHERE job_id = ? ORDER BY rowid`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*NotebookAccess
	for rows.Next() {
		a := &NotebookAccess{}
		if err := rows.Scan(&a.JobID, &a.CellIndex, &a.ItemID, &a.Path, &a.Direction); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
