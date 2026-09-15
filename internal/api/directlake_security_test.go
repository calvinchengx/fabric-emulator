package api

// OneLake security on the Direct Lake query path.
//
// The load-bearing shape is doc 54's: TWO CALLERS, ONE QUERY, DIFFERENT
// ANSWERS. A suite that only asserted the refusals would pass against a handler
// that refused everyone, so every restriction below is asserted alongside an
// unrestricted caller reading the same table in the same run.
//
// One test here still pins WRONG behaviour, and says so: a row filter is
// refused rather than applied, because there is no engine on this path to apply
// a predicate with. That is a smaller divergence than serving unfiltered rows,
// and it is loud instead of silent — but it is a divergence, and the test exists
// so that closing it cannot go unnoticed the way `docs/07` went on calling
// T-SQL security a non-goal for two releases after it shipped.

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/pkg/onelakesec"
)

// securedLakehouse is the shared fixture: a lakehouse whose `sales` table holds
// two regions, and a semantic model bound to it in Direct Lake.
func securedLakehouse(t *testing.T, st *store.Store) (*store.Workspace, *store.Item, *store.Item) {
	t.Helper()
	ws := seedWorkspace(t, st)
	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "secured-lake"}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}
	putDirectLakeFile(t, st, ws.ID, lake.ID, "Tables/sales/part-0.parquet",
		directLakeParquet(t, []directLakeSale{{"us", 80}, {"eu", 60}}))
	putDirectLakeFile(t, st, ws.ID, lake.ID, "Tables/sales/_delta_log/00000000000000000000.json",
		[]byte(`{"add":{"path":"part-0.parquet"}}`))

	model := &store.Item{WorkspaceID: ws.ID, Type: "SemanticModel", DisplayName: "Secured Sales"}
	parts := []store.DefinitionPart{{Path: "model.bim", PayloadType: "InlineBase64",
		Payload: base64.StdEncoding.EncodeToString(directLakeModel(ws.ID, lake.ID))}}
	if err := st.CreateItem(model, parts); err != nil {
		t.Fatal(err)
	}
	// Querying needs Build on the model, which a Viewer does not inherit. These
	// tests are about what the SOURCE lets the viewer read, so Build is granted
	// here and tested on its own in executequeries_test.go.
	grantBuild(t, st, model, viewer.ID)
	return ws, lake, model
}

// grantBuild shares a semantic model for Read and Build (Explore).
func grantBuild(t *testing.T, st *store.Store, model *store.Item, principalID string) {
	t.Helper()
	if err := st.PutItemAccess(store.ItemAccess{ItemID: model.ID, PrincipalID: principalID, PrincipalType: "User",
		Permissions: []string{store.PermRead, store.PermExplore}}); err != nil {
		t.Fatal(err)
	}
}

// putRole stores one role verbatim, the way the authoring handler would.
func putRole(t *testing.T, st *store.Store, itemID, name, body string) {
	t.Helper()
	if err := st.PutOneLakeRoles(itemID, []store.OneLakeRole{
		{ItemID: itemID, Name: name, Body: []byte(body)},
	}); err != nil {
		t.Fatal(err)
	}
}

