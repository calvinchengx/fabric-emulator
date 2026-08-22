package onelakesec

import (
	"reflect"
	"strings"
	"testing"
)

// The assertions that carry weight here are REFUSALS and CONSOLIDATION.
//
// A happy-path test — "a member of a role can read the table" — passes
// identically against an evaluator that returns everything for everyone, which
// is the one implementation this package exists to rule out. So every grant is
// paired with something that must NOT be granted.

const alice = "11111111-1111-1111-1111-111111111111"
const bob = "22222222-2222-2222-2222-222222222222"

func permit(paths []string, rows string, cols []string) DecisionRule {
	return DecisionRule{Effect: EffectPermit, Paths: paths, Actions: []string{AccessRead}, Rows: rows, Columns: cols}
}

func pathsOf(es []AccessEntry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Path)
	}
	return out
}

func entry(t *testing.T, es []AccessEntry, path string) AccessEntry {
	t.Helper()
	for _, e := range es {
		if e.Path == path {
			return e
		}
	}
	t.Fatalf("no entry for %s; got %v", path, pathsOf(es))
	return AccessEntry{}
}

// Deny by default: "all users start with no access to data unless explicitly
// granted by a OneLake security role."
func TestAPrincipalInNoRoleGetsNothing(t *testing.T) {
	roles := []Role{{
		Name:          "readers",
		DecisionRules: []DecisionRule{permit([]string{"Tables/dbo/Customers"}, "", nil)},
		Members:       Members{Entra: []string{alice}},
	}}
	if got := Effective(roles, Principal{ObjectID: bob}, InputTables); len(got) != 0 {
		t.Fatalf("bob is in no role but got %v", pathsOf(got))
	}
	// And the empty result is a result, not a failure to find the role.
	if got := Effective(roles, Principal{ObjectID: alice}, InputTables); len(got) != 1 {
		t.Fatalf("alice is a member but got %v", pathsOf(got))
	}
}

// An empty principal must not match an empty member list by accident — the
// classic zero-value bug, and it would grant anonymous access to every role.
func TestTheEmptyPrincipalMatchesNothing(t *testing.T) {
	roles := []Role{{
		Name:          "empty-members",
		DecisionRules: []DecisionRule{permit([]string{"*"}, "", nil)},
		Members:       Members{Entra: []string{""}},
	}}
	if got := Effective(roles, Principal{}, InputTables); len(got) != 0 {
		t.Fatalf("the zero principal was admitted: %v", pathsOf(got))
	}
}

// DefaultReader: membership is VIRTUAL, derived from holding a permission on
// the item rather than from a stored member list. Without this a newly created
// item is unreadable, which is not what the product does.
func TestVirtualMembershipAdmitsAnyoneHoldingThePermission(t *testing.T) {
	roles := []Role{{
		Name:          "DefaultReader",
		DecisionRules: []DecisionRule{permit([]string{"*"}, "", nil)},
		Members:       Members{ItemAccess: []string{"ReadAll"}},
	}}
	// Nobody is named in the role, yet a ReadAll holder is a member.
	got := Effective(roles, Principal{ObjectID: bob, ItemAccess: []string{"ReadAll"}}, InputTables)
	if len(got) != 1 || got[0].Path != InputTables {
		t.Fatalf("ReadAll holder was not admitted: %v", pathsOf(got))
	}
	// And someone without the permission still gets nothing: the role is
	// virtualised, not open.
	if none := Effective(roles, Principal{ObjectID: bob}, InputTables); len(none) != 0 {
		t.Fatalf("a principal with no item permission was admitted: %v", pathsOf(none))
	}
}

// Scope is a path, so a role on one table must not reach another.
func TestScopeDoesNotLeakAcrossPaths(t *testing.T) {
	roles := []Role{{
		Name:          "customers-only",
		DecisionRules: []DecisionRule{permit([]string{"Tables/dbo/Customers"}, "", nil)},
		Members:       Members{Entra: []string{alice}},
	}}
	got := Effective(roles, Principal{ObjectID: alice}, InputTables)
	if want := []string{"Tables/dbo/Customers"}; !reflect.DeepEqual(pathsOf(got), want) {
		t.Fatalf("paths = %v, want %v", pathsOf(got), want)
	}
}

