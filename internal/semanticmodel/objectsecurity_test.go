package semanticmodel

import (
	"strings"
	"testing"
)

// olsModel is the retail shape with measures, so hiding can be seen reaching
// the calculations that read what it hides.
func olsModel() (*Model, Data) {
	m, d := retailModel()
	m.Tables[0].Columns = append(m.Tables[0].Columns, Column{Name: "PostalCode"})
	for i, r := range d["Store"] {
		r["PostalCode"] = []string{"98052", "10001", "60601", "94016"}[i]
	}
	m.Tables[2].Measures = []Measure{
		{Name: "Units", Expression: "SUM('Sales'[Units])"},
		{Name: "Months", Expression: "COUNTROWS('Time')"},
		{Name: "Postcodes", Expression: "DISTINCTCOUNT(Store[PostalCode])"},
		{Name: "Bare", Expression: "[PostalCode]"},
		{Name: "Via", Expression: "[Months] + 1"},
		{Name: "Broken", Expression: `"unterminated`},
	}
	m.Tables[1].Measures = []Measure{{Name: "Clock", Expression: "1"}}
	return m, d
}

func hide(name string, perms ...TablePermission) Role {
	return Role{Name: name, TablePermissions: perms}
}

func measureNames(m *Model) []string {
	var out []string
	for _, t := range m.Tables {
		for _, ms := range t.Measures {
			out = append(out, ms.Name)
		}
	}
	return out
}

func TestAHiddenTableDoesNotExist(t *testing.T) {
	m, d := olsModel()
	om, od, err := ApplyObjectSecurity(m, d, []Role{hide("NoTime", TablePermission{Table: "time", MetadataPermission: "None"})})
	if err != nil {
		t.Fatal(err)
	}
	if om.Table("Time") != nil || od["Time"] != nil {
		t.Fatal("the hidden table is still in the model or the data")
	}
	if len(om.Relationships) != 1 || om.Relationships[0].Name != "Sales_Store" {
		t.Errorf("relationships = %+v, want only Sales_Store", om.Relationships)
	}
	// Its own measures and every measure reading it — directly or through
	// another measure — go with it; the rest stay.
	if got := strings.Join(measureNames(om), ","); got != "Units,Postcodes,Bare" {
		t.Errorf("measures = %s, want Units,Postcodes,Bare", got)
	}
	for _, q := range []string{`EVALUATE 'Time'`, `EVALUATE ROW("n", COUNTROWS('Time'))`, `EVALUATE ROW("m", [Months])`} {
		if _, err := Evaluate(om, od, q); err == nil {
			t.Errorf("%s evaluated against a hidden table", q)
		}
		if _, err := Evaluate(m, d, q); err != nil {
			t.Errorf("%s fails on the unsecured model too: %v", q, err)
		}
	}
	// The source model and data are untouched: they are shared with Write holders.
	if m.Table("Time") == nil || len(d["Time"]) != 2 || len(measureNames(m)) != 7 {
		t.Error("ApplyObjectSecurity changed its input")
	}
}

func TestAHiddenColumnDoesNotExist(t *testing.T) {
	m, d := olsModel()
	om, od, err := ApplyObjectSecurity(m, d, []Role{hide("NoPostcode",
		TablePermission{Table: "Store", ColumnPermissions: []ColumnPermission{{Column: "postalcode", MetadataPermission: "none"}}})})
	if err != nil {
		t.Fatal(err)
	}
	res, err := Evaluate(om, od, `EVALUATE 'Store'`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(res.Columns, ","), "PostalCode") || od["Store"][0]["PostalCode"] != nil {
		t.Errorf("the hidden column is still returned: %v / %v", res.Columns, od["Store"][0])
	}
	if d["Store"][0]["PostalCode"] == nil {
		t.Error("the input rows lost the column")
	}
	if got := strings.Join(measureNames(om), ","); got != "Clock,Units,Months,Via" {
		t.Errorf("measures = %s, want the two reading PostalCode (and the broken one) hidden", got)
	}
	for _, q := range []string{
		`EVALUATE SUMMARIZECOLUMNS('Store'[PostalCode], "u", SUM('Sales'[Units]))`,
		`EVALUATE ROW("p", SELECTEDVALUE('Store'[PostalCode]))`,
		`EVALUATE ROW("p", 'Store'[PostalCode])`,
	} {
		if _, err := Evaluate(om, od, q); err == nil || !strings.Contains(err.Error(), "no column") {
			t.Errorf("%s: err = %v, want the column not to exist", q, err)
		}
	}
}

