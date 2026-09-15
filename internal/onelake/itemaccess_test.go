package onelake

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// Item permissions on the OneLake data plane. Every grant here is paired with
// the caller it does NOT admit and with its own revoke: a grant witnessed
// without its revoke proves storage, not permission, and one witnessed without
// a refused neighbour passes against a surface that admits everyone.

func grantItem(t *testing.T, f *fixture, it *store.Item, principal string, additional ...string) {
	t.Helper()
	if err := f.st.PutItemAccess(store.ItemAccess{ItemID: it.ID, PrincipalID: principal, PrincipalType: "User",
		Permissions: []string{store.PermRead}, Additional: additional}); err != nil {
		t.Fatal(err)
	}
}

func revokeItem(t *testing.T, f *fixture, it *store.Item, principal string) {
	t.Helper()
	if err := f.st.DeleteItemAccess(it.ID, principal); err != nil {
		t.Fatal(err)
	}
}

func secondItem(t *testing.T, f *fixture) *store.Item {
	t.Helper()
	other := &store.Item{WorkspaceID: f.ws.ID, DisplayName: "other", Type: "Lakehouse"}
	if err := f.st.CreateItem(other, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.st.CreateOneLakePath(&store.OneLakePath{WorkspaceID: f.ws.ID, ItemID: other.ID,
		RelPath: "Tables/secret/part-0.parquet", Content: []byte("x")}, false); err != nil {
		t.Fatal(err)
	}
	return other
}

// "You can use item permissions to give a user access to a single item in a
// workspace that they don't have access to." Exactly one item, on both
// surfaces, and gone again on revoke.
func TestAReadAllGrantAdmitsAStrangerToOneItem(t *testing.T) {
	f := newFixture(t)
	seedFile(t, f, "Tables/sales/part-0.parquet")
	other := secondItem(t, f)
	tok := f.storageToken("stranger-1")
	granted := "/" + f.ws.ID + "/" + f.it.ID + "/Tables/sales/part-0.parquet"
	elsewhere := "/" + f.ws.ID + "/" + other.ID + "/Tables/secret/part-0.parquet"

	// The control: before any grant the stranger reaches nothing.
	if w := f.do("GET", granted, tok, nil); w.Code != http.StatusForbidden {
		t.Fatalf("before the grant = %d, want 403", w.Code)
	}

	grantItem(t, f, f.it, "stranger-1", store.PermReadAll)
	if w := f.do("GET", granted, tok, nil); w.Code != http.StatusOK {
		t.Fatalf("dfs read of the granted item = %d %s", w.Code, w.Body)
	}
	if w := f.doBlob("GET", granted, tok, nil, nil); w.Code != http.StatusOK {
		t.Fatalf("blob read of the granted item = %d %s", w.Code, w.Body)
	}
	// Nothing beyond the item: not its neighbour, not the workspace.
	if w := f.do("GET", elsewhere, tok, nil); w.Code != http.StatusForbidden {
		t.Errorf("the neighbouring item = %d, want 403", w.Code)
	}
	if w := f.do("HEAD", "/"+f.ws.ID, tok, nil); w.Code != http.StatusForbidden {
		t.Errorf("the workspace container = %d, want 403", w.Code)
	}
	if w := f.do("GET", "/"+f.ws.ID+"?resource=filesystem", tok, nil); w.Code != http.StatusForbidden {
		t.Errorf("enumerating the workspace = %d, want 403", w.Code)
	}
	// A write is still refused: sharing grants no write.
	if w := f.do("PUT", granted+"?resource=file", tok, nil); w.Code != http.StatusForbidden {
		t.Errorf("a write through the grant = %d, want 403", w.Code)
	}

	revokeItem(t, f, f.it, "stranger-1")
	if w := f.do("GET", granted, tok, nil); w.Code != http.StatusForbidden {
		t.Fatalf("after the revoke = %d, want 403", w.Code)
	}
}

// A Delta reader lists a table before it reads it, so a grant that admitted the
// read and refused the listing would still refuse the engine.
func TestAStrangerListsInsideTheGrantedItemOnly(t *testing.T) {
	f := newFixture(t)
	seedFile(t, f, "Tables/sales/part-0.parquet")
	other := secondItem(t, f)
	tok := f.storageToken("stranger-1")
	dfsList := func(dir string) (int, string) {
		w := f.do("GET", "/"+f.ws.ID+"?resource=filesystem&recursive=true&directory="+dir, tok, nil)
		return w.Code, w.Body.String()
	}
	blobList := func(prefix string) (int, string) {
		w := f.doBlob("GET", "/"+f.ws.ID+"?restype=container&comp=list&prefix="+prefix, tok, nil, nil)
		return w.Code, w.Body.String()
	}

	if code, _ := dfsList(f.it.ID + "/Tables"); code != http.StatusForbidden {
		t.Fatalf("listing before the grant = %d, want 403", code)
	}
	grantItem(t, f, f.it, "stranger-1", store.PermReadAll)
	if code, body := dfsList(f.it.ID + "/Tables"); code != http.StatusOK || !strings.Contains(body, "part-0.parquet") {
		t.Fatalf("dfs listing inside the granted item = %d %s", code, body)
	}
	if code, body := blobList("lake.Lakehouse/Tables"); code != http.StatusOK || !strings.Contains(body, "part-0.parquet") {
		t.Fatalf("blob listing inside the granted item = %d %s", code, body)
	}
	if code, _ := dfsList(other.ID + "/Tables"); code != http.StatusForbidden {
		t.Errorf("dfs listing inside another item = %d, want 403", code)
	}
	if code, _ := blobList("other.Lakehouse/Tables"); code != http.StatusForbidden {
		t.Errorf("blob listing inside another item = %d, want 403", code)
	}
	if code, _ := dfsList("no-such-item/Tables"); code != http.StatusForbidden {
		t.Errorf("listing inside an unknown item = %d, want 403", code)
	}
}

// A Viewer has no ReadAll; when an item has no OneLake security roles, a ReadAll
// grant is exactly what admits them — and the revoke takes it away while leaving
// the Viewer role in place.
func TestAReadAllGrantAdmitsAViewerToAnItemWithoutRoles(t *testing.T) {
	f := newFixture(t)
	grantRole(t, f, "viewer-1", store.RoleViewer)
	grantRole(t, f, "viewer-2", store.RoleViewer)
	seedFile(t, f, "Tables/sales/part-0.parquet")
	path := "/" + f.ws.ID + "/" + f.it.ID + "/Tables/sales/part-0.parquet"

	grantItem(t, f, f.it, "viewer-1", store.PermReadAll)
	if w := f.do("GET", path, f.storageToken("viewer-1"), nil); w.Code != http.StatusOK {
		t.Fatalf("the granted Viewer = %d %s", w.Code, w.Body)
	}
	// Same role, same request, no grant: refused.
	if w := f.do("GET", path, f.storageToken("viewer-2"), nil); w.Code != http.StatusForbidden ||
		!strings.Contains(w.Body.String(), "ReadAll") {
		t.Fatalf("the ungranted Viewer = %d %s, want a ReadAll refusal", w.Code, w.Body)
	}
	// The workspace listing shows the granted Viewer this item's files.
	w := f.do("GET", "/"+f.ws.ID+"?resource=filesystem&recursive=true", f.storageToken("viewer-1"), nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "part-0.parquet") {
		t.Fatalf("the granted Viewer's listing = %d %s", w.Code, w.Body)
	}

	revokeItem(t, f, f.it, "viewer-1")
	if w := f.do("GET", path, f.storageToken("viewer-1"), nil); w.Code != http.StatusForbidden {
		t.Fatalf("after the revoke = %d, want 403", w.Code)
	}
}

// Once an item HAS OneLake security roles, the roles decide: ReadAll reads only
// through a role that admits ReadAll holders — DefaultReader — and only when its
// sourcePath names this item.
func TestWithRolesOnReadAllReadsThroughDefaultReaderOnly(t *testing.T) {
	f := newFixture(t)
	grantRole(t, f, "viewer-1", store.RoleViewer)
	seedFile(t, f, "Tables/sales/part-0.parquet")
	grantItem(t, f, f.it, "viewer-1", store.PermReadAll)
	path := "/" + f.ws.ID + "/" + f.it.ID + "/Tables/sales/part-0.parquet"
	tok := f.storageToken("viewer-1")

	// A policy that names somebody else. ReadAll alone no longer reads.
	putRole(t, f, "somebody-else", "Tables/sales")
	if w := f.do("GET", path, tok, nil); w.Code != http.StatusForbidden {
		t.Fatalf("ReadAll under a policy with no DefaultReader = %d, want 403", w.Code)
	}

	defaultReader := func(source string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{
			"name": "DefaultReader",
			"decisionRules": []map[string]any{{"effect": "Permit", "permission": []map[string]any{
				{"attributeName": "Path", "attributeValueIncludedIn": []string{"*"}},
				{"attributeName": "Action", "attributeValueIncludedIn": []string{"Read"}}}}},
			"members": map[string]any{"fabricItemMembers": []map[string]any{
				{"sourcePath": source, "itemAccess": []string{"ReadAll"}}}},
		})
		if err := f.st.PutOneLakeRoles(f.it.ID, []store.OneLakeRole{{ItemID: f.it.ID, Name: "DefaultReader", Body: body}}); err != nil {
			t.Fatal(err)
		}
	}
	defaultReader(f.ws.ID + "/" + f.it.ID)
	if w := f.do("GET", path, tok, nil); w.Code != http.StatusOK {
		t.Fatalf("ReadAll through DefaultReader = %d %s", w.Code, w.Body)
	}
	// Named for another item, the same role admits nobody here.
	defaultReader(f.ws.ID + "/99999999-9999-9999-9999-999999999999")
	if w := f.do("GET", path, tok, nil); w.Code != http.StatusForbidden {
		t.Fatalf("DefaultReader for another item = %d, want 403", w.Code)
	}
}

