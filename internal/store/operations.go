package store

import (
	"database/sql"
	"errors"
)

// CreateOperation records a new LRO. CompleteAt is when (on the emulator
// clock) the operation flips from Running to its terminal state.
func (s *Store) CreateOperation(op *Operation) error {
	op.CreatedAt = s.Now()
	if op.ID == "" {
		op.ID = NewID()
	}
	if op.CompleteAt == 0 {
		op.CompleteAt = op.CreatedAt
	}
	_, err := s.db.Exec(
		`INSERT INTO operations (id, kind, created_at, complete_at, result_ref, fail_with) VALUES (?,?,?,?,?,?)`,
		op.ID, op.Kind, op.CreatedAt, op.CompleteAt, op.ResultRef, op.FailWith)
	return err
}

// GetOperation fetches one operation.
func (s *Store) GetOperation(id string) (*Operation, error) {
	op := &Operation{}
	err := s.db.QueryRow(
		`SELECT id, kind, created_at, complete_at, result_ref, fail_with FROM operations WHERE id = ?`, id).
		Scan(&op.ID, &op.Kind, &op.CreatedAt, &op.CompleteAt, &op.ResultRef, &op.FailWith)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return op, err
}