// inputPath selects a half of the item. A rule written for Files must not
// surface when an engine asks about Tables, or it would filter a table by a
// rule meant for a folder.
func TestFilesRulesDoNotAnswerATablesQuestion(t *testing.T) {
	roles := []Role{{
		Name: "mixed",
		DecisionRules: []DecisionRule{
			permit([]string{"Files/raw"}, "", nil),
			permit([]string{"Tables/dbo/Orders"}, "", nil),
		},
		Members: Members{Entra: []string{alice}},
	}}
	tables := pathsOf(Effective(roles, Principal{ObjectID: alice}, InputTables))
	if !reflect.DeepEqual(tables, []string{"Tables/dbo/Orders"}) {
		t.Fatalf("Tables = %v", tables)
	}
	files := pathsOf(Effective(roles, Principal{ObjectID: alice}, InputFiles))
	if !reflect.DeepEqual(files, []string{"Files/raw"}) {
		t.Fatalf("Files = %v", files)
	}
}

// A scope written relative to the item resolves under whichever half was asked
// for, and `*` means the whole half.
func TestScopeSpellings(t *testing.T) {
	for _, tc := range []struct{ scope, input, want string }{
		{"*", InputTables, "Tables"},
		{"Tables", InputTables, "Tables"},
		{"Tables/dbo/Customers", InputTables, "Tables/dbo/Customers"},
		{"/Tables/dbo/Customers", InputTables, "Tables/dbo/Customers"},
		{"dbo/Customers", InputTables, "Tables/dbo/Customers"},
		{"raw/2026", InputFiles, "Files/raw/2026"},
	} {
		roles := []Role{{
			Name:          "r",
			DecisionRules: []DecisionRule{permit([]string{tc.scope}, "", nil)},
			Members:       Members{Entra: []string{alice}},
		}}
		got := pathsOf(Effective(roles, Principal{ObjectID: alice}, tc.input))
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("scope %q under %s = %v, want [%s]", tc.scope, tc.input, got, tc.want)
		}
	}
}

// The product supports "only GRANT type roles". A Deny we accepted and ignored
// would be worse than one we refused, so it must not silently grant.
func TestANonPermitRuleGrantsNothing(t *testing.T) {
	roles := []Role{{
		Name: "denied",
		DecisionRules: []DecisionRule{
			{Effect: "Deny", Paths: []string{"Tables/dbo/Customers"}, Actions: []string{AccessRead}},
		},
		Members: Members{Entra: []string{alice}},
	}}
	if got := Effective(roles, Principal{ObjectID: alice}, InputTables); len(got) != 0 {
		t.Fatalf("a non-Permit rule granted access: %v", pathsOf(got))
	}
}

// Consolidation is a UNION across roles, because the API returns one entry per
// path. Two roles on the same table must not produce two entries.
func TestTwoRolesOnOnePathConsolidate(t *testing.T) {
	roles := []Role{
		{Name: "a", DecisionRules: []DecisionRule{permit([]string{"Tables/dbo/T"}, "", nil)},
			Members: Members{Entra: []string{alice}}},
		{Name: "b", DecisionRules: []DecisionRule{
			{Effect: EffectPermit, Paths: []string{"Tables/dbo/T"}, Actions: []string{AccessReadWrite}}},
			Members: Members{Entra: []string{alice}}},
	}
	got := Effective(roles, Principal{ObjectID: alice}, InputTables)
	if len(got) != 1 {
		t.Fatalf("want one consolidated entry, got %v", pathsOf(got))
	}
	if want := []string{AccessRead, AccessReadWrite}; !reflect.DeepEqual(got[0].Access, want) {
		t.Fatalf("access = %v, want %v", got[0].Access, want)
	}
}

