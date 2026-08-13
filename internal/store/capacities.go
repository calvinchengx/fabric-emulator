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

// Capacity sources. Seed is local and always present. ARM rows arrive from
// arm-emulator's family feed when FABRIC_ARM_URL is set.
const (
	CapacitySourceSeed = "seed"
	CapacitySourceARM  = "arm"
)

// Capacity is an assignable object. Source and ARMID are how this process
// remembers which rows came from ARM so a vanished ARM capacity can be
// dropped without touching the seeded default. MaxConcurrentJobs is emulator
// admission control (docs/36), not a Fabric REST field. None of those three
// are on the wire.
type Capacity struct {
	ID                string `json:"id"`
	DisplayName       string `json:"displayName"`
	SKU               string `json:"sku"`
	Region            string `json:"region"`
	State             string `json:"state"`
	Source            string `json:"-"`
	ARMID             string `json:"-"`
	MaxConcurrentJobs int    `json:"-"`
}

const capacityCols = `id, display_name, sku, region, state, source, arm_id, max_concurrent_jobs`

func scanCapacity(row interface{ Scan(...any) error }) (*Capacity, error) {
	c := &Capacity{}
	err := row.Scan(&c.ID, &c.DisplayName, &c.SKU, &c.Region, &c.State, &c.Source, &c.ARMID, &c.MaxConcurrentJobs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// seedCapacity inserts the default capacity if missing (idempotent).
func (s *Store) seedCapacity() error {
	_, err := s.db.Exec(`
INSERT INTO capacities (id, display_name, sku, region, state, source, arm_id, max_concurrent_jobs)
VALUES (?, 'Emulator Capacity', 'F64', 'local', 'Active', ?, '', ?)
ON CONFLICT(id) DO NOTHING`, DefaultCapacityID, CapacitySourceSeed, DefaultMaxConcurrentJobs)
	return err
}

// PutCapacity creates or updates a capacity. A zero MaxConcurrentJobs on
// insert becomes DefaultMaxConcurrentJobs; an existing row's ceiling is
// left alone so an ARM refresh does not reset a test-set admission limit.
func (s *Store) PutCapacity(c *Capacity) error {
	if c.Source == "" {
		c.Source = CapacitySourceSeed
	}
	max := c.MaxConcurrentJobs
	if max == 0 {
		max = DefaultMaxConcurrentJobs
	}
	_, err := s.db.Exec(`
INSERT INTO capacities (`+capacityCols+`) VALUES (?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET display_name = excluded.display_name,
	sku = excluded.sku, region = excluded.region, state = excluded.state,
	source = excluded.source, arm_id = excluded.arm_id`,
		c.ID, c.DisplayName, c.SKU, c.Region, c.State, c.Source, c.ARMID, max)
	return err
}

// GetCapacity fetches one capacity.
func (s *Store) GetCapacity(id string) (*Capacity, error) {
	return scanCapacity(s.db.QueryRow(
		`SELECT `+capacityCols+` FROM capacities WHERE id = ?`, id))
}

// ListCapacities returns all capacities.
func (s *Store) ListCapacities() ([]*Capacity, error) {
	rows, err := s.db.Query(`SELECT ` + capacityCols + ` FROM capacities ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Capacity
	for rows.Next() {
		c, err := scanCapacity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteCapacity removes a capacity; ErrNotFound when absent. The seeded
// default cannot be deleted — ARM vanishing a capacity must not take the
// standalone fallback with it.
func (s *Store) DeleteCapacity(id string) error {
	if id == DefaultCapacityID {
		return nil
	}
	res, err := s.db.Exec(`DELETE FROM capacities WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
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
