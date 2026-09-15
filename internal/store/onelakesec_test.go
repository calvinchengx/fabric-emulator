package store

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/pkg/onelakesec"
)

// The store half of OneLake security: rows in, rows out, and the projection
// into the evaluator's types. The rules themselves are tested in pkg/onelakesec;
// what matters here is that a documented payload survives the round trip and
// arrives at the evaluator meaning the same thing.

func lakehouse(t *testing.T, s *Store) *Item {
	t.Helper()
	ws := &Workspace{DisplayName: "sec-ws"}
	if err := s.CreateWorkspace(ws, Principal{ID: "p", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	it := &Item{WorkspaceID: ws.ID, DisplayName: "lake", Type: "Lakehouse"}
	if err := s.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	return it
}

// The documented payload shape, from the REST reference.
const readersRole = `{
  "name": "readers",
  "decisionRules": [{
    "effect": "Permit",
    "permission": [
      {"attributeName": "Path", "attributeValueIncludedIn": ["Tables/dbo/Customers"]},
      {"attributeName": "Action", "attributeValueIncludedIn": ["Read"]}
    ]
  }],
  "members": {
    "microsoftEntraMembers": [{"objectId": "11111111-1111-1111-1111-111111111111"}]
  }
}`

func TestRolesRoundTripVerbatim(t *testing.T) {
	s := newTestStore(t)
	it := lakehouse(t, s)

	// A field we do not read must survive: the payload is an open shape and a
	// client that sent it expects to read it back.
	body := json.RawMessage(`{"name":"readers","decisionRules":[],"members":{},"tenantId":"keep-me"}`)
	if err := s.PutOneLakeRoles(it.ID, []OneLakeRole{{Name: "readers", Body: body}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListOneLakeRoles(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("roles = %d, want 1", len(got))
	}
	var back map[string]any
	if err := json.Unmarshal(got[0].Body, &back); err != nil {
		t.Fatal(err)
	}
	if back["tenantId"] != "keep-me" {
		t.Fatalf("an unread field was dropped: %s", got[0].Body)
	}
}

// PUT replaces the whole set — "creating, updating, and deleting roles to match
// the payload you send". A merge would leave behind a role the caller believes
// it deleted, which is the direction that grants access nobody asked for.
func TestPutReplacesRatherThanMerges(t *testing.T) {
	s := newTestStore(t)
	it := lakehouse(t, s)
	two := []OneLakeRole{
		{Name: "a", Body: json.RawMessage(`{"name":"a"}`)},
		{Name: "b", Body: json.RawMessage(`{"name":"b"}`)},
	}
	if err := s.PutOneLakeRoles(it.ID, two); err != nil {
		t.Fatal(err)
	}
	if err := s.PutOneLakeRoles(it.ID, two[:1]); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListOneLakeRoles(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("after replacing with one role, got %d: %+v", len(got), got)
	}
}

// The projection is where a payload becomes rules. Getting Path and Action the
// wrong way round would produce a role scoped to "Read" granting "Tables/…",
// which grants nothing and looks like a policy that simply does not work.
func TestProjectionMapsPathAndActionAttributes(t *testing.T) {
	s := newTestStore(t)
	it := lakehouse(t, s)
	if err := s.PutOneLakeRoles(it.ID, []OneLakeRole{
		{Name: "readers", Body: json.RawMessage(readersRole)}}); err != nil {
		t.Fatal(err)
	}
	roles, err := s.EvaluatableRoles(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(roles) != 1 || len(roles[0].DecisionRules) != 1 {
		t.Fatalf("roles = %+v", roles)
	}
	r := roles[0].DecisionRules[0]
	if len(r.Paths) != 1 || r.Paths[0] != "Tables/dbo/Customers" {
		t.Fatalf("paths = %v", r.Paths)
	}
	if len(r.Actions) != 1 || r.Actions[0] != onelakesec.AccessRead {
		t.Fatalf("actions = %v", r.Actions)
	}

	// And it evaluates: the named member is admitted, a stranger is not.
	alice := onelakesec.Principal{ObjectID: "11111111-1111-1111-1111-111111111111"}
	if got := onelakesec.Effective(roles, alice, onelakesec.InputTables); len(got) != 1 {
		t.Fatalf("the named member got nothing: %v", got)
	}
	stranger := onelakesec.Principal{ObjectID: "99999999-9999-9999-9999-999999999999"}
	if got := onelakesec.Effective(roles, stranger, onelakesec.InputTables); len(got) != 0 {
		t.Fatalf("a stranger was granted %v", got)
	}
}

// fabricItemMembers is the virtual-membership kind, and it is what makes
// DefaultReader work. Dropping it in the projection would leave a newly created
// item unreadable by everyone.
func TestProjectionCarriesVirtualMembership(t *testing.T) {
	s := newTestStore(t)
	it := lakehouse(t, s)
	holder := onelakesec.Principal{ObjectID: "anyone", ItemAccess: []string{"ReadAll"}}

	// The reference's own sourcePath shape — this item — in both spellings its
	// pattern allows, braces and case included.
	for _, source := range []string{
		it.WorkspaceID + "/" + it.ID,
		"{" + strings.ToUpper(it.WorkspaceID) + "}/{" + it.ID + "}",
	} {
		roles := defaultReader(t, s, it, source)
		if got := onelakesec.Effective(roles, holder, onelakesec.InputTables); len(got) != 1 {
			t.Fatalf("sourcePath %q: a ReadAll holder was not admitted by DefaultReader: %v", source, got)
		}
		if got := onelakesec.Effective(roles, onelakesec.Principal{ObjectID: "anyone"},
			onelakesec.InputTables); len(got) != 0 {
			t.Fatalf("virtual membership admitted someone holding nothing: %v", got)
		}
	}
}

// A member entry naming ANOTHER item asks about access there. Matching it
// against what the principal holds on THIS item would admit every ReadAll
// holder here to a role written for holders of ReadAll on that one — which is
// what reading members without their sourcePath did.
func TestAMemberEntryForAnotherItemConfersNothing(t *testing.T) {
	s := newTestStore(t)
	it := lakehouse(t, s)
	holder := onelakesec.Principal{ObjectID: "anyone", ItemAccess: []string{"ReadAll"}}
	for _, source := range []string{
		it.WorkspaceID + "/99999999-9999-9999-9999-999999999999",
		"/",
		"",
	} {
		roles := defaultReader(t, s, it, source)
		if got := onelakesec.Effective(roles, holder, onelakesec.InputTables); len(got) != 0 {
			t.Errorf("sourcePath %q admitted a ReadAll holder of this item: %v", source, got)
		}
	}
}

// The item is read only to check a sourcePath; if it cannot be read, the policy
// cannot be evaluated, and that is an error rather than a role without members.
func TestAnUnreadableItemFailsMembershipEvaluation(t *testing.T) {
	s := newTestStore(t)
	it := lakehouse(t, s)
	defaultReader(t, s, it, it.WorkspaceID+"/"+it.ID)
	// Renamed, not dropped: a DROP deletes every row first, and the cascade
	// would take the roles with it, so no lookup would ever happen.
	if _, err := s.db.Exec(`ALTER TABLE items RENAME TO items_elsewhere`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EvaluatableRoles(it.ID); err == nil {
		t.Fatal("a sourcePath was checked against an item that could not be read")
	}
}

func defaultReader(t *testing.T, s *Store, it *Item, source string) []onelakesec.Role {
	t.Helper()
	body := `{"name":"DefaultReader","decisionRules":[{"effect":"Permit","permission":[
      {"attributeName":"Path","attributeValueIncludedIn":["*"]},
      {"attributeName":"Action","attributeValueIncludedIn":["Read"]}]}],
      "members":{"fabricItemMembers":[{"sourcePath":"` + source + `","itemAccess":["ReadAll"]}]}}`
	if err := s.PutOneLakeRoles(it.ID, []OneLakeRole{
		{Name: "DefaultReader", Body: json.RawMessage(body)}}); err != nil {
		t.Fatal(err)
	}
	roles, err := s.EvaluatableRoles(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	return roles
}

// A malformed role must not make the item unreadable, and must not grant
// anything either. Skipping it lands on deny, which is the model's default.
func TestAMalformedRoleIsSkippedNotFatal(t *testing.T) {
	s := newTestStore(t)
	it := lakehouse(t, s)
	if err := s.PutOneLakeRoles(it.ID, []OneLakeRole{
		{Name: "broken", Body: json.RawMessage(`{"decisionRules": "not-an-array"}`)},
		{Name: "readers", Body: json.RawMessage(readersRole)},
	}); err != nil {
		t.Fatal(err)
	}
	roles, err := s.EvaluatableRoles(it.ID)
	if err != nil {
		t.Fatalf("one bad role failed the whole read: %v", err)
	}
	if len(roles) != 1 || roles[0].Name != "readers" {
		t.Fatalf("roles = %+v, want only the parseable one", roles)
	}
}

func TestARoleNeedsAName(t *testing.T) {
	s := newTestStore(t)
	it := lakehouse(t, s)
	if err := s.PutOneLakeRoles(it.ID, []OneLakeRole{
		{Body: json.RawMessage(`{}`)}}); err == nil {
		t.Fatal("a nameless role was accepted; it is the primary key")
	}
	// And the failed PUT left nothing behind.
	got, err := s.ListOneLakeRoles(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("a rejected PUT wrote %d rows", len(got))
	}
}

// Roles belong to an item, so deleting the item takes them with it rather than
// leaving policy attached to an id that can be reused.
func TestRolesCascadeWithTheItem(t *testing.T) {
	s := newTestStore(t)
	it := lakehouse(t, s)
	if err := s.PutOneLakeRoles(it.ID, []OneLakeRole{
		{Name: "readers", Body: json.RawMessage(readersRole)}}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteItem(it.WorkspaceID, it.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListOneLakeRoles(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("policy outlived its item: %+v", got)
	}
}

func TestDeleteOneLakeRolesClearsThePolicy(t *testing.T) {
	s := newTestStore(t)
	it := lakehouse(t, s)
	if err := s.PutOneLakeRoles(it.ID, []OneLakeRole{
		{Name: "readers", Body: json.RawMessage(readersRole)}}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteOneLakeRoles(it.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListOneLakeRoles(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("roles remain: %+v", got)
	}
}

// A dead connection must surface as an error from every entry point, not as an
// empty policy — because an empty policy reads as "this item has no roles",
// which is a statement about the data rather than about the database.
func TestOneLakeRolesReportStoreFailures(t *testing.T) {
	s := newTestStore(t)
	it := lakehouse(t, s)
	_ = s.Close()

	if err := s.PutOneLakeRoles(it.ID, []OneLakeRole{
		{Name: "readers", Body: json.RawMessage(readersRole)}}); err == nil {
		t.Error("PutOneLakeRoles on a closed store returned nil")
	}
	if _, err := s.ListOneLakeRoles(it.ID); err == nil {
		t.Error("ListOneLakeRoles on a closed store returned nil")
	}
	if _, err := s.EvaluatableRoles(it.ID); err == nil {
		t.Error("EvaluatableRoles on a closed store returned nil")
	}
	if err := s.DeleteOneLakeRoles(it.ID); err == nil {
		t.Error("DeleteOneLakeRoles on a closed store returned nil")
	}
}

// Policy for an item that does not exist is refused by the foreign key, rather
// than stored against an id that a later item could be assigned. Orphaned
// policy is the shape that grants access nobody authored.
func TestPolicyForAnUnknownItemIsRefused(t *testing.T) {
	s := newTestStore(t)
	err := s.PutOneLakeRoles("no-such-item", []OneLakeRole{
		{Name: "readers", Body: json.RawMessage(readersRole)}})
	if err == nil {
		t.Fatal("roles were stored against an item that does not exist")
	}
}

// The DELETE half of the replace runs before the inserts, so a failure there
// has to surface rather than leaving the old policy in place while reporting
// success — that would be a PUT that silently did nothing.
func TestPutSurfacesAFailureBeforeItWrites(t *testing.T) {
	s := newTestStore(t)
	it := lakehouse(t, s)
	if _, err := s.db.Exec(`DROP TABLE onelake_roles`); err != nil {
		t.Fatal(err)
	}
	if err := s.PutOneLakeRoles(it.ID, []OneLakeRole{
		{Name: "readers", Body: json.RawMessage(readersRole)}}); err == nil {
		t.Fatal("PutOneLakeRoles reported success with no table to write to")
	}
}

// A row that cannot be read is an error, not a skipped role. Silently dropping
// it would quietly narrow someone's access and look like a policy change.
func TestAnUnreadableRowFailsTheRead(t *testing.T) {
	s := newTestStore(t)
	it := lakehouse(t, s)
	// Recreate the table without NOT NULL so a corrupt row can exist at all,
	// then write one. Scanning NULL into a string is what a real corruption
	// would look like here.
	for _, stmt := range []string{
		`DROP TABLE onelake_roles`,
		`CREATE TABLE onelake_roles (item_id TEXT NOT NULL, name TEXT NOT NULL,
		   body TEXT, PRIMARY KEY (item_id, name))`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(
		`INSERT INTO onelake_roles (item_id, name, body) VALUES (?, 'broken', NULL)`,
		it.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListOneLakeRoles(it.ID); err == nil {
		t.Fatal("a row that cannot be scanned was reported as no error")
	}
	// EvaluatableRoles rides on the same read, so it must fail too rather than
	// returning an empty (deny-everything) policy that looks authored.
	if _, err := s.EvaluatableRoles(it.ID); err == nil {
		t.Fatal("EvaluatableRoles turned an unreadable row into an empty policy")
	}
}

// --- The documented constraints shape ------------------------------------------
//
// Row and column security arrive inside `decisionRules[].constraints`, keyed by
// table. This layer used to read flat `rows` / `columns` off the rule instead —
// a shape the reference never had — so a policy authored the documented way was
// evaluated as unrestricted. These tests author policy the way Microsoft's REST
// reference does, byte for byte where the reference gives a sample.

// evaluate stores one role body and returns the effective access for alice.
func evaluate(t *testing.T, body string) []onelakesec.AccessEntry {
	t.Helper()
	s := newTestStore(t)
	it := lakehouse(t, s)
	if err := s.PutOneLakeRoles(it.ID, []OneLakeRole{{Name: "r", Body: json.RawMessage(body)}}); err != nil {
		t.Fatal(err)
	}
	roles, err := s.EvaluatableRoles(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	return onelakesec.Effective(roles, onelakesec.Principal{ObjectID: aliceID}, onelakesec.InputTables)
}

const aliceID = "11111111-1111-1111-1111-111111111111"

// role wraps decision rules in a role naming alice.
func role(rules string) string {
	return `{"name":"r","decisionRules":[` + rules + `],
	  "members":{"microsoftEntraMembers":[{"objectId":"` + aliceID + `"}]}}`
}

const readAll = `{"attributeName":"Path","attributeValueIncludedIn":["*"]},
	  {"attributeName":"Action","attributeValueIncludedIn":["Read"]}`

// The reference's "with constraints" sample, verbatim apart from membership:
// the sample admits members through fabricItemMembers, which this test is not
// about, so alice is named instead.
func TestTheReferenceConstraintsSampleNarrows(t *testing.T) {
	got := evaluate(t, `{
  "name": "default_role_1",
  "decisionRules": [
    {
      "effect": "Permit",
      "permission": [
        {"attributeName": "Path", "attributeValueIncludedIn": ["*"]},
        {"attributeName": "Action", "attributeValueIncludedIn": ["Read"]}
      ],
      "constraints": {
        "columns": [
          {
            "tablePath": "/Tables/industrytable",
            "columnNames": ["Industry"],
            "columnEffect": "Permit",
            "columnAction": ["Read"]
          }
        ],
        "rows": [
          {
            "tablePath": "/Tables/industrytable",
            "value": "select * from Industrytable where Industry=\"Green\""
          }
        ]
      }
    }
  ],
  "members": {"microsoftEntraMembers": [{"objectId": "`+aliceID+`"}]}
}`)
	n := onelakesec.Narrowing(got, "Tables/industrytable/part-0.parquet")
	if n == nil {
		t.Fatalf("the documented constraint did not narrow: %+v", got)
	}
	if n.Rows != `select * from Industrytable where Industry="Green"` {
		t.Errorf("rows = %q", n.Rows)
	}
	if len(n.Columns) != 1 || n.Columns[0] != "Industry" {
		t.Errorf("columns = %v", n.Columns)
	}
	// The rule grants `*`; only the constrained table is narrowed.
	if onelakesec.Narrowing(got, "Tables/other/part-0.parquet") != nil {
		t.Error("a table the sample does not constrain was narrowed")
	}
}

// The reference's unconstrained and tables-path samples keep granting: strictness
// must not cost a policy that has nothing in it to be strict about.
func TestTheReferenceUnconstrainedSamplesStillGrant(t *testing.T) {
	for name, perm := range map[string]string{
		"default":     `{"attributeName":"Path","attributeValueIncludedIn":["*"]}`,
		"tables-path": `{"attributeName":"Path","attributeValueIncludedIn":["/Tables/sales","/Tables/users"]}`,
	} {
		got := evaluate(t, role(`{"effect":"Permit","permission":[`+perm+`,
		  {"attributeName":"Action","attributeValueIncludedIn":["Read"]}]}`))
		if !onelakesec.Allows(got, "Tables/sales/part-0.parquet") {
			t.Errorf("%s: the sample stopped granting: %+v", name, got)
		}
		if onelakesec.Narrowing(got, "Tables/sales/part-0.parquet") != nil {
			t.Errorf("%s: an unconstrained sample was narrowed", name)
		}
	}
}

// `columnNames: ["*"]` is "all columns", and a row constraint beside it still
// applies.
func TestAStarColumnConstraintKeepsAllColumns(t *testing.T) {
	got := evaluate(t, role(`{"effect":"Permit","permission":[`+readAll+`],
	  "constraints":{
	    "rows":[{"tablePath":"/Tables/sales","value":"select * from sales where r = 1"}],
	    "columns":[{"tablePath":"/Tables/sales","columnNames":["*"],"columnEffect":"Permit","columnAction":["Read"]}]}}`))
	n := onelakesec.Narrowing(got, "Tables/sales")
	if n == nil || n.Columns != nil || n.Rows == "" {
		t.Fatalf("narrowing = %+v, want rows filtered and every column", n)
	}
}

// Every refusal is checked against a readable twin in the same test: a parser
// that dropped EVERY constrained rule would pass the refusals alone, and would
// be a different bug — one that denies policies the product enforces.
func TestAConstraintThisParserCannotReadFailsClosed(t *testing.T) {
	const good = `{"tablePath":"/Tables/sales","columnNames":["region"],"columnEffect":"Permit","columnAction":["Read"]}`
	readable := role(`{"effect":"Permit","permission":[` + readAll + `],
	  "constraints":{"columns":[` + good + `]}}`)
	if got := evaluate(t, readable); !onelakesec.Allows(got, "Tables/sales") ||
		onelakesec.Narrowing(got, "Tables/sales") == nil {
		t.Fatalf("the readable twin did not grant-and-narrow: %+v", got)
	}

	for name, rule := range map[string]string{
		// The flat shape this layer used to read. It is not the reference's,
		// and reading it would keep a dialect no Microsoft client emits.
		"flat rows on the rule": `{"effect":"Permit","permission":[` + readAll + `],
		  "rows":"select * from sales where r = 1"}`,
		"flat columns on the rule": `{"effect":"Permit","permission":[` + readAll + `],
		  "columns":["region"]}`,
		"an unknown key on the rule": `{"effect":"Permit","permission":[` + readAll + `],
		  "filter":"anything"}`,
		"an unknown attribute": `{"effect":"Permit","permission":[` + readAll + `,
		  {"attributeName":"Region","attributeValueIncludedIn":["us"]}]}`,
		"an unknown key in a permission": `{"effect":"Permit","permission":[
		  {"attributeName":"Path","attributeValueIncludedIn":["*"],"scope":"x"},
		  {"attributeName":"Action","attributeValueIncludedIn":["Read"]}]}`,
		"constraints that are not an object": `{"effect":"Permit","permission":[` + readAll + `],
		  "constraints":"rows"}`,
		"an unknown key in constraints": `{"effect":"Permit","permission":[` + readAll + `],
		  "constraints":{"cells":[]}}`,
		"an unknown key in a row constraint": `{"effect":"Permit","permission":[` + readAll + `],
		  "constraints":{"rows":[{"tablePath":"/Tables/sales","value":"select 1","mode":"x"}]}}`,
		"a row constraint with no value": `{"effect":"Permit","permission":[` + readAll + `],
		  "constraints":{"rows":[{"tablePath":"/Tables/sales","value":" "}]}}`,
		"a row constraint with no table": `{"effect":"Permit","permission":[` + readAll + `],
		  "constraints":{"rows":[{"tablePath":"/","value":"select 1"}]}}`,
		"two row constraints on one table": `{"effect":"Permit","permission":[` + readAll + `],
		  "constraints":{"rows":[{"tablePath":"/Tables/sales","value":"select 1"},
		                         {"tablePath":"Tables/SALES","value":"select 2"}]}}`,
		"an unknown key in a column constraint": `{"effect":"Permit","permission":[` + readAll + `],
		  "constraints":{"columns":[{"tablePath":"/Tables/sales","columnNames":["region"],"columnEffect":"Permit","columnAction":["Read"],"mask":true}]}}`,
		"a column constraint with no table": `{"effect":"Permit","permission":[` + readAll + `],
		  "constraints":{"columns":[{"tablePath":"","columnNames":["region"],"columnEffect":"Permit","columnAction":["Read"]}]}}`,
		"an empty column list": `{"effect":"Permit","permission":[` + readAll + `],
		  "constraints":{"columns":[{"tablePath":"/Tables/sales","columnNames":[],"columnEffect":"Permit","columnAction":["Read"]}]}}`,
		"a non-Permit column effect": `{"effect":"Permit","permission":[` + readAll + `],
		  "constraints":{"columns":[{"tablePath":"/Tables/sales","columnNames":["region"],"columnEffect":"Deny","columnAction":["Read"]}]}}`,
		"no column action": `{"effect":"Permit","permission":[` + readAll + `],
		  "constraints":{"columns":[{"tablePath":"/Tables/sales","columnNames":["region"],"columnEffect":"Permit","columnAction":[]}]}}`,
		"a column action other than Read": `{"effect":"Permit","permission":[` + readAll + `],
		  "constraints":{"columns":[{"tablePath":"/Tables/sales","columnNames":["region"],"columnEffect":"Permit","columnAction":["Read","Write"]}]}}`,
		"two column constraints on one table": `{"effect":"Permit","permission":[` + readAll + `],
		  "constraints":{"columns":[` + good + `,` + good + `]}}`,
	} {
		if got := evaluate(t, role(rule)); onelakesec.Allows(got, "Tables/sales") {
			t.Errorf("%s: granted %+v — an unreadable restriction must deny", name, got)
		}
	}
}

// Dropping the unreadable rule leaves the role's other rules standing. That is
// safe because a constraint narrows only its own rule: the principal held
// whatever the other rules grant without this one.
func TestOnlyTheUnreadableRuleIsDropped(t *testing.T) {
	got := evaluate(t, role(`
	  {"effect":"Permit","permission":[
	    {"attributeName":"Path","attributeValueIncludedIn":["/Tables/users"]},
	    {"attributeName":"Action","attributeValueIncludedIn":["Read"]}]},
	  {"effect":"Permit","permission":[
	    {"attributeName":"Path","attributeValueIncludedIn":["/Tables/sales"]},
	    {"attributeName":"Action","attributeValueIncludedIn":["Read"]}],
	   "rows":"select * from sales where r = 1"}`))
	if !onelakesec.Allows(got, "Tables/users") {
		t.Error("a readable rule was dropped with its unreadable neighbour")
	}
	if onelakesec.Allows(got, "Tables/sales") {
		t.Error("the unreadable rule still granted its table")
	}
}

// An explicit `"constraints": null` is no constraints, not an unreadable one.
func TestANullConstraintsObjectIsNoConstraint(t *testing.T) {
	got := evaluate(t, role(`{"effect":"Permit","permission":[`+readAll+`],"constraints":null}`))
	if !onelakesec.Allows(got, "Tables/sales") || onelakesec.Narrowing(got, "Tables/sales") != nil {
		t.Fatalf("got %+v, want an unrestricted grant", got)
	}
}
