package store

import (
	"database/sql"
	"errors"
)

// Job instance statuses (clock-derived like operations).
const (
	JobNotStarted = "NotStarted"
	JobInProgress = "InProgress"
	JobCompleted  = "Completed"
	JobFailed     = "Failed"
	JobCancelled  = "Cancelled"
)

// JobInstance is a scheduled item job. Nothing executes — status is derived
// from the controllable clock, with Cancelled overriding everything.
type JobInstance struct {
	ID         string
	ItemID     string
	JobType    string
	InvokeType string
	CreatedAt  int64
	CompleteAt int64
	Cancelled  bool
	FailWith   string
}

// StatusAt derives the wire status at the given clock time.
func (j JobInstance) StatusAt(now int64) string {
	if j.Cancelled {
		return JobCancelled
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
	_, err := s.db.Exec(`
INSERT INTO job_instances (id, item_id, job_type, invoke_type, created_at, complete_at, cancelled, fail_with)
VALUES (?,?,?,?,?,?,?,?)`,
		j.ID, j.ItemID, j.JobType, j.InvokeType, j.CreatedAt, j.CompleteAt, j.Cancelled, j.FailWith)
	return err
}

// GetJobInstance fetches one job scoped to its item.
func (s *Store) GetJobInstance(itemID, id string) (*JobInstance, error) {
	j := &JobInstance{}
	err := s.db.QueryRow(`
SELECT id, item_id, job_type, invoke_type, created_at, complete_at, cancelled, fail_with
FROM job_instances WHERE item_id = ? AND id = ?`, itemID, id).
		Scan(&j.ID, &j.ItemID, &j.JobType, &j.InvokeType, &j.CreatedAt, &j.CompleteAt, &j.Cancelled, &j.FailWith)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
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

// CancelJobInstance marks a job cancelled.
func (s *Store) CancelJobInstance(itemID, id string) error {
	res, err := s.db.Exec(
		`UPDATE job_instances SET cancelled = 1 WHERE item_id = ? AND id = ?`, itemID, id)
	if err != nil {
		return err
	}
	return oneRow(res)
}
