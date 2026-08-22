package onelake

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/clock"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// OneLake security on the DFS surface (stage 3 of docs/54-onelake-security.md).
//
// The layer does exactly ONE thing on this path, and the tests are shaped
// around proving it is that one thing and not more:
//
//	"View files in OneLake — Admin: Always Yes. Member: Always Yes.
//	 Contributor: Always Yes. Viewer: No by default. Use OneLake security to
//	 grant the access."
//
// So a role can WIDEN a Viewer and must not NARROW anyone else. A suite that
// only showed a granted Viewer reading would pass against a surface that had
// stopped checking anything at all, so every grant here is paired with a
// refusal that must survive it.

func grantRole(t *testing.T, f *fixture, id, role string) {
	t.Helper()
	if err := f.st.CreateRoleAssignment(&store.RoleAssignment{
		WorkspaceID: f.ws.ID, Principal: store.Principal{ID: id, Type: "User"}, Role: role,
	}); err != nil {
		t.Fatal(err)
	}
}

// putRole installs a OneLake security role granting one principal one path.
func putRole(t *testing.T, f *fixture, principal, path string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"name": "readers",
		"decisionRules": []map[string]any{{
			"effect": "Permit",
			"permission": []map[string]any{
				{"attributeName": "Path", "attributeValueIncludedIn": []string{path}},
				{"attributeName": "Action", "attributeValueIncludedIn": []string{"Read"}},
			},
		}},
		"members": map[string]any{
			"microsoftEntraMembers": []map[string]string{{"objectId": principal}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.PutOneLakeRoles(f.it.ID, []store.OneLakeRole{
		{ItemID: f.it.ID, Name: "readers", Body: body}}); err != nil {
		t.Fatal(err)
	}
}

// seedFile puts a readable path in the item so a grant has something to reach.
func seedFile(t *testing.T, f *fixture, rel string) {
	t.Helper()
	if err := f.st.CreateOneLakePath(&store.OneLakePath{
		WorkspaceID: f.ws.ID, ItemID: f.it.ID, RelPath: rel, Content: []byte("x"),
	}, false); err != nil {
		t.Fatal(err)
	}
}

// The widening: a Viewer, refused by default, reads exactly what a role grants.
func TestAOneLakeRoleGrantsAViewerOnePath(t *testing.T) {
	f := newFixture(t)
	grantRole(t, f, "viewer-1", store.RoleViewer)
	seedFile(t, f, "Tables/dbo/Customers/part-0.parquet")
	seedFile(t, f, "Tables/dbo/Orders/part-0.parquet")

	base := "/" + f.ws.ID + "/" + f.it.ID + "/"
	tok := f.storageToken("viewer-1")

	// Before any role exists, the Viewer is refused — the default the product
	// documents, and the state every other item is in.
	if w := f.do("GET", base+"Tables/dbo/Customers/part-0.parquet", tok, nil); w.Code != http.StatusForbidden {
		t.Fatalf("viewer read with no roles = %d, want 403", w.Code)
	}

	putRole(t, f, "viewer-1", "Tables/dbo/Customers")

	if w := f.do("GET", base+"Tables/dbo/Customers/part-0.parquet", tok, nil); w.Code != http.StatusOK {
		t.Fatalf("granted path = %d, want 200 (%s)", w.Code, w.Body)
	}
	// THE CONTROL. A grant on one table must not reach another, or the role is
	// decorative and the surface is simply open to any Viewer.
	if w := f.do("GET", base+"Tables/dbo/Orders/part-0.parquet", tok, nil); w.Code != http.StatusForbidden {
		t.Fatalf("ungranted sibling table = %d, want 403", w.Code)
	}
}

// A grant to someone else grants nothing to you.
func TestAGrantToAnotherPrincipalDoesNotTravel(t *testing.T) {
	f := newFixture(t)
	grantRole(t, f, "viewer-1", store.RoleViewer)
	grantRole(t, f, "viewer-2", store.RoleViewer)
	seedFile(t, f, "Tables/dbo/Customers/part-0.parquet")
	putRole(t, f, "viewer-1", "Tables/dbo/Customers")

	path := "/" + f.ws.ID + "/" + f.it.ID + "/Tables/dbo/Customers/part-0.parquet"
	if w := f.do("GET", path, f.storageToken("viewer-1"), nil); w.Code != http.StatusOK {
		t.Fatalf("the named viewer = %d, want 200", w.Code)
	}
	if w := f.do("GET", path, f.storageToken("viewer-2"), nil); w.Code != http.StatusForbidden {
		t.Fatalf("an unnamed viewer = %d, want 403", w.Code)
	}
}

// "Since Workspace Admin, Member and Contributor roles automatically grant
// Write permissions to OneLake, they override any OneLake security Read
// permissions." A role scoped to one table must not shrink a Contributor.
func TestARoleDoesNotNarrowContributorsAndAbove(t *testing.T) {
	f := newFixture(t)
	grantRole(t, f, "contrib-1", store.RoleContributor)
	seedFile(t, f, "Tables/dbo/Orders/part-0.parquet")
	// The role names a different table, and a different principal.
	putRole(t, f, "viewer-1", "Tables/dbo/Customers")

	base := "/" + f.ws.ID + "/" + f.it.ID + "/"
	for _, id := range []string{"contrib-1", "admin-1"} {
		if w := f.do("GET", base+"Tables/dbo/Orders/part-0.parquet", f.storageToken(id), nil); w.Code != http.StatusOK {
			t.Errorf("%s = %d, want 200: OneLake security must not narrow ReadAll", id, w.Code)
		}
	}
}

// A principal with no workspace role at all is refused whatever the item's
// roles say: workspace permissions are "the first security boundary", and
// OneLake security narrows within an item rather than admitting a stranger.
func TestAStrangerIsNotAdmittedByAnItemRole(t *testing.T) {
	f := newFixture(t)
	seedFile(t, f, "Tables/dbo/Customers/part-0.parquet")
	putRole(t, f, "nobody-1", "Tables/dbo/Customers")

	w := f.do("GET", "/"+f.ws.ID+"/"+f.it.ID+"/Tables/dbo/Customers/part-0.parquet",
		f.storageToken("nobody-1"), nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("a non-member named in an item role = %d, want 403", w.Code)
	}
}

// This increment implements Read. A granted Viewer must not be able to write,
// rather than have a ReadWrite grant half-honoured.
func TestAGrantedViewerStillCannotWrite(t *testing.T) {
	f := newFixture(t)
	grantRole(t, f, "viewer-1", store.RoleViewer)
	seedFile(t, f, "Tables/dbo/Customers/part-0.parquet")
	putRole(t, f, "viewer-1", "Tables/dbo/Customers")

	base := "/" + f.ws.ID + "/" + f.it.ID + "/Tables/dbo/Customers/"
	tok := f.storageToken("viewer-1")
	for _, method := range []string{"PUT", "PATCH", "DELETE"} {
		if w := f.do(method, base+"new.parquet", tok, nil); w.Code != http.StatusForbidden {
			t.Errorf("%s by a granted viewer = %d, want 403", method, w.Code)
		}
	}
}

// The Viewer is still refused at the workspace level: a grant is scoped to an
// item, so it cannot confer listing the container above it.
func TestAGrantDoesNotReachTheWorkspaceLevel(t *testing.T) {
	f := newFixture(t)
	grantRole(t, f, "viewer-1", store.RoleViewer)
	putRole(t, f, "viewer-1", "*")
	if w := f.do("HEAD", "/"+f.ws.ID, f.storageToken("viewer-1"), nil); w.Code != http.StatusForbidden {
		t.Fatalf("workspace HEAD by a granted viewer = %d, want 403", w.Code)
	}
}

// A wildcard grant reaches the whole half it was written for, and not the other.
func TestAWildcardGrantIsScopedToItsHalf(t *testing.T) {
	f := newFixture(t)
	grantRole(t, f, "viewer-1", store.RoleViewer)
	seedFile(t, f, "Tables/dbo/T/part-0.parquet")
	seedFile(t, f, "Files/raw/x.csv")
	putRole(t, f, "viewer-1", "Tables")

	base := "/" + f.ws.ID + "/" + f.it.ID + "/"
	tok := f.storageToken("viewer-1")
	if w := f.do("GET", base+"Tables/dbo/T/part-0.parquet", tok, nil); w.Code != http.StatusOK {
		t.Fatalf("Tables grant = %d, want 200", w.Code)
	}
	if w := f.do("GET", base+"Files/raw/x.csv", tok, nil); w.Code != http.StatusForbidden {
		t.Fatalf("a Tables grant reached Files: %d, want 403", w.Code)
	}
}

// A failure to READ THE POLICY must refuse the request, not fall through to a
// permissive default. Deny-by-default only means anything if the "no roles"
// answer is distinguishable from the "could not ask" answer.
//
// Called directly: the decision is what is under test, and routing a request to
// it would only add ways for the test to be wrong.
func TestAuthorizeViewerRefusesWhenThePolicyCannotBeRead(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir, clock.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := &Service{Store: st}

	// A second connection drops the table out from under the service, the way
	// api_failure_test.go reaches its 500 branches.
	db, err := sql.Open("sqlite", filepath.Join(dir, "fabric-emulator.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("DROP TABLE onelake_roles"); err != nil {
		t.Fatal(err)
	}

	derr := s.authorizeViewer("some-item", "Tables/dbo/T", "viewer-1", http.MethodGet)
	if derr == nil {
		t.Fatal("an unreadable policy was treated as a decision")
	}
	if derr.status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s)", derr.status, derr.msg)
	}
}