// principalAccess answers for a stranger holding a grant, from the same decision
// the storage surface uses — so an engine is never told something the storage
// surface would contradict.
func TestPrincipalAccessAnswersForAGrantee(t *testing.T) {
	f := newFixture(t)
	grantRole(t, f, "engine-1", store.RoleMember)
	if got, code := askAccess(t, f, "engine-1", "stranger-1", "Tables"); code != http.StatusOK || len(got.Value) != 0 {
		t.Fatalf("before the grant = %d %+v, want no access", code, got)
	}
	grantItem(t, f, f.it, "stranger-1", store.PermReadAll)
	got, code := askAccess(t, f, "engine-1", "stranger-1", "Tables")
	if code != http.StatusOK || len(got.Value) != 1 || got.Value[0].Path != "Tables" || got.Value[0].Rows != "" {
		t.Fatalf("after the grant = %d %+v, want the whole half", code, got)
	}
	// A grant of Read without ReadAll is not OneLake access.
	grantItem(t, f, f.it, "stranger-1", store.PermReadData)
	if got, _ := askAccess(t, f, "engine-1", "stranger-1", "Tables"); len(got.Value) != 0 {
		t.Fatalf("Read and ReadData only = %+v, want no OneLake access", got)
	}
}

// Deciding whether a stranger may list reads their grant; a store that cannot
// answer is a 500 on both surfaces, never a listing and never a plain refusal
// that reads as "you have no access".
func TestAStrangerListingFailsWhenAccessCannotBeRead(t *testing.T) {
	dir := t.TempDir()
	f := newFixtureIn(t, dir)
	seedFile(t, f, "Tables/sales/part-0.parquet")
	db, err := sql.Open("sqlite", filepath.Join(dir, "fabric-emulator.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("DROP TABLE item_access"); err != nil {
		t.Fatal(err)
	}
	tok := f.storageToken("stranger-1")
	if w := f.do("GET", "/"+f.ws.ID+"?resource=filesystem&directory="+f.it.ID+"/Tables", tok, nil); w.Code != http.StatusInternalServerError {
		t.Errorf("dfs = %d, want 500", w.Code)
	}
	if w := f.doBlob("GET", "/"+f.ws.ID+"?restype=container&comp=list&prefix=lake.Lakehouse/Tables", tok, nil, nil); w.Code != http.StatusInternalServerError {
		t.Errorf("blob = %d, want 500", w.Code)
	}
}

// The listing filter resolves the item, then decides. A policy that cannot be
// read AFTER the item resolves closes the filter and reports it, rather than
// listing the item as though it had no policy.
func TestTheFilterClosesWhenTheDecisionCannotBeMade(t *testing.T) {
	dir := t.TempDir()
	f := newFixtureIn(t, dir)
	grantRole(t, f, "viewer-1", store.RoleViewer)
	db, err := sql.Open("sqlite", filepath.Join(dir, "fabric-emulator.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("DROP TABLE onelake_roles"); err != nil {
		t.Fatal(err)
	}
	fl := f.svc.newViewerFilter("viewer-1")
	if fl.allows(f.it.ID, "Tables/sales/part-0.parquet") {
		t.Error("a path was allowed while the decision could not be made")
	}
	if fl.Err() == nil {
		t.Error("the failure was not reported")
	}
}
