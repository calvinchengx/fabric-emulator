package semanticmodel

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The retail shape: Store and Time are dimensions, Sales the fact that points at
// both — so a filter on Store must reach Sales, and must not reach Time.
func retailModel(roles ...Role) (*Model, Data) {
	m := &Model{
		Tables: []Table{
			{Name: "Store", Columns: []Column{{Name: "StoreId"}, {Name: "Territory"}}},
			{Name: "Time", Columns: []Column{{Name: "MonthKey"}}},
			{Name: "Sales", Columns: []Column{{Name: "StoreId"}, {Name: "MonthKey"}, {Name: "Units"}}},
		},
		Relationships: []Relationship{
			{Name: "Sales_Store", FromTable: "Sales", FromColumn: "StoreId", ToTable: "Store", ToColumn: "StoreId"},
			{Name: "Sales_Time", FromTable: "Sales", FromColumn: "MonthKey", ToTable: "Time", ToColumn: "MonthKey"},
		},
		Roles: roles,
	}
	d := Data{
		"Store": {{"StoreId": 1.0, "Territory": "West"}, {"StoreId": 2.0, "Territory": "East"},
			{"StoreId": 3.0, "Territory": "Central"}, {"StoreId": 4.0, "Territory": "West"}},
		"Time": {{"MonthKey": 201301}, {"MonthKey": 201304}},
		"Sales": {{"StoreId": 1, "MonthKey": 201301, "Units": 10}, {"StoreId": 2, "MonthKey": 201301, "Units": 20},
			{"StoreId": 3, "MonthKey": 201304, "Units": 30}, {"StoreId": 4, "MonthKey": 201304, "Units": 40},
			{"StoreId": 99, "MonthKey": 201304, "Units": 50}, {"StoreId": nil, "MonthKey": 201304, "Units": 60}},
	}
	return m, d
}

func filterRole(name, table, expr string) Role {
	return Role{Name: name, TablePermissions: []TablePermission{{Table: table, FilterExpression: expr}}}
}

func column(rows []Row, col string) []string {
	var out []string
	for _, r := range rows {
		out = append(out, strings.TrimSuffix(strings.TrimSuffix(fmtAny(r[col]), ".0"), ""))
	}
	sort.Strings(out)
	return out
}

func fmtAny(v any) string { return strings.TrimSpace(strings.ReplaceAll(dstr(v), " ", "")) }

