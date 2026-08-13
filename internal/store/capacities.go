package store

import (
	"database/sql"
	"errors"
)

// DefaultCapacityID is the deterministic seeded capacity every instance
// boots with (docs/02, ## Capacities). Workspaces created without an
// explicit capacityId are auto-assigned to it, so tooling that refuses
// capacity-less workspaces (fabric-cicd) works out of the box.
const DefaultCapacityID = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"

// DefaultMaxConcurrentJobs is high enough that an unmodified instance never
// queues: every job is admitted, matching the emulator before this column
// existed. Tests set it to 1 to exercise saturation (docs/36).
const DefaultMaxConcurrentJobs = 999

// Capacity is an assignable object. MaxConcurrentJobs is emulator admission
// control (docs/36), not a Fabric REST field — it is omitted from the wire.
type Capacity struct {
	ID                string `json:"id"`
	DisplayName       string `json:"displayName"`
	SKU               string `json:"sku"`
	Region            string `json:"region"`
	State             string `json:"state"`
	MaxConcurrentJobs int    `json:"-"`
}

// seedCapacity inserts the default capacity if missing (idempotent).
func (s *Store) seedCapacity() error {
	_, err := s.db.Exec(`
INSERT INTO capacities (id, display_name, sku, region, state, max_concurrent_jobs)
VALUES (?, 'Emulator Capacity', 'F64', 'local', 'Active', ?)
ON CONFLICT(id) DO NOTHING`, DefaultCapacityID, DefaultMaxConcurrentJobs)
	return err
}

// GetCapacity fetches one capacity.
func (s *Store) GetCapacity(id string) (*Capacity, error) {
	c := &Capacity{}
	err := s.db.QueryRow(
		`SELECT id, display_name, sku, region, state, max_concurrent_jobs FROM capacities WHERE id = ?`, id).
		Scan(&c.ID, &c.DisplayName, &c.SKU, &c.Region, &c.State, &c.MaxConcurrentJobs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// ListCapacities returns all capacities.
func (s *Store) ListCapacities() ([]*Capacity, error) {
	rows, err := s.db.Query(
		`SELECT id, display_name, sku, region, state, max_concurrent_jobs FROM capacities ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Capacity
	for rows.Next() {
		c := &Capacity{}
		if err := rows.Scan(&c.ID, &c.DisplayName, &c.SKU, &c.Region, &c.State, &c.MaxConcurrentJobs); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetCapacityMaxConcurrentJobs overrides the admission ceiling. A test sets
// this to 1 to get deterministic queueing; 0 is treated as unlimited.
func (s *Store) SetCapacityMaxConcurrentJobs(id string, n int) error {
	res, err := s.db.Exec(`UPDATE capacities SET max_concurrent_jobs = ? WHERE id = ?`, n, id)
	if err != nil {
		return err
	}
	return oneRow(res)
}
