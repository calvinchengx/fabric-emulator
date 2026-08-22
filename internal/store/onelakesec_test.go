package store

import (
	"encoding/json"
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
	body := `{"name":"DefaultReader","decisionRules":[{"effect":"Permit","permission":[
      {"attributeName":"Path","attributeValueIncludedIn":["*"]},
      {"attributeName":"Action","attributeValueIncludedIn":["Read"]}]}],
      "members":{"fabricItemMembers":[{"sourcePath":"/","itemAccess":["ReadAll"]}]}}`
	if err := s.PutOneLakeRoles(it.ID, []OneLakeRole{
		{Name: "DefaultReader", Body: json.RawMessage(body)}}); err != nil {
		t.Fatal(err)
	}
	roles, err := s.EvaluatableRoles(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	holder := onelakesec.Principal{ObjectID: "anyone", ItemAccess: []string{"ReadAll"}}
	if got := onelakesec.Effective(roles, holder, onelakesec.InputTables); len(got) != 1 {
		t.Fatalf("a ReadAll holder was not admitted by DefaultReader: %v", got)
	}
	if got := onelakesec.Effective(roles, onelakesec.Principal{ObjectID: "anyone"},
		onelakesec.InputTables); len(got) != 0 {
		t.Fatalf("virtual membership admitted someone holding nothing: %v", got)
	}
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
