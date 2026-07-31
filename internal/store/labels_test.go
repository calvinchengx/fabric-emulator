package store

import "testing"

// LabelEventType is the audit schema's classification of a label change:
// 1 upgraded, 2 downgraded, 3 removed, 4 changed-same-order. The same-order
// case cannot be reached through the seeded taxonomy (every seeded label has
// a distinct order), so it is pinned here.
func TestLabelEventTypeClassification(t *testing.T) {
	public := &SensitivityLabel{ID: "a", Name: "Public", Order: 1}
	secret := &SensitivityLabel{ID: "b", Name: "Confidential", Order: 3}
	// A different label at the same sensitivity as `secret`.
	sibling := &SensitivityLabel{ID: "c", Name: "Confidential (EU)", Order: 3}

	for _, tc := range []struct {
		name      string
		old, next *SensitivityLabel
		want      int
	}{
		{"first label applied counts as an upgrade", nil, public, LabelUpgraded},
		{"less restrictive to more restrictive", public, secret, LabelUpgraded},
		{"more restrictive to less restrictive", secret, public, LabelDowngraded},
		{"same order", secret, sibling, LabelChangedSameOrder},
		{"cleared", secret, nil, LabelRemoved},
		{"cleared when there was none", nil, nil, LabelRemoved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := LabelEventType(tc.old, tc.next); got != tc.want {
				t.Fatalf("LabelEventType = %d, want %d", got, tc.want)
			}
		})
	}
}