func apply(t *testing.T, m *Model, d Data, roles ...Role) Data {
	t.Helper()
	out, err := ApplyRowSecurity(m, d, roles, SecurityEnv{UPN: "ada@contoso.com"})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestNoRoleSeesNoRows(t *testing.T) {
	m, d := retailModel()
	out := apply(t, m, d)
	for _, table := range []string{"Store", "Time", "Sales"} {
		if rows, ok := out[table]; !ok || len(rows) != 0 {
			t.Errorf("%s = %v, want present and empty", table, rows)
		}
	}
}

// A filter on the dimension narrows the facts through the relationship, and
// leaves the unrelated dimension whole. The fact row with no matching store is
// withheld once stores are narrowed.
func TestADimensionFilterReachesTheFactsOneWay(t *testing.T) {
	m, d := retailModel()
	out := apply(t, m, d, filterRole("West", "Store", `[Territory] = "West"`))
	if got := column(out["Store"], "StoreId"); !reflect.DeepEqual(got, []string{"1", "4"}) {
		t.Errorf("Store = %v", got)
	}
	if got := column(out["Sales"], "StoreId"); !reflect.DeepEqual(got, []string{"1", "4"}) {
		t.Errorf("Sales = %v, want the West stores' rows, not store 99's nor the unassigned row", got)
	}
	if len(out["Time"]) != 2 {
		t.Errorf("Time = %v, want untouched: a filter does not flow many → one", out["Time"])
	}
}

// Filtering the facts does not narrow the dimension: the default direction is
// one → many only.
func TestAFactFilterDoesNotReachTheDimension(t *testing.T) {
	m, d := retailModel()
	out := apply(t, m, d, filterRole("Big", "Sales", `[Units] >= 30`))
	if len(out["Store"]) != 4 {
		t.Errorf("Store = %v, want all four", out["Store"])
	}
	if got := column(out["Sales"], "Units"); !reflect.DeepEqual(got, []string{"30", "40", "50", "60"}) {
		t.Errorf("Sales = %v", got)
	}
}

func TestRolesAreAdditive(t *testing.T) {
	m, d := retailModel()
	// Two roles, both filtering Store: the union of what each admits.
	out := apply(t, m, d, filterRole("West", "Store", `[Territory] = "West"`),
		filterRole("East", "Store", `[Territory] = "East"`))
	if got := column(out["Store"], "StoreId"); !reflect.DeepEqual(got, []string{"1", "2", "4"}) {
		t.Errorf("West ∪ East = %v", got)
	}
	// A role that leaves Store alone leaves it whole, whatever another role says.
	out = apply(t, m, d, filterRole("West", "Store", `[Territory] = "West"`), Role{Name: "Everything"})
	if len(out["Store"]) != 4 || len(out["Sales"]) != 6 {
		t.Errorf("with an unfiltering role: Store %d, Sales %d; want everything", len(out["Store"]), len(out["Sales"]))
	}
	// A permission with only metadata settings filters no rows.
	out = apply(t, m, d, Role{Name: "OLS only", TablePermissions: []TablePermission{{Table: "Store", MetadataPermission: "read"}}})
	if len(out["Store"]) != 4 {
		t.Errorf("a metadata-only permission narrowed rows: %v", out["Store"])
	}
}

// Security filters travel chains: narrowing the far end narrows every table
// downstream of it, not only the next one.
func TestFiltersPropagateTransitively(t *testing.T) {
	m := &Model{
		Tables: []Table{
			{Name: "Region", Columns: []Column{{Name: "RegionId"}, {Name: "Name"}}},
			{Name: "Store", Columns: []Column{{Name: "StoreId"}, {Name: "RegionId"}}},
			{Name: "Sales", Columns: []Column{{Name: "StoreId"}, {Name: "Units"}}},
		},
		Relationships: []Relationship{
			// Declared fact-first, so a single pass in declaration order would miss
			// the second hop: the fixpoint is what reaches it.
			{Name: "Sales_Store", FromTable: "Sales", FromColumn: "StoreId", ToTable: "Store", ToColumn: "StoreId"},
			{Name: "Store_Region", FromTable: "Store", FromColumn: "RegionId", ToTable: "Region", ToColumn: "RegionId"},
		},
	}
	d := Data{
		"Region": {{"RegionId": "R1", "Name": "North"}, {"RegionId": "R2", "Name": "South"}},
		"Store":  {{"StoreId": 1, "RegionId": "r1"}, {"StoreId": 2, "RegionId": "R2"}}, // keys join case-insensitively
		"Sales":  {{"StoreId": 1, "Units": 5}, {"StoreId": 2, "Units": 7}},
	}
	out := apply(t, m, d, filterRole("North", "Region", `[Name] = "North"`))
	if len(out["Store"]) != 1 || len(out["Sales"]) != 1 || fmtAny(out["Sales"][0]["Units"]) != "5" {
		t.Fatalf("Store %v, Sales %v; want the North chain only", out["Store"], out["Sales"])
	}
}

func TestAnInactiveRelationshipCarriesNoFilter(t *testing.T) {
	m, d := retailModel()
	m.Relationships[0].Inactive = true
	m.Relationships[0].SecurityFilteringBehavior = "bothDirections" // ignored: inactive
	out := apply(t, m, d, filterRole("West", "Store", `[Territory] = "West"`))
	if len(out["Sales"]) != 6 {
		t.Fatalf("Sales = %d rows through an inactive relationship, want all 5", len(out["Sales"]))
	}
}

func TestApplyRowSecurityRefusesWhatItCannotApply(t *testing.T) {
	west := filterRole("West", "Store", `[Territory] = "West"`)
	for name, tc := range map[string]struct {
		mutate func(*Model)
		roles  []Role
		want   string
	}{
		"both directions": {func(m *Model) { m.Relationships[0].SecurityFilteringBehavior = "BothDirections" },
			[]Role{west}, "both directions"},
		"many-to-many": {func(m *Model) { m.Relationships[1].FromCardinality, m.Relationships[1].ToCardinality = "many", "many" },
			[]Role{west}, "many-to-many"},
		"a filter that does not compile": {func(*Model) {}, []Role{filterRole("Bad", "Store", `RELATED(Time[MonthKey]) = 1`)},
			`role "Bad"`},
		"a filter that cannot evaluate": {func(*Model) {}, []Role{filterRole("Bad", "Store", `[Territory] = 1`)},
			"cannot compare"},
	} {
		m, d := retailModel()
		tc.mutate(m)
		if _, err := ApplyRowSecurity(m, d, tc.roles, SecurityEnv{}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

// The relationship properties row security reads are parsed from both formats;
// a parser that dropped them would propagate filters the product does not.
func TestRelationshipSecurityPropertiesAreParsed(t *testing.T) {
	tables := `"tables":[{"name":"A","columns":[{"name":"K","dataType":"int64"}]},{"name":"B","columns":[{"name":"K","dataType":"int64"}]}]`
	m, err := ParseTMSL([]byte(`{"name":"m","model":{` + tables + `,"relationships":[
	  {"name":"r1","fromTable":"A","fromColumn":"K","toTable":"B","toColumn":"K",
	   "fromCardinality":"many","toCardinality":"many","isActive":false,"securityFilteringBehavior":"bothDirections"},
	  {"name":"r2","fromTable":"A","fromColumn":"K","toTable":"B","toColumn":"K","isActive":true}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []Relationship{
		{Name: "r1", FromTable: "A", FromColumn: "K", ToTable: "B", ToColumn: "K",
			FromCardinality: "many", ToCardinality: "many", Inactive: true, SecurityFilteringBehavior: "bothDirections"},
		{Name: "r2", FromTable: "A", FromColumn: "K", ToTable: "B", ToColumn: "K"},
	}
	if !reflect.DeepEqual(m.Relationships, want) {
		t.Fatalf("TMSL relationships =\n%#v\nwant\n%#v", m.Relationships, want)
	}

	m, err = ParseTMDL(map[string][]byte{
		"definition/tables/A.tmdl": []byte("table A\n\tcolumn K\n\t\tdataType: int64\n"),
		"definition/tables/B.tmdl": []byte("table B\n\tcolumn K\n\t\tdataType: int64\n"),
		"definition/relationships.tmdl": []byte("relationship r1\n\tfromColumn: A.K\n\ttoColumn: B.K\n" +
			"\tfromCardinality: many\n\ttoCardinality: many\n\tisActive: false\n\tsecurityFilteringBehavior: bothDirections\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Relationships) != 1 || !reflect.DeepEqual(m.Relationships[0], want[0]) {
		t.Fatalf("TMDL relationships =\n%#v\nwant\n%#v", m.Relationships, want[:1])
	}
}

// The braces, && and || the filter grammar needed are new tokens to the query
// lexer; the query grammar still refuses them by name rather than misreading.
func TestQueryGrammarRefusesFilterOnlyTokens(t *testing.T) {
	m, d := retailModel()
	for _, q := range []string{`EVALUATE {1}`, `EVALUATE ROW("x", 1 && 1)`, `EVALUATE ROW("x", 1 || 1)`} {
		if _, err := Evaluate(m, d, q); err == nil {
			t.Errorf("%s: evaluated", q)
		}
	}
}