// assertPolicyNarrows fails unless the stored policy really restricts this
// principal, evaluated through the same two calls the DFS surface makes. Every
// assertion below rests on the fixture having teeth; this is where that is
// checked rather than assumed.
func assertPolicyNarrows(t *testing.T, st *store.Store, itemID, objectID string, wantAllowed bool) {
	t.Helper()
	roles, err := st.EvaluatableRoles(itemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(roles) == 0 {
		t.Fatal("fixture stored no evaluatable role")
	}
	const rel = "Tables/sales"
	entries := onelakesec.Effective(roles,
		onelakesec.Principal{ObjectID: objectID}, onelakesec.InputFor(rel))
	if allowed := onelakesec.Allows(entries, rel); allowed != wantAllowed {
		t.Fatalf("fixture allows=%v, want %v", allowed, wantAllowed)
	}
	if wantAllowed && onelakesec.Narrowing(entries, rel) == nil {
		t.Fatal("fixture grants unrestricted access; the assertion would prove nothing")
	}
}

const salesQuery = `{"queries":[{"query":"EVALUATE SUMMARIZECOLUMNS(Sales[Region], \"Total\", [Total])"}]}`

// query runs the model's one query as somebody, returning status and body.
func query(t *testing.T, a *API, p *auth.Principal, model *store.Item) (int, string) {
	t.Helper()
	w := do(a.executeQueries, p, "POST", salesQuery, map[string]string{"datasetId": model.ID})
	return w.Code, w.Body.String()
}

// A principal no role grants cannot resolve the table — while the owner, whose
// Contributor-and-above role holds ReadAll, reads all of it.
func TestDirectLakeRefusesATableNoRoleGrants(t *testing.T) {
	a, st := newAPI(t)
	_, lake, model := securedLakehouse(t, st)

	// A real policy, naming somebody else. Deny-by-default then applies to our
	// Viewer — a stronger fixture than storing no roles at all, where a reader
	// could put the outcome down to "no policy configured".
	putRole(t, st, lake.ID, "others", `{"name":"others","decisionRules":[{"effect":"Permit","permission":[
	  {"attributeName":"Path","attributeValueIncludedIn":["Tables/sales"]},
	  {"attributeName":"Action","attributeValueIncludedIn":["Read"]}]}],
	  "members":{"microsoftEntraMembers":[{"objectId":"somebody-else"}]}}`)
	assertPolicyNarrows(t, st, lake.ID, viewer.ID, false)

	code, body := query(t, a, viewer, model)
	if code != 400 || !bytes.Contains([]byte(body), []byte("can't be found")) {
		t.Errorf("viewer = %d %s, want 400 naming an unresolvable table", code, body)
	}

	// The other caller, same query, same run: a narrowing that narrowed
	// everybody would pass the assertion above on its own.
	if code, body := query(t, a, admin, model); code != 200 ||
		!bytes.Contains([]byte(body), []byte(`"Sales[Region]":"eu"`)) {
		t.Errorf("owner = %d %s, want every row", code, body)
	}
}

// A column outside the grant is reported as a column that cannot be found,
// which is how the product reports it: the object is absent from the namespace
// rather than present and forbidden.
func TestDirectLakeRefusesAColumnOutsideTheProjection(t *testing.T) {
	a, st := newAPI(t)
	_, lake, model := securedLakehouse(t, st)

	// `region` only. The model's Sales table also needs `amount`, which `Total`
	// sums, so the query cannot be answered.
	putRole(t, st, lake.ID, "region-only", fmt.Sprintf(`{"name":"region-only","decisionRules":[{"effect":"Permit",
	  "permission":[
	    {"attributeName":"Path","attributeValueIncludedIn":["Tables/sales"]},
	    {"attributeName":"Action","attributeValueIncludedIn":["Read"]}],
	  "constraints":{"columns":[{"tablePath":"/Tables/sales","columnNames":["region"],"columnEffect":"Permit","columnAction":["Read"]}]}}],
	  "members":{"microsoftEntraMembers":[{"objectId":%q}]}}`, viewer.ID))
	assertPolicyNarrows(t, st, lake.ID, viewer.ID, true)

	code, body := query(t, a, viewer, model)
	if code != 400 {
		t.Fatalf("viewer = %d %s, want 400", code, body)
	}
	// The denied column is named, and the granted one is not: a message that
	// named the table alone would not tell an author which grant to widen.
	// The body is JSON, so the message's own quotes arrive escaped.
	if !bytes.Contains([]byte(body), []byte(`column \"Amount\" can't be found`)) {
		t.Errorf("viewer = %s, want the denied column named", body)
	}

	if code, body := query(t, a, admin, model); code != 200 ||
		!bytes.Contains([]byte(body), []byte(`"[Total]":80`)) {
		t.Errorf("owner = %d %s, want the summed column", code, body)
	}
}

// A projection that covers every column the model needs is served, and serves
// the right rows. Without this the projection code could reject everything and
// the suite above would still pass.
func TestDirectLakeServesAProjectionThatCoversTheModel(t *testing.T) {
	a, st := newAPI(t)
	_, lake, model := securedLakehouse(t, st)

	putRole(t, st, lake.ID, "both-columns", fmt.Sprintf(`{"name":"both-columns","decisionRules":[{"effect":"Permit",
	  "permission":[
	    {"attributeName":"Path","attributeValueIncludedIn":["Tables/sales"]},
	    {"attributeName":"Action","attributeValueIncludedIn":["Read"]}],
	  "constraints":{"columns":[{"tablePath":"/Tables/sales","columnNames":["region","amount"],"columnEffect":"Permit","columnAction":["Read"]}]}}],
	  "members":{"microsoftEntraMembers":[{"objectId":%q}]}}`, viewer.ID))
	assertPolicyNarrows(t, st, lake.ID, viewer.ID, true)

	code, body := query(t, a, viewer, model)
	if code != 200 {
		t.Fatalf("viewer = %d %s, want 200", code, body)
	}
	for _, want := range []string{`"Sales[Region]":"us"`, `"[Total]":80`, `"Sales[Region]":"eu"`} {
		if !bytes.Contains([]byte(body), []byte(want)) {
			t.Errorf("viewer = %s, missing %q", body, want)
		}
	}
}

// A row filter is REFUSED rather than applied, and this test pins that as the
// divergence it is. Real Fabric filters: "Semantic models using Direct Lake on
// OneLake mode — RLS/CLS filtering: Yes — GA", and an empty result is
// documented as the expected outcome when a filter excludes every row. We have
// no engine on this path to evaluate a predicate with, so the choice is between
// wrong rows and no rows. When a bounded predicate evaluator lands, this test
// fails, and that failure is the instruction to assert filtered rows instead.
func TestDirectLakeRefusesARowFilterItCannotApply(t *testing.T) {
	a, st := newAPI(t)
	_, lake, model := securedLakehouse(t, st)

	putRole(t, st, lake.ID, "us-only", fmt.Sprintf(`{"name":"us-only","decisionRules":[{"effect":"Permit",
	  "permission":[
	    {"attributeName":"Path","attributeValueIncludedIn":["Tables/sales"]},
	    {"attributeName":"Action","attributeValueIncludedIn":["Read"]}],
	  "constraints":{"rows":[{"tablePath":"/Tables/sales","value":"SELECT * FROM sales WHERE region = 'us'"}]}}],
	  "members":{"microsoftEntraMembers":[{"objectId":%q}]}}`, viewer.ID))
	assertPolicyNarrows(t, st, lake.ID, viewer.ID, true)

	code, body := query(t, a, viewer, model)
	if code != 400 {
		t.Fatalf("FILTER APPLIED? viewer = %d %s — if rows are now filtered, assert the filtered "+
			"rows here and regrade the parity row", code, body)
	}
	// The refusal must say what it could not do, so an author is not left
	// guessing whether their filter was wrong or merely unsupported.
	for _, want := range []string{"row-level security", "cannot apply"} {
		if !bytes.Contains([]byte(body), []byte(want)) {
			t.Errorf("viewer = %s, missing %q", body, want)
		}
	}
	// And the unrestricted caller is untouched by somebody else's filter.
	if code, body := query(t, a, admin, model); code != 200 ||
		!bytes.Contains([]byte(body), []byte(`"Sales[Region]":"eu"`)) {
		t.Errorf("owner = %d %s, want every row", code, body)
	}
}

// An item with NO OneLake security roles is ReadAll's to open: "If OneLake
// security isn't on, Direct Lake on OneLake needs the effective identity to have
// Read and ReadAll." A Viewer inherits Read but not ReadAll, so is refused until
// ReadAll is granted — and refused again when it is revoked.
//
// This test replaced one that pinned the looser rule on purpose, so that
// tightening it would be a decision and not an accident. It was a decision:
// docs/57, stage 3.
func TestDirectLakeWithoutRolesRequiresReadAll(t *testing.T) {
	a, st := newAPI(t)
	_, lake, model := securedLakehouse(t, st)

	roles, err := st.EvaluatableRoles(lake.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(roles) != 0 {
		t.Fatal("fixture has roles; this test is about an item with none")
	}
	if code, body := query(t, a, viewer, model); code != 400 ||
		!bytes.Contains([]byte(body), []byte("cannot read the source")) {
		t.Fatalf("a Viewer without ReadAll = %d %s, want the source refused", code, body)
	}
	// The owner reads it in the same run: the refusal is the rule, not the table.
	if code, body := query(t, a, admin, model); code != 200 {
		t.Fatalf("owner = %d %s", code, body)
	}

	if err := st.PutItemAccess(store.ItemAccess{ItemID: lake.ID, PrincipalID: viewer.ID, PrincipalType: "User",
		Permissions: []string{store.PermRead}, Additional: []string{store.PermReadAll}}); err != nil {
		t.Fatal(err)
	}
	if code, body := query(t, a, viewer, model); code != 200 ||
		!bytes.Contains([]byte(body), []byte(`"Sales[Region]":"us"`)) {
		t.Fatalf("a Viewer granted ReadAll = %d %s, want every row", code, body)
	}

	if err := st.DeleteItemAccess(lake.ID, viewer.ID); err != nil {
		t.Fatal(err)
	}
	if code, _ := query(t, a, viewer, model); code != 400 {
		t.Fatalf("after the revoke = %d, want the source refused again", code)
	}
}

// A covering grant that narrows nothing serves the whole table. Without this
// the gate could be refusing every Viewer with a role, and the suite above —
// all refusals but one — would not notice.
func TestDirectLakeServesAnUnnarrowedGrant(t *testing.T) {
	a, st := newAPI(t)
	_, lake, model := securedLakehouse(t, st)

	putRole(t, st, lake.ID, "readers", fmt.Sprintf(`{"name":"readers","decisionRules":[{"effect":"Permit",
	  "permission":[
	    {"attributeName":"Path","attributeValueIncludedIn":["Tables/sales"]},
	    {"attributeName":"Action","attributeValueIncludedIn":["Read"]}]}],
	  "members":{"microsoftEntraMembers":[{"objectId":%q}]}}`, viewer.ID))

	roles, err := st.EvaluatableRoles(lake.ID)
	if err != nil {
		t.Fatal(err)
	}
	entries := onelakesec.Effective(roles,
		onelakesec.Principal{ObjectID: viewer.ID}, onelakesec.InputFor("Tables/sales"))
	if onelakesec.Narrowing(entries, "Tables/sales") != nil {
		t.Fatal("fixture narrows the viewer; this test is about a grant that does not")
	}

	code, body := query(t, a, viewer, model)
	if code != 200 {
		t.Fatalf("viewer = %d %s, want 200", code, body)
	}
	for _, want := range []string{`"Sales[Region]":"us"`, `"Sales[Region]":"eu"`, `"[Total]":60`} {
		if !bytes.Contains([]byte(body), []byte(want)) {
			t.Errorf("viewer = %s, missing %q", body, want)
		}
	}
}

// A model column that names no sourceColumn falls back to its own name, and the
// projection has to honour that fallback or it would deny a column the grant
// actually covers.
func TestDirectLakeProjectionHonoursTheSourceColumnFallback(t *testing.T) {
	a, st := newAPI(t)
	ws, lake, _ := securedLakehouse(t, st)

	// `region` carries no sourceColumn, so its source IS `region`.
	bare := fmt.Sprintf(`{
	  "name":"Bare","compatibilityLevel":1604,
	  "model":{
	    "expressions":[{"name":"DL","kind":"m","expression":"let Source = AzureStorage.DataLake(\"https://onelake.dfs.fabric.microsoft.com/%s/%s\", [HierarchicalNavigation=true]) in Source"}],
	    "tables":[{"name":"Sales","columns":[{"name":"region","dataType":"string"}],
	      "partitions":[{"name":"Sales","mode":"directLake","source":{"type":"entity","entityName":"sales","schemaName":"dbo","expressionSource":"DL"}}]}]
	  }
	}`, ws.ID, lake.ID)
	model := &store.Item{WorkspaceID: ws.ID, Type: "SemanticModel", DisplayName: "Bare"}
	parts := []store.DefinitionPart{{Path: "model.bim", PayloadType: "InlineBase64",
		Payload: base64.StdEncoding.EncodeToString([]byte(bare))}}
	if err := st.CreateItem(model, parts); err != nil {
		t.Fatal(err)
	}
	grantBuild(t, st, model, viewer.ID)

	putRole(t, st, lake.ID, "region-only", fmt.Sprintf(`{"name":"region-only","decisionRules":[{"effect":"Permit",
	  "permission":[
	    {"attributeName":"Path","attributeValueIncludedIn":["Tables/sales"]},
	    {"attributeName":"Action","attributeValueIncludedIn":["Read"]}],
	  "constraints":{"columns":[{"tablePath":"/Tables/sales","columnNames":["region"],"columnEffect":"Permit","columnAction":["Read"]}]}}],
	  "members":{"microsoftEntraMembers":[{"objectId":%q}]}}`, viewer.ID))
	assertPolicyNarrows(t, st, lake.ID, viewer.ID, true)

	w := do(a.executeQueries, viewer, "POST",
		`{"queries":[{"query":"EVALUATE Sales"}]}`, map[string]string{"datasetId": model.ID})
	if w.Code != 200 {
		t.Fatalf("viewer = %d %s, want the granted column served", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(`"Sales[region]":"us"`)) {
		t.Errorf("viewer = %s, want the region rows", w.Body.String())
	}
}

// Failing to READ the policy must fail the query, not wave it through. An error
// from the store is not "this item has no roles", and treating it as one would
// serve a secured table to somebody a policy excludes — the failure direction
// that costs the most.
func TestDirectLakeFailsWhenThePolicyCannotBeRead(t *testing.T) {
	a, st, dir := newDiskAPI(t)
	ws := seedWorkspace(t, st)
	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "secured-lake"}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}
	putDirectLakeFile(t, st, ws.ID, lake.ID, "Tables/sales/part-0.parquet",
		directLakeParquet(t, []directLakeSale{{"us", 80}, {"eu", 60}}))
	putDirectLakeFile(t, st, ws.ID, lake.ID, "Tables/sales/_delta_log/00000000000000000000.json",
		[]byte(`{"add":{"path":"part-0.parquet"}}`))
	model := &store.Item{WorkspaceID: ws.ID, Type: "SemanticModel", DisplayName: "Secured Sales"}
	parts := []store.DefinitionPart{{Path: "model.bim", PayloadType: "InlineBase64",
		Payload: base64.StdEncoding.EncodeToString(directLakeModel(ws.ID, lake.ID))}}
	if err := st.CreateItem(model, parts); err != nil {
		t.Fatal(err)
	}
	grantBuild(t, st, model, viewer.ID)

	dropTable(t, dir, "onelake_roles")
	if code, body := query(t, a, viewer, model); code == 200 {
		t.Errorf("viewer read the table with the policy unreadable: %s", body)
	}
	// The owner is not narrowed by any policy, so an unreadable one must not
	// stop them: the gate is skipped before the read, not after it.
	if code, body := query(t, a, admin, model); code != 200 {
		t.Errorf("owner = %d %s, want 200", code, body)
	}
}
