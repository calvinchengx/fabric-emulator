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

// Capacity sources. Seed is local and always present. ARM rows arrive from
// arm-emulator's family feed when FABRIC_ARM_URL is set.
const (
	CapacitySourceSeed = "seed"
	CapacitySourceARM  = "arm"
)

// Capacity is an assignable object only — no SKU/billing/throttling model.
// Source and ARMID are not Fabric REST fields; they are how this process
// remembers which rows came from ARM so a vanished ARM capacity can be
// dropped without touching the seeded default.
type Capacity struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	SKU         string `json:"sku"`
	Region      string `json:"region"`
	State       string `json:"state"`
	Source      string `json:"-"`
	ARMID       string `json:"-"`
}

const capacityCols = `id, display_name, sku, region, state, source, arm_id`

func scanCapacity(row interface{ Scan(...any) error }) (*Capacity, error) {
	c := &Capacity{}
	err := row.Scan(&c.ID, &c.DisplayName, &c.SKU, &c.Region, &c.State, &c.Source, &c.ARMID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// seedCapacity inserts the default capacity if missing (idempotent).
func (s *Store) seedCapacity() error {
	_, err := s.db.Exec(`
INSERT INTO capacities (id, display_name, sku, region, state, source, arm_id)
VALUES (?, 'Emulator Capacity', 'F64', 'local', 'Active', ?, '')
ON CONFLICT(id) DO NOTHING`, DefaultCapacityID, CapacitySourceSeed)
	return err
}

// PutCapacity creates or updates a capacity.
func (s *Store) PutCapacity(c *Capacity) error {
	if c.Source == "" {
		c.Source = CapacitySourceSeed
	}
	_, err := s.db.Exec(`
INSERT INTO capacities (`+capacityCols+`) VALUES (?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET display_name = excluded.display_name,
	sku = excluded.sku, region = excluded.region, state = excluded.state,
	source = excluded.source, arm_id = excluded.arm_id`,
		c.ID, c.DisplayName, c.SKU, c.Region, c.State, c.Source, c.ARMID)
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
