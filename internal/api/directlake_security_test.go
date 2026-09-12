package api

// Direct Lake and OneLake security: a hole, pinned so that closing it is
// noticed.
//
// EVERY ASSERTION HERE IS OF THE WRONG BEHAVIOUR, ON PURPOSE. The parity map
// grades Direct Lake security 🔴, and that grade rests on nothing but somebody
// having read the code — which is the weakest evidence this repo accepts, and
// the kind that silently goes stale. A negative claim needs a witness as much
// as a positive one does: without this file, wiring `pkg/onelakesec` into the
// query path would leave the parity row saying "not implemented" and no test
// would object, exactly as `docs/07` went on calling T-SQL security a non-goal
// for two releases after it shipped.
//
// So these tests PASS today and are meant to FAIL the moment the gap closes.
// A failure here is not a regression; it is the signal to flip the parity rows
// from 🔴 to 🟢 and delete this file. The fixture and the assertions are
// written to make that flip mechanical: each test asserts the policy is real
// and says "deny" or "narrow", then asserts the DAX path answered as though it
// did not exist.

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"testing"

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
	return ws, lake, model
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

// effectiveFor evaluates an item's stored policy for a principal, through the
// same two calls the DFS surface makes — so what this reports is what OneLake
// security actually decided, not a restatement of the fixture.
func effectiveFor(t *testing.T, st *store.Store, itemID, objectID, rel string) []onelakesec.AccessEntry {
	t.Helper()
	roles, err := st.EvaluatableRoles(itemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(roles) == 0 {
		t.Fatal("fixture stored no evaluatable role; the assertions below would be vacuous")
	}
	return onelakesec.Effective(roles,
		onelakesec.Principal{ObjectID: objectID}, onelakesec.InputFor(rel))
}

const salesQuery = `{"queries":[{"query":"EVALUATE SUMMARIZECOLUMNS(Sales[Region], \"Total\", [Total])"}]}`

// A Viewer no role names is denied every byte of the table on the storage
// surface — and reads all of it through DAX.
func TestDirectLakeServesAViewerNoRoleGrants(t *testing.T) {
	a, st := newAPI(t)
	_, lake, model := securedLakehouse(t, st)

	// A real policy, naming somebody else. Deny-by-default then applies to our
	// Viewer — which is a stronger fixture than storing no roles at all, where
	// a reader could put the outcome down to "no policy configured".
	putRole(t, st, lake.ID, "others", `{"name":"others","decisionRules":[{"effect":"Permit","permission":[
	  {"attributeName":"Path","attributeValueIncludedIn":["Tables/sales"]},
	  {"attributeName":"Action","attributeValueIncludedIn":["Read"]}]}],
	  "members":{"microsoftEntraMembers":[{"objectId":"somebody-else"}]}}`)

	entries := effectiveFor(t, st, lake.ID, viewer.ID, "Tables/sales")
	if onelakesec.Allows(entries, "Tables/sales") {
		t.Fatal("fixture grants the viewer access; it must not, or the next assertion proves nothing")
	}

	// The hole. Storage says no; the query path never asks.
	w := do(a.executeQueries, viewer, "POST", salesQuery, map[string]string{"datasetId": model.ID})
	if w.Code != 200 {
		t.Fatalf("executeQueries = %d %s", w.Code, w.Body.String())
	}
	for _, want := range []string{`"Sales[Region]":"us"`, `"Sales[Region]":"eu"`} {
		if !bytes.Contains(w.Body.Bytes(), []byte(want)) {
			t.Fatalf("CLOSED? a principal OneLake security denies no longer reads %s: %s",
				want, w.Body.String())
		}
	}
}

// A Viewer narrowed to one region reads both regions through DAX. This is the
// damaging half: the caller is a legitimate reader of the table, and the filter
// that should shape what they see is simply not applied.
func TestDirectLakeIgnoresARowFilter(t *testing.T) {
	a, st := newAPI(t)
	_, lake, model := securedLakehouse(t, st)

	putRole(t, st, lake.ID, "us-only", fmt.Sprintf(`{"name":"us-only","decisionRules":[{"effect":"Permit",
	  "permission":[
	    {"attributeName":"Path","attributeValueIncludedIn":["Tables/sales"]},
	    {"attributeName":"Action","attributeValueIncludedIn":["Read"]}],
	  "rows":"region = 'us'"}],
	  "members":{"microsoftEntraMembers":[{"objectId":%q}]}}`, viewer.ID))

	entries := effectiveFor(t, st, lake.ID, viewer.ID, "Tables/sales")
	narrowing := onelakesec.Narrowing(entries, "Tables/sales")
	if narrowing == nil {
		t.Fatal("fixture does not narrow the viewer; the next assertion proves nothing")
	}
	if !onelakesec.Allows(entries, "Tables/sales") {
		t.Fatal("fixture denies outright; this test is about a filter, not a denial")
	}

	w := do(a.executeQueries, viewer, "POST", salesQuery, map[string]string{"datasetId": model.ID})
	if w.Code != 200 {
		t.Fatalf("executeQueries = %d %s", w.Code, w.Body.String())
	}
	// `eu` is the row the filter excludes. Its presence is the divergence.
	if !bytes.Contains(w.Body.Bytes(), []byte(`"Sales[Region]":"eu"`)) {
		t.Fatalf("CLOSED? %s is now applied to Direct Lake: %s", narrowing.Why(), w.Body.String())
	}
}

// A Viewer granted one column reads every column through DAX.
func TestDirectLakeIgnoresAColumnProjection(t *testing.T) {
	a, st := newAPI(t)
	_, lake, model := securedLakehouse(t, st)

	putRole(t, st, lake.ID, "region-only", fmt.Sprintf(`{"name":"region-only","decisionRules":[{"effect":"Permit",
	  "permission":[
	    {"attributeName":"Path","attributeValueIncludedIn":["Tables/sales"]},
	    {"attributeName":"Action","attributeValueIncludedIn":["Read"]}],
	  "columns":["region"]}],
	  "members":{"microsoftEntraMembers":[{"objectId":%q}]}}`, viewer.ID))

	entries := effectiveFor(t, st, lake.ID, viewer.ID, "Tables/sales")
	if onelakesec.Narrowing(entries, "Tables/sales") == nil {
		t.Fatal("fixture does not narrow the viewer; the next assertion proves nothing")
	}

	// `amount` is outside the projection, and `Total` sums it.
	w := do(a.executeQueries, viewer, "POST", salesQuery, map[string]string{"datasetId": model.ID})
	if w.Code != 200 {
		t.Fatalf("executeQueries = %d %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(`"[Total]":80`)) {
		t.Fatalf("CLOSED? a column outside the projection no longer reaches DAX: %s", w.Body.String())
	}
}
