package store

import (
	"database/sql"
	"encoding/json"
	"errors"
)

// Job instance statuses (clock-derived like operations).
const (
	JobNotStarted = "NotStarted"
	JobInProgress = "InProgress"
	JobCompleted  = "Completed"
	JobFailed     = "Failed"
	JobCancelled  = "Cancelled"
	JobQueued     = "Queued"
)

const jobCols = `id, item_id, job_type, invoke_type, created_at, complete_at, cancelled, fail_with, queued, execution_data`

// JobInstance is a scheduled item job. Status is derived from the controllable
// clock, with Cancelled overriding everything and Queued overriding the clock
// until admission (docs/36).
type JobInstance struct {
	ID            string
	ItemID        string
	JobType       string
	InvokeType    string
	CreatedAt     int64
	CompleteAt    int64
	Cancelled     bool
	FailWith      string
	Queued        bool
	ExecutionData map[string]any
}

// StatusAt derives the wire status at the given clock time.
func (j JobInstance) StatusAt(now int64) string {
	if j.Cancelled {
		return JobCancelled
	}
	if j.Queued {
		return JobQueued
	}
	if now < j.CompleteAt {
		if now == j.CreatedAt {
			return JobNotStarted
		}
		return JobInProgress
	}
	if j.FailWith != "" {
		return JobFailed
	}
	return JobCompleted
}

func scanJob(scan func(dest ...any) error) (*JobInstance, error) {
	j := &JobInstance{}
	var queued int
	var execJSON string
	err := scan(&j.ID, &j.ItemID, &j.JobType, &j.InvokeType, &j.CreatedAt, &j.CompleteAt,
		&j.Cancelled, &j.FailWith, &queued, &execJSON)
	if err != nil {
		return nil, err
	}
	j.Queued = queued != 0
	if execJSON != "" {
		_ = json.Unmarshal([]byte(execJSON), &j.ExecutionData)
	}
	return j, nil
}

func jobExecJSON(j *JobInstance) string {
	if len(j.ExecutionData) == 0 {
		return ""
	}
	b, err := json.Marshal(j.ExecutionData)
	if err != nil {
		return ""
	}
	return string(b)
}

// CreateJobInstance records a scheduled job.
func (s *Store) CreateJobInstance(j *JobInstance) error {
	j.CreatedAt = s.Now()
	if j.ID == "" {
		j.ID = NewID()
	}
	if j.InvokeType == "" {
		j.InvokeType = "Manual"
	}
	if j.CompleteAt == 0 {
		j.CompleteAt = j.CreatedAt
	}
	queued := 0
	if j.Queued {
		queued = 1
	}
	_, err := s.db.Exec(`
INSERT INTO job_instances (id, item_id, job_type, invoke_type, created_at, complete_at, cancelled, fail_with, queued, execution_data)
VALUES (?,?,?,?,?,?,?,?,?,?)`,
		j.ID, j.ItemID, j.JobType, j.InvokeType, j.CreatedAt, j.CompleteAt, j.Cancelled, j.FailWith,
		queued, jobExecJSON(j))
	return err
}

