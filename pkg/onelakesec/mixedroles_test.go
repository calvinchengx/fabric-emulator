package onelakesec

import (
	"strings"
	"testing"
)

// One role filtering a table's rows and ANOTHER narrowing its columns is the
// combination OneLake security does not support. Both in one role is supported,
// and so is either beside a role granting the table whole.
func TestMixedRowAndColumnRoles(t *testing.T) {
	const sales = "Tables/dbo/sales"
	rows := Role{Name: "Rows", Members: Members{Entra: []string{alice}},
		DecisionRules: []DecisionRule{permit([]string{sales}, "SELECT * FROM sales WHERE region = 'us'", nil)}}
	cols := Role{Name: "Cols", Members: Members{Entra: []string{alice}},
		DecisionRules: []DecisionRule{permit([]string{sales}, "", []string{"region"})}}
	both := Role{Name: "Both", Members: Members{Entra: []string{alice}},
		DecisionRules: []DecisionRule{permit([]string{sales}, "SELECT * FROM sales WHERE region = 'us'", []string{"region"})}}
	whole := Role{Name: "Whole", Members: Members{Entra: []string{alice}},
		DecisionRules: []DecisionRule{permit([]string{sales}, "", nil)}}
	hrRows := Role{Name: "HRRows", Members: Members{Entra: []string{alice}},
		DecisionRules: []DecisionRule{permit([]string{"Tables/dbo/hr"}, "SELECT * FROM hr WHERE x = 1", nil)}}
	me := Principal{ObjectID: alice}

	e := entry(t, Effective([]Role{rows, cols}, me, InputTables), sales)
	if !strings.Contains(e.Unsupported, `role "Rows"`) || !strings.Contains(e.Unsupported, `role "Cols"`) {
		t.Errorf("rows + cols: Unsupported = %q", e.Unsupported)
	}
	// Both restrictions are kept, not opened by the union, so a reader that
	// cannot raise the error still applies both.
	if e.Rows == "" || len(e.Columns) != 1 {
		t.Errorf("rows + cols: rows %q columns %v, want both kept", e.Rows, e.Columns)
	}
	if n := Narrowing([]AccessEntry{e}, sales); n == nil || n.Why() != e.Unsupported {
		t.Errorf("Narrowing/Why = %v", n)
	}
	// A third role granting the table whole does not make it supported.
	if e := entry(t, Effective([]Role{rows, cols, whole}, me, InputTables), sales); e.Unsupported == "" {
		t.Error("rows + cols + whole was supported")
	}
	// A role holding both kinds beside another holding only one is still two roles.
	if e := entry(t, Effective([]Role{both, cols}, me, InputTables), sales); e.Unsupported == "" {
		t.Error("both + cols was supported")
	}
	for name, roles := range map[string][]Role{
		"both kinds in one role":       {both},
		"rows beside a whole grant":    {rows, whole},
		"columns beside a whole grant": {cols, whole},
		"rows on another table":        {cols, hrRows},
	} {
		for _, e := range Effective(roles, me, InputTables) {
			if e.Unsupported != "" {
				t.Errorf("%s: %s is unsupported: %s", name, e.Path, e.Unsupported)
			}
		}
	}
	// The union still opens what a supported combination grants whole.
	if e := entry(t, Effective([]Role{rows, whole}, me, InputTables), sales); e.Rows != "" {
		t.Errorf("rows + whole: rows %q, want unrestricted", e.Rows)
	}
}
