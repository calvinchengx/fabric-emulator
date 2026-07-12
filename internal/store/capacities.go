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

// Capacity is an assignable object only — no SKU/billing/throttling model.
type Capacity struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	SKU         string `json:"sku"`
	Region      string `json:"region"`
	State       string `json:"state"`
}

// seedCapacity inserts the default capacity if missing (idempotent).
func (s *Store) seedCapacity() error {
	_, err := s.db.Exec(`
INSERT INTO capacities (id, display_name, sku, region, state)
VALUES (?, 'Emulator Capacity', 'F64', 'local', 'Active')
ON CONFLICT(id) DO NOTHING`, DefaultCapacityID)
	return err
}

// GetCapacity fetches one capacity.
func (s *Store) GetCapacity(id string) (*Capacity, error) {
	c := &Capacity{}
	err := s.db.QueryRow(
		`SELECT id, display_name, sku, region, state FROM capacities WHERE id = ?`, id).
		Scan(&c.ID, &c.DisplayName, &c.SKU, &c.Region, &c.State)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// ListCapacities returns all capacities.
func (s *Store) ListCapacities() ([]*Capacity, error) {
	rows, err := s.db.Query(
		`SELECT id, display_name, sku, region, state FROM capacities ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Capacity
	for rows.Next() {
		c := &Capacity{}
		if err := rows.Scan(&c.ID, &c.DisplayName, &c.SKU, &c.Region, &c.State); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