// Row filters from two roles union, and — the case that matters — a role with
// NO filter erases the other's. Under a Permit-only model, adding a role must
// never take rows away.
func TestRowFiltersUnionAndAnUnrestrictedRoleWins(t *testing.T) {
	narrow := "SELECT * FROM [dbo].[T] WHERE [id] = 1"
	other := "SELECT * FROM [dbo].[T] WHERE [id] = 2"

	both := []Role{
		{Name: "a", DecisionRules: []DecisionRule{permit([]string{"Tables/dbo/T"}, narrow, nil)},
			Members: Members{Entra: []string{alice}}},
		{Name: "b", DecisionRules: []DecisionRule{permit([]string{"Tables/dbo/T"}, other, nil)},
			Members: Members{Entra: []string{alice}}},
	}
	e := entry(t, Effective(both, Principal{ObjectID: alice}, InputTables), "Tables/dbo/T")
	if !strings.Contains(e.Rows, narrow) || !strings.Contains(e.Rows, other) || !strings.Contains(e.Rows, "UNION") {
		t.Fatalf("rows = %q, want both predicates unioned", e.Rows)
	}

	withOpen := append(both, Role{
		Name: "open", DecisionRules: []DecisionRule{permit([]string{"Tables/dbo/T"}, "", nil)},
		Members: Members{Entra: []string{alice}}})
	open := entry(t, Effective(withOpen, Principal{ObjectID: alice}, InputTables), "Tables/dbo/T")
	if open.Rows != "" {
		t.Fatalf("rows = %q, want unrestricted once a role grants the table without a filter", open.Rows)
	}
}

// Columns behave the same way, and for the same reason.
func TestColumnSetsUnionAndAnUnrestrictedRoleWins(t *testing.T) {
	roles := []Role{
		{Name: "a", DecisionRules: []DecisionRule{permit([]string{"Tables/dbo/T"}, "", []string{"id"})},
			Members: Members{Entra: []string{alice}}},
		{Name: "b", DecisionRules: []DecisionRule{permit([]string{"Tables/dbo/T"}, "", []string{"name"})},
			Members: Members{Entra: []string{alice}}},
	}
	e := entry(t, Effective(roles, Principal{ObjectID: alice}, InputTables), "Tables/dbo/T")
	if want := []string{"id", "name"}; !reflect.DeepEqual(e.Columns, want) {
		t.Fatalf("columns = %v, want %v", e.Columns, want)
	}

	roles = append(roles, Role{
		Name: "all", DecisionRules: []DecisionRule{permit([]string{"Tables/dbo/T"}, "", nil)},
		Members: Members{Entra: []string{alice}}})
	open := entry(t, Effective(roles, Principal{ObjectID: alice}, InputTables), "Tables/dbo/T")
	if open.Columns != nil {
		t.Fatalf("columns = %v, want all columns once a role grants the table unnarrowed", open.Columns)
	}
}

// A role the principal is NOT in must not contribute its rules, even when
// another role admits them to the same path. Otherwise membership is decorative.
func TestRulesFromRolesYouAreNotInAreNotApplied(t *testing.T) {
	roles := []Role{
		{Name: "mine", DecisionRules: []DecisionRule{permit([]string{"Tables/dbo/T"}, "", nil)},
			Members: Members{Entra: []string{alice}}},
		{Name: "theirs", DecisionRules: []DecisionRule{permit([]string{"Tables/dbo/Secret"}, "", nil)},
			Members: Members{Entra: []string{bob}}},
	}
	got := pathsOf(Effective(roles, Principal{ObjectID: alice}, InputTables))
	if !reflect.DeepEqual(got, []string{"Tables/dbo/T"}) {
		t.Fatalf("paths = %v — a role alice is not in contributed", got)
	}
}

// Entra object IDs are case-insensitive; the API's own sample uses uppercase.
func TestEntraMatchingIsCaseInsensitive(t *testing.T) {
	roles := []Role{{
		Name:          "r",
		DecisionRules: []DecisionRule{permit([]string{"*"}, "", nil)},
		Members:       Members{Entra: []string{strings.ToUpper(alice)}},
	}}
	if got := Effective(roles, Principal{ObjectID: alice}, InputTables); len(got) != 1 {
		t.Fatalf("case-different object id did not match: %v", pathsOf(got))
	}
}

// Output is ordered, so a caller diffing two responses sees a real change
// rather than map iteration order.
func TestOutputIsDeterministic(t *testing.T) {
	roles := []Role{{
		Name: "r",
		DecisionRules: []DecisionRule{
			permit([]string{"Tables/z", "Tables/a", "Tables/m"}, "", nil)},
		Members: Members{Entra: []string{alice}},
	}}
	want := []string{"Tables/a", "Tables/m", "Tables/z"}
	for i := 0; i < 20; i++ {
		if got := pathsOf(Effective(roles, Principal{ObjectID: alice}, InputTables)); !reflect.DeepEqual(got, want) {
			t.Fatalf("iteration %d: %v", i, got)
		}
	}
}