// "Relationships that reference a secured column work provided the table the
// column is in is not secured."
func TestAHiddenKeyColumnStillJoins(t *testing.T) {
	m, d := olsModel()
	om, od, err := ApplyObjectSecurity(m, d, []Role{hide("NoKey",
		TablePermission{Table: "Store", ColumnPermissions: []ColumnPermission{{Column: "StoreId", MetadataPermission: "none"}}})})
	if err != nil {
		t.Fatal(err)
	}
	if om.Table("Store").Column("StoreId") != nil {
		t.Fatal("the key column is still visible")
	}
	res, err := Evaluate(om, od, `EVALUATE SUMMARIZECOLUMNS('Store'[Territory], "u", SUM('Sales'[Units]))`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, r := range res.Rows {
		got[dstr(r["Store[Territory]"])] = r["[u]"]
	}
	if dstr(got["West"]) != "50" || dstr(got["East"]) != "20" || dstr(got["Central"]) != "30" {
		t.Fatalf("units by territory through a hidden key = %v", got)
	}
}

// Across roles an object is hidden only when every role hides it; a table one
// role hides and another does not stays, and a column is hidden when each role
// hides it either on its own or with its table.
func TestObjectSecurityAcrossRoles(t *testing.T) {
	m, d := olsModel()
	noStore := hide("NoStore", TablePermission{Table: "Store", MetadataPermission: "none"})
	noPostcode := hide("NoPostcode", TablePermission{Table: "Store",
		ColumnPermissions: []ColumnPermission{{Column: "PostalCode", MetadataPermission: "none"}}})
	om, _, err := ApplyObjectSecurity(m, d, []Role{noStore, noPostcode})
	if err != nil {
		t.Fatal(err)
	}
	if s := om.Table("Store"); s == nil || s.Column("PostalCode") != nil || s.Column("Territory") == nil {
		t.Fatalf("Store = %+v, want visible without PostalCode", s)
	}
	om, _, err = ApplyObjectSecurity(m, d, []Role{noStore, {Name: "Everything"}})
	if err != nil {
		t.Fatal(err)
	}
	if s := om.Table("Store"); s == nil || s.Column("PostalCode") == nil {
		t.Fatal("a role that hides nothing did not grant what the other role hid")
	}
	if om != m {
		t.Error("nothing hidden, yet the model was copied")
	}
	// A permission set to read hides nothing.
	if om, _, _ = ApplyObjectSecurity(m, d, []Role{hide("Read", TablePermission{Table: "Store", MetadataPermission: "read",
		ColumnPermissions: []ColumnPermission{{Column: "PostalCode", MetadataPermission: "read"}}})}); om != m {
		t.Error("metadataPermission read hid something")
	}
	if om, _, _ = ApplyObjectSecurity(m, d, nil); om != m {
		t.Error("no role hid something")
	}
}

func TestRowAndObjectSecurityFromDifferentRolesIsAnError(t *testing.T) {
	m, d := olsModel()
	west := hide("West", TablePermission{Table: "Store", FilterExpression: `[Territory] = "West"`})
	noTime := hide("NoTime", TablePermission{Table: "Time", MetadataPermission: "none"})
	noCol := hide("NoCol", TablePermission{Table: "Store", ColumnPermissions: []ColumnPermission{{Column: "PostalCode", MetadataPermission: "none"}}})
	for _, roles := range [][]Role{{west, noTime}, {noTime, west}, {west, noCol}} {
		if _, _, err := ApplyObjectSecurity(m, d, roles); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
			t.Errorf("%s + %s: err = %v", roles[0].Name, roles[1].Name, err)
		}
	}
	// Both from ONE role is allowed.
	both := hide("Both", west.TablePermissions[0], noTime.TablePermissions[0])
	if _, _, err := ApplyObjectSecurity(m, d, []Role{both}); err != nil {
		t.Errorf("row and object security in one role: %v", err)
	}
	// Two filtering roles, or a filter beside a read-only permission, are not a mix.
	if _, _, err := ApplyObjectSecurity(m, d, []Role{west, west, hide("Read", TablePermission{Table: "Time", MetadataPermission: "read"})}); err != nil {
		t.Errorf("no object security at all: %v", err)
	}
}

func TestATableBetweenTwoOthersCannotBeSecured(t *testing.T) {
	chain := func(secure string) *Model {
		return &Model{
			Tables: []Table{{Name: "Region"}, {Name: "Store"}, {Name: "Sales"}},
			Relationships: []Relationship{
				{Name: "Sales_Store", FromTable: "Sales", ToTable: "Store"},
				{Name: "Store_Region", FromTable: "Store", ToTable: "Region"},
			},
			Roles: []Role{{Name: "R", TablePermissions: []TablePermission{
				{Table: "Region", FilterExpression: "TRUE()"},
				{Table: secure, MetadataPermission: "none"}}}},
		}
	}
	if err := CheckObjectSecurityChains(chain("store")); err == nil || !strings.Contains(err.Error(), "breaks the relationship chain") {
		t.Errorf("securing the middle table: err = %v", err)
	}
	for _, end := range []string{"Region", "Sales"} {
		if err := CheckObjectSecurityChains(chain(end)); err != nil {
			t.Errorf("securing the end table %s: %v", end, err)
		}
	}
	// A fact table related to two dimensions carries no filter between them.
	m, _ := olsModel()
	m.Roles = []Role{hide("NoSales", TablePermission{Table: "Sales", MetadataPermission: "none"})}
	if err := CheckObjectSecurityChains(m); err != nil {
		t.Errorf("securing a star's fact table: %v", err)
	}
}

// Independent of object security: a group column or bare column that does not
// exist is an error, never a BLANK group or a misleading hint.
func TestAMissingColumnIsAnErrorNotABlank(t *testing.T) {
	m, d := olsModel()
	for _, q := range []string{
		`EVALUATE SUMMARIZECOLUMNS('Store'[Nope], "u", SUM('Sales'[Units]))`,
		`EVALUATE SUMMARIZECOLUMNS('Nowhere'[Nope])`,
		`EVALUATE ROW("x", 'Store'[Nope])`,
	} {
		if _, err := Evaluate(m, d, q); err == nil {
			t.Errorf("%s evaluated", q)
		}
	}
}
