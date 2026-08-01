package store

import (
	"database/sql"
	"errors"
)

// MaxSchedulesPerItem is Fabric's documented ceiling on an item's schedules;
// exceeding it is the `ScheduleExceedsLimit` error.
const MaxSchedulesPerItem = 20

// Invoke types on a job instance. Fabric distinguishes how a run started, and
// clients branch on it — a scheduled run is not a manual one.
const (
	InvokeManual         = "Manual"
	InvokeScheduled      = "Scheduled"
	InvokeEventTriggered = "EventTriggered"
)

// ItemSchedule is one entry of an item's job schedule (Fabric's ItemSchedule
// resource). Configuration is the caller's ScheduleConfig JSON, kept verbatim
// so GET returns what POST sent.
type ItemSchedule struct {
	ID            string
	WorkspaceID   string
	ItemID        string
	JobType       string
	Enabled       bool
	Configuration string
	ExecutionData string
	OwnerID       string
	OwnerType     string
	CreatedAt     int64
	// FiredThrough is the newest occurrence already materialised as a job
	// instance. Evaluation fires the half-open window (FiredThrough, now], so
	// no occurrence is ever run twice.
	FiredThrough int64
}

const scheduleCols = `id, workspace_id, item_id, job_type, enabled, configuration,
	execution_data, owner_id, owner_type, created_at, fired_through`

func scanSchedule(row interface{ Scan(...any) error }) (*ItemSchedule, error) {
	s := &ItemSchedule{}
	err := row.Scan(&s.ID, &s.WorkspaceID, &s.ItemID, &s.JobType, &s.Enabled, &s.Configuration,
		&s.ExecutionData, &s.OwnerID, &s.OwnerType, &s.CreatedAt, &s.FiredThrough)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return s, err
}

// CountItemSchedules counts an item's schedules for one job type — the number
// MaxSchedulesPerItem is enforced against.
func (s *Store) CountItemSchedules(itemID, jobType string) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM item_schedules WHERE item_id = ? AND job_type = ?`, itemID, jobType).Scan(&n)
	return n, err
}

// CreateItemSchedule stores a schedule.
func (s *Store) CreateItemSchedule(sc *ItemSchedule) error {
	if sc.ID == "" {
		sc.ID = NewID()
	}
	if sc.OwnerType == "" {
		sc.OwnerType = "User"
	}
	sc.CreatedAt = s.Now()
	_, err := s.db.Exec(`
INSERT INTO item_schedules (`+scheduleCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		sc.ID, sc.WorkspaceID, sc.ItemID, sc.JobType, sc.Enabled, sc.Configuration,
		sc.ExecutionData, sc.OwnerID, sc.OwnerType, sc.CreatedAt, sc.FiredThrough)
	return err
}

// GetItemSchedule fetches one schedule scoped to its item and job type — the
// same scoping the URL carries, so a schedule id from another item's job type
// is a 404 rather than a cross-item read.
func (s *Store) GetItemSchedule(itemID, jobType, id string) (*ItemSchedule, error) {
	return scanSchedule(s.db.QueryRow(`SELECT `+scheduleCols+`
FROM item_schedules WHERE item_id = ? AND job_type = ? AND id = ?`, itemID, jobType, id))
}

// ListItemSchedules returns an item's schedules for one job type, oldest first.
func (s *Store) ListItemSchedules(itemID, jobType string) ([]*ItemSchedule, error) {
	rows, err := s.db.Query(`SELECT `+scheduleCols+`
FROM item_schedules WHERE item_id = ? AND job_type = ? ORDER BY created_at, id`, itemID, jobType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectSchedules(rows)
}

// ListEnabledSchedulesForItem returns one item's enabled schedules across all
// job types — the working set when an item-scoped read triggers evaluation.
func (s *Store) ListEnabledSchedulesForItem(itemID string) ([]*ItemSchedule, error) {
	rows, err := s.db.Query(`SELECT `+scheduleCols+`
FROM item_schedules WHERE item_id = ? AND enabled = 1 ORDER BY created_at, id`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectSchedules(rows)
}

// ListEnabledSchedules returns every enabled schedule in the tenant — the
// working set of a scheduler evaluation.
func (s *Store) ListEnabledSchedules() ([]*ItemSchedule, error) {
	rows, err := s.db.Query(`SELECT ` + scheduleCols + `
FROM item_schedules WHERE enabled = 1 ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectSchedules(rows)
}

func collectSchedules(rows *sql.Rows) ([]*ItemSchedule, error) {
	var out []*ItemSchedule
	for rows.Next() {
		sc, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// UpdateItemSchedule replaces the mutable fields of a schedule (PATCH replaces
// the whole configuration, as the reference's update payload does).
func (s *Store) UpdateItemSchedule(sc *ItemSchedule) error {
	res, err := s.db.Exec(`
UPDATE item_schedules SET enabled = ?, configuration = ?, execution_data = ?, fired_through = ?
WHERE item_id = ? AND job_type = ? AND id = ?`,
		sc.Enabled, sc.Configuration, sc.ExecutionData, sc.FiredThrough, sc.ItemID, sc.JobType, sc.ID)
	if err != nil {
		return err
	}
	return oneRow(res)
}

// SetScheduleFiredThrough advances a schedule's high-water mark.
func (s *Store) SetScheduleFiredThrough(id string, through int64) error {
	res, err := s.db.Exec(`UPDATE item_schedules SET fired_through = ? WHERE id = ?`, through, id)
	if err != nil {
		return err
	}
	return oneRow(res)
}

// DeleteItemSchedule removes a schedule.
func (s *Store) DeleteItemSchedule(itemID, jobType, id string) error {
	res, err := s.db.Exec(
		`DELETE FROM item_schedules WHERE item_id = ? AND job_type = ? AND id = ?`, itemID, jobType, id)
	if err != nil {
		return err
	}
	return oneRow(res)
}