// The same predicate from two roles is one predicate, not "X UNION X". An
// engine given the duplicate would still return the right rows, so this shows
// up as noise in a log rather than a wrong answer — which is why it needs a
// test rather than a reviewer noticing.
func TestAnIdenticalRowFilterIsNotUnionedWithItself(t *testing.T) {
	same := "SELECT * FROM [dbo].[T] WHERE [id] = 1"
	roles := []Role{
		{Name: "a", DecisionRules: []DecisionRule{permit([]string{"Tables/dbo/T"}, same, nil)},
			Members: Members{Entra: []string{alice}}},
		{Name: "b", DecisionRules: []DecisionRule{permit([]string{"Tables/dbo/T"}, same, nil)},
			Members: Members{Entra: []string{alice}}},
	}
	e := entry(t, Effective(roles, Principal{ObjectID: alice}, InputTables), "Tables/dbo/T")
	if e.Rows != same {
		t.Fatalf("rows = %q, want the predicate once", e.Rows)
	}
}

// A scope naming only the other half of the item contributes nothing, rather
// than being reinterpreted as a relative path under the requested half.
func TestABareOtherHalfScopeIsNotReinterpreted(t *testing.T) {
	roles := []Role{{
		Name:          "files-only",
		DecisionRules: []DecisionRule{permit([]string{"Files"}, "", nil)},
		Members:       Members{Entra: []string{alice}},
	}}
	if got := Effective(roles, Principal{ObjectID: alice}, InputTables); len(got) != 0 {
		t.Fatalf("a Files scope answered a Tables question: %v", pathsOf(got))
	}
}

// Coverage is segment-aware. Plain prefix matching would let a grant on
// `Tables/dbo/Cust` reach `Tables/dbo/Customers`, which is a different table.
func TestCoversIsSegmentAware(t *testing.T) {
	for _, tc := range []struct {
		entry, target string
		want          bool
	}{
		{"Tables", "Tables", true},
		{"Tables", "Tables/dbo/Customers", true},
		{"Tables/dbo/Customers", "Tables/dbo/Customers", true},
		{"Tables/dbo/Customers", "Tables/dbo/Customers/part-0.parquet", true},
		{"Tables/dbo/Customers", "Tables/dbo/Orders", false},
		{"Tables/dbo/Cust", "Tables/dbo/Customers", false},
		{"Tables/dbo/Customers", "Tables/dbo", false},
		{"Files/raw", "Tables/raw", false},
		{"", "Tables/anything", true},
	} {
		if got := Covers(tc.entry, tc.target); got != tc.want {
			t.Errorf("Covers(%q, %q) = %v, want %v", tc.entry, tc.target, got, tc.want)
		}
	}
}

func TestAllowsIsDenyByDefault(t *testing.T) {
	if Allows(nil, "Tables/dbo/Customers") {
		t.Fatal("no entries granted access")
	}
	entries := []AccessEntry{{Path: "Tables/dbo/Customers", Effect: EffectPermit}}
	if !Allows(entries, "Tables/dbo/Customers/part-0.parquet") {
		t.Fatal("a granted folder did not reach a file inside it")
	}
	if Allows(entries, "Tables/dbo/Orders") {
		t.Fatal("a grant on one table reached another")
	}
	// An entry that is not a Permit grants nothing, whatever its path.
	if Allows([]AccessEntry{{Path: "Tables", Effect: "Deny"}}, "Tables/x") {
		t.Fatal("a non-Permit entry granted access")
	}
}

func TestInputForPicksTheHalf(t *testing.T) {
	for _, tc := range []struct{ rel, want string }{
		{"Files/raw/x.csv", InputFiles},
		{"files/raw", InputFiles},
		{"Tables/dbo/T", InputTables},
		{"", InputTables},
	} {
		if got := InputFor(tc.rel); got != tc.want {
			t.Errorf("InputFor(%q) = %s, want %s", tc.rel, got, tc.want)
		}
	}
}
