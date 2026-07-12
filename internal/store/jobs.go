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

// CancelJobInstance marks a job cancelled.
func (s *Store) CancelJobInstance(itemID, id string) error {
	res, err := s.db.Exec(
		`UPDATE job_instances SET cancelled = 1 WHERE item_id = ? AND id = ?`, itemID, id)
	if err != nil {
		return err
	}
	return oneRow(res)
}