// GetJobInstance fetches one job scoped to its item.
func (s *Store) GetJobInstance(itemID, id string) (*JobInstance, error) {
	row := s.db.QueryRow(`SELECT `+jobCols+` FROM job_instances WHERE item_id = ? AND id = ?`, itemID, id)
	j, err := scanJob(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

// ListItemJobInstances returns an item's job instances, newest first — the
// order List Item Job Instances is useful in (the last run is the one you
// want). Distinct from the portal's tenant-wide ListJobInstances.
func (s *Store) ListItemJobInstances(itemID string) ([]*JobInstance, error) {
	rows, err := s.db.Query(`
SELECT `+jobCols+` FROM job_instances WHERE item_id = ? ORDER BY created_at DESC, rowid DESC`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*JobInstance
	for rows.Next() {
		j, err := scanJob(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ListQueuedJobs returns jobs waiting for a capacity slot, oldest first so
// admission is FIFO across the tenant.
func (s *Store) ListQueuedJobs() ([]*JobInstance, error) {
	rows, err := s.db.Query(`
SELECT ` + jobCols + ` FROM job_instances WHERE queued = 1 AND cancelled = 0
ORDER BY created_at ASC, rowid ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*JobInstance
	for rows.Next() {
		j, err := scanJob(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// CountActiveJobsOnCapacity is the number of jobs currently occupying a slot
// on this capacity: admitted (not queued), not cancelled, not yet complete.
// Keyed by capacity, never by item — two jobs on the same notebook both count
// when both are running (docs/36: do not serialise same-item jobs).
func (s *Store) CountActiveJobsOnCapacity(capacityID string) (int, error) {
	var n int
	err := s.db.QueryRow(`
SELECT COUNT(*) FROM job_instances j
JOIN items i ON i.id = j.item_id
JOIN workspaces w ON w.id = i.workspace_id
WHERE w.capacity_id = ?
  AND j.queued = 0
  AND j.cancelled = 0
  AND j.complete_at > ?`, capacityID, s.Now()).Scan(&n)
	return n, err
}

// AdmitQueuedJob clears the queued flag and sets complete_at so the clock (or
// an engine callback) can finish the job. Caller then dispatches execution.
func (s *Store) AdmitQueuedJob(id string, completeAt int64) error {
	res, err := s.db.Exec(
		`UPDATE job_instances SET queued = 0, complete_at = ? WHERE id = ? AND queued = 1`,
		completeAt, id)
	if err != nil {
		return err
	}
	return oneRow(res)
}

// SetPipelineRun records the interpreter's activity-run detail for a pipeline
// job (queried back by the queryactivityruns surface).
func (s *Store) SetPipelineRun(jobID, status, activityRunsJSON string) error {
	_, err := s.db.Exec(`
INSERT INTO pipeline_runs (job_id, status, activity_runs) VALUES (?,?,?)
ON CONFLICT(job_id) DO UPDATE SET status = excluded.status, activity_runs = excluded.activity_runs`,
		jobID, status, activityRunsJSON)
	return err
}

// GetPipelineRun returns the recorded status and activity-runs JSON for a job.
func (s *Store) GetPipelineRun(jobID string) (status, activityRunsJSON string, err error) {
	err = s.db.QueryRow(`SELECT status, activity_runs FROM pipeline_runs WHERE job_id = ?`, jobID).
		Scan(&status, &activityRunsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return status, activityRunsJSON, err
}

// SetJobFailure records a terminal failure code on a job (used when a
// DataPipeline interpreter run fails, overriding the clock-derived success).
func (s *Store) SetJobFailure(itemID, id, failWith string) error {
	res, err := s.db.Exec(
		`UPDATE job_instances SET fail_with = ? WHERE item_id = ? AND id = ?`, failWith, itemID, id)
	if err != nil {
		return err
	}
	return oneRow(res)
}

// FinalizeJob forces a job to a terminal state *now* — complete_at=now with the
// given failure code (empty = success). Used when a real engine reports a
// RunNotebook result, so the job reflects the run rather than the clock.
func (s *Store) FinalizeJob(itemID, id, failWith string) error {
	res, err := s.db.Exec(
		`UPDATE job_instances SET complete_at = ?, fail_with = ? WHERE item_id = ? AND id = ?`,
		s.Now(), failWith, itemID, id)
	if err != nil {
		return err
	}
	return oneRow(res)
}

// SetNotebookRun upserts the parsed/executed run detail for a RunNotebook job.
func (s *Store) SetNotebookRun(jobID, status, runJSON string) error {
	_, err := s.db.Exec(`
INSERT INTO notebook_runs (job_id, status, run) VALUES (?,?,?)
ON CONFLICT(job_id) DO UPDATE SET status = excluded.status, run = excluded.run`,
		jobID, status, runJSON)
	return err
}

// GetNotebookRun returns the recorded status and run JSON for a RunNotebook job.
func (s *Store) GetNotebookRun(jobID string) (status, runJSON string, err error) {
	err = s.db.QueryRow(`SELECT status, run FROM notebook_runs WHERE job_id = ?`, jobID).
		Scan(&status, &runJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return status, runJSON, err
}

// CancelJobInstance marks a job cancelled.
func (s *Store) CancelJobInstance(itemID, id string) error {
	res, err := s.db.Exec(
		`UPDATE job_instances SET cancelled = 1 WHERE item_id = ? AND id = ?`, itemID, id)
	if err != nil {
		return err
	}
	return oneRow(res)
}
