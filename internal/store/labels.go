package store

import (
	"database/sql"
	"errors"
)

// Sensitivity labels (fabric-docs governance/sensitivity-label-audit-schema.md
// and the bulkSetLabels / bulkRemoveLabels admin REST APIs it cites).
//
// What is faithful here: the label-change *event* model — which label was
// applied, which one it replaced, and whether the change was an upgrade, a
// downgrade, a same-order change or a removal.
//
// What is emulator-provided: the label *taxonomy* itself. In real Fabric the
// labels and their sensitivity order come from Microsoft Purview, which is
// not attachable offline, so the emulator seeds a small ordered set. Order is
// what LabelEventType is computed from, so it has to exist somewhere.
type SensitivityLabel struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Order int    `json:"order"` // higher = more restrictive
}

// LabelEventType values, verbatim from the audit schema.
const (
	LabelUpgraded          = 1
	LabelDowngraded        = 2
	LabelRemoved           = 3
	LabelChangedSameOrder  = 4
	ActionSourceManual     = 3 // "Manual" in the ActionSource enum
	ActionSourceDetailAPI  = 5 // "PublicAPI" — bulkSetLabels / bulkRemoveLabels
	ArtifactTypeFabricItem = 12
)

// seedLabels installs the emulator's label taxonomy (idempotent). The ids are
// deterministic so a restart does not invalidate labels already applied.
func (s *Store) seedLabels() error {
	for _, l := range []SensitivityLabel{
		{ID: "11111111-0000-4000-8000-00000000000a", Name: "Public", Order: 1},
		{ID: "11111111-0000-4000-8000-00000000000b", Name: "General", Order: 2},
		{ID: "11111111-0000-4000-8000-00000000000c", Name: "Confidential", Order: 3},
		{ID: "11111111-0000-4000-8000-00000000000d", Name: "Highly Confidential", Order: 4},
	} {
		if _, err := s.db.Exec(
			`INSERT INTO sensitivity_labels (id, name, sort_order) VALUES (?,?,?)
			 ON CONFLICT(id) DO NOTHING`, l.ID, l.Name, l.Order); err != nil {
			return err
		}
	}
	return nil
}

// GetLabel fetches one label from the taxonomy.
func (s *Store) GetLabel(id string) (*SensitivityLabel, error) {
	l := &SensitivityLabel{}
	err := s.db.QueryRow(
		`SELECT id, name, sort_order FROM sensitivity_labels WHERE id = ?`, id).
		Scan(&l.ID, &l.Name, &l.Order)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return l, err
}

// ListLabels returns the taxonomy, least restrictive first.
func (s *Store) ListLabels() ([]*SensitivityLabel, error) {
	rows, err := s.db.Query(`SELECT id, name, sort_order FROM sensitivity_labels ORDER BY sort_order`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*SensitivityLabel{}
	for rows.Next() {
		l := &SensitivityLabel{}
		if err := rows.Scan(&l.ID, &l.Name, &l.Order); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ItemLabel returns the label applied to an item, or ErrNotFound when the
// item carries none.
func (s *Store) ItemLabel(itemID string) (*SensitivityLabel, error) {
	var labelID string
	err := s.db.QueryRow(`SELECT label_id FROM item_labels WHERE item_id = ?`, itemID).Scan(&labelID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.GetLabel(labelID)
}

// SetItemLabel applies a label, replacing any existing one.
func (s *Store) SetItemLabel(itemID, labelID string) error {
	_, err := s.db.Exec(
		`INSERT INTO item_labels (item_id, label_id) VALUES (?,?)
		 ON CONFLICT(item_id) DO UPDATE SET label_id = excluded.label_id`, itemID, labelID)
	return err
}

// RemoveItemLabel clears an item's label. Reports whether one was present.
func (s *Store) RemoveItemLabel(itemID string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM item_labels WHERE item_id = ?`, itemID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// LabelEventType classifies a change the way the audit schema does. old may
// be nil (no previous label) and next may be nil (removal).
func LabelEventType(old, next *SensitivityLabel) int {
	switch {
	case next == nil:
		return LabelRemoved
	case old == nil || next.Order > old.Order:
		return LabelUpgraded
	case next.Order < old.Order:
		return LabelDowngraded
	default:
		return LabelChangedSameOrder
	}
}
