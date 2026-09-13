package api

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// Stage 3: querying a semantic model needs Read and Build, and a Direct Lake
// model's source is read through the same OneLake decision the storage surface
// uses. Every admission is paired with the same request refused.

const everythingQuery = `{"queries":[{"query":"EVALUATE Sales"}]}`

// Build comes from three places, and each one is witnessed with a caller who
// lacks it: Contributor inheritance, a grant to a Viewer, and a grant to a
// principal with no workspace role at all.
func TestExecuteQueriesRequiresReadAndBuildFromAnySource(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	assignRole(t, st, ws.ID, contributor, store.RoleContributor)
	ds := createSemanticModel(t, st, ws.ID)
	pv := map[string]string{"datasetId": ds.ID}
	q := `{"queries":[{"query":"EVALUATE 'Store'"}]}`

	if w := do(a.executeQueries, contributor, "POST", q, pv); w.Code != http.StatusOK {
		t.Errorf("a Contributor, who inherits Build = %d %s", w.Code, w.Body)
	}

	// A principal with no role: Read alone is not enough…
	if err := st.PutItemAccess(store.ItemAccess{ItemID: ds.ID, PrincipalID: stranger.ID, PrincipalType: "User",
		Permissions: []string{store.PermRead}}); err != nil {
		t.Fatal(err)
	}
	if w := do(a.executeQueries, stranger, "POST", q, pv); w.Code != http.StatusForbidden {
		t.Errorf("a stranger with Read only = %d, want 403", w.Code)
	}
	// …Read and Build is, through the documented dataset-users API…
	if w := do(a.putDatasetUser, admin, "PUT",
		`{"identifier":"stranger-1","principalType":"User","datasetUserAccessRight":"ReadExplore"}`, pv); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	if w := do(a.executeQueries, stranger, "POST", q, pv); w.Code != http.StatusOK {
		t.Errorf("a stranger with Read and Build = %d %s", w.Code, w.Body)
	}
	// …and removing it removes the query.
	if w := do(a.putDatasetUser, admin, "PUT",
		`{"identifier":"stranger-1","principalType":"User","datasetUserAccessRight":"None"}`, pv); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	if w := do(a.executeQueries, stranger, "POST", q, pv); w.Code != http.StatusForbidden {
		t.Errorf("after PUT None = %d, want 403", w.Code)
	}
}

func TestExecuteQueriesFailsWhenAccessCannotBeRead(t *testing.T) {
	a, st, dir := newDiskAPI(t)
	ws := seedWorkspace(t, st)
	ds := createSemanticModel(t, st, ws.ID)
	dropTable(t, dir, "role_assignments")
	if w := do(a.executeQueries, admin, "POST", `{"queries":[{"query":"EVALUATE 'Store'"}]}`,
		map[string]string{"datasetId": ds.ID}); w.Code != http.StatusInternalServerError {
		t.Fatalf("= %d %s, want 500", w.Code, w.Body)
	}
}

// crossWorkspaceModel is a Direct Lake model in one workspace over a lakehouse
// in another that only admin has a role in.
func crossWorkspaceModel(t *testing.T, st *store.Store) (*store.Item, *store.Item) {
	t.Helper()
	modelWS := seedWorkspace(t, st)
	sourceWS := &store.Workspace{DisplayName: "source"}
	if err := st.CreateWorkspace(sourceWS, store.Principal{ID: admin.ID, Type: admin.Type}); err != nil {
		t.Fatal(err)
	}
	lake := &store.Item{WorkspaceID: sourceWS.ID, Type: "Lakehouse", DisplayName: "lake"}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}
	putDirectLakeFile(t, st, sourceWS.ID, lake.ID, "Tables/sales/part-0.parquet",
		directLakeParquet(t, []directLakeSale{{"us", 80}}))
	putDirectLakeFile(t, st, sourceWS.ID, lake.ID, "Tables/sales/_delta_log/00000000000000000000.json",
		[]byte(`{"add":{"path":"part-0.parquet"}}`))
	model := &store.Item{WorkspaceID: modelWS.ID, Type: "SemanticModel", DisplayName: "cross"}
	parts := []store.DefinitionPart{{Path: "model.bim", PayloadType: "InlineBase64",
		Payload: base64.StdEncoding.EncodeToString(directLakeModel(sourceWS.ID, lake.ID))}}
	if err := st.CreateItem(model, parts); err != nil {
		t.Fatal(err)
	}
	return model, lake
}

// A model reaches a lakehouse in a workspace its reader has no role in, through
// a grant on the lakehouse — and not without one.
func TestAGrantOnTheSourceReachesAcrossWorkspaces(t *testing.T) {
	a, st := newAPI(t)
	model, lake := crossWorkspaceModel(t, st)
	grantBuild(t, st, model, stranger.ID)
	pv := map[string]string{"datasetId": model.ID}

	if w := do(a.executeQueries, stranger, "POST", everythingQuery, pv); w.Code != http.StatusBadRequest ||
		!bytes.Contains(w.Body.Bytes(), []byte("cannot read the source")) {
		t.Fatalf("without a source grant = %d %s", w.Code, w.Body)
	}
	if err := st.PutItemAccess(store.ItemAccess{ItemID: lake.ID, PrincipalID: stranger.ID, PrincipalType: "User",
		Permissions: []string{store.PermRead}, Additional: []string{store.PermReadAll}}); err != nil {
		t.Fatal(err)
	}
	if w := do(a.executeQueries, stranger, "POST", everythingQuery, pv); w.Code != http.StatusOK ||
		!bytes.Contains(w.Body.Bytes(), []byte(`"Sales[Region]":"us"`)) {
		t.Fatalf("with Read and ReadAll on the source = %d %s", w.Code, w.Body)
	}
}

// Somebody with no role in the source workspace is told only that they cannot
// read the source — not whether an item by that name exists. Someone with a role
// there is told the plain truth.
func TestAnUnresolvableSourceDisclosesOnlyToARoleHolder(t *testing.T) {
	a, st := newAPI(t)
	model, lake := crossWorkspaceModel(t, st)
	grantBuild(t, st, model, stranger.ID)
	pv := map[string]string{"datasetId": model.ID}
	if err := st.DeleteItem(lake.WorkspaceID, lake.ID); err != nil {
		t.Fatal(err)
	}
	if w := do(a.executeQueries, stranger, "POST", everythingQuery, pv); !bytes.Contains(w.Body.Bytes(), []byte("cannot read the source")) ||
		bytes.Contains(w.Body.Bytes(), []byte("no lakehouse or warehouse")) {
		t.Errorf("a stranger = %d %s, want only a refusal", w.Code, w.Body)
	}
	if w := do(a.executeQueries, admin, "POST", everythingQuery, pv); !bytes.Contains(w.Body.Bytes(), []byte("no lakehouse or warehouse")) {
		t.Errorf("the owner = %d %s, want the missing source named", w.Code, w.Body)
	}
}

// A warehouse source carries no OneLake roles, so ReadAll is the whole decision
// — refused before any SQL is attempted, which is why no SQL Server is needed.
func TestAWarehouseSourceRequiresReadAll(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	wh := typedIn(t, st, ws.ID, "Warehouse")
	model := &store.Item{WorkspaceID: ws.ID, Type: "SemanticModel", DisplayName: "gold"}
	parts := []store.DefinitionPart{{Path: "model.bim", PayloadType: "InlineBase64",
		Payload: base64.StdEncoding.EncodeToString(directLakeWarehouseModel(ws.ID, wh.ID))}}
	if err := st.CreateItem(model, parts); err != nil {
		t.Fatal(err)
	}
	grantBuild(t, st, model, viewer.ID)
	a.SQLDB = nil // nothing may reach SQL on the refused path
	w := do(a.executeQueries, viewer, "POST", `{"queries":[{"query":"EVALUATE Revenue"}]}`,
		map[string]string{"datasetId": model.ID})
	if w.Code != http.StatusBadRequest || !bytes.Contains(w.Body.Bytes(), []byte("cannot read the source")) {
		t.Fatalf("a Viewer without ReadAll = %d %s", w.Code, w.Body)
	}
	// With ReadAll the refusal moves past access to the next honest answer:
	// there is no SQL engine here — proving the access check let it through.
	if err := st.PutItemAccess(store.ItemAccess{ItemID: wh.ID, PrincipalID: viewer.ID, PrincipalType: "User",
		Permissions: []string{store.PermRead}, Additional: []string{store.PermReadAll}}); err != nil {
		t.Fatal(err)
	}
	w = do(a.executeQueries, viewer, "POST", `{"queries":[{"query":"EVALUATE Revenue"}]}`,
		map[string]string{"datasetId": model.ID})
	if bytes.Contains(w.Body.Bytes(), []byte("cannot read the source")) || !bytes.Contains(w.Body.Bytes(), []byte("serves no SQL")) {
		t.Fatalf("a Viewer with ReadAll = %d %s, want past the access check", w.Code, w.Body)
	}
}

// The source's access decision reads the store; a failure is an error, never an
// admission.
func TestDirectLakeFailsWhenSourceAccessCannotBeRead(t *testing.T) {
	a, st, dir := newDiskAPI(t)
	ws := seedWorkspace(t, st)
	assignRole(t, st, ws.ID, contributor, store.RoleContributor)
	lake := typedIn(t, st, ws.ID, "Lakehouse")
	model := &store.Item{WorkspaceID: ws.ID, Type: "SemanticModel", DisplayName: "m"}
	parts := []store.DefinitionPart{{Path: "model.bim", PayloadType: "InlineBase64",
		Payload: base64.StdEncoding.EncodeToString(directLakeModel(ws.ID, lake.ID))}}
	if err := st.CreateItem(model, parts); err != nil {
		t.Fatal(err)
	}
	// Precisely the SOURCE: the contributor holds no grant on the model, so its
	// access resolves cleanly, while their grant on the lakehouse is corrupt.
	// Breaking item_access wholesale would fail at the model's check instead and
	// prove nothing about this path.
	corruptGrantFor(t, st, dir, lake, contributor.ID)
	w := do(a.executeQueries, contributor, "POST", everythingQuery, map[string]string{"datasetId": model.ID})
	if w.Code != http.StatusBadRequest || !bytes.Contains(w.Body.Bytes(), []byte(`Direct Lake table \"Sales\"`)) {
		t.Fatalf("= %d %s, want the source's access failure on the Direct Lake table", w.Code, w.Body)
	}
}

// The loader's remaining refusals, reached as the owner so no access decision is
// in the way: a partition naming a missing expression, a source workspace that
// does not exist, and a model column the Delta table does not carry.
func TestDirectLakeLoaderRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		model func(wsID, lakeID string) []byte
		want  string
	}{
		"a missing expression": {func(wsID, lakeID string) []byte {
			return bytes.Replace(directLakeModel(wsID, lakeID), []byte(`"expressionSource":"DL_Lakehouse"`),
				[]byte(`"expressionSource":"nowhere"`), 1)
		}, "references missing expression"},
		"an unknown workspace": {func(_, lakeID string) []byte {
			return directLakeModel("no-such-workspace", lakeID)
		}, "workspace is not available"},
		"a column the table lacks": {func(wsID, lakeID string) []byte {
			return bytes.Replace(directLakeModel(wsID, lakeID), []byte(`"sourceColumn":"amount"`),
				[]byte(`"sourceColumn":"no_such_column"`), 1)
		}, "missing source column"},
	} {
		t.Run(name, func(t *testing.T) {
			a, st := newAPI(t)
			ws := seedWorkspace(t, st)
			lake := typedIn(t, st, ws.ID, "Lakehouse")
			putDirectLakeFile(t, st, ws.ID, lake.ID, "Tables/sales/part-0.parquet",
				directLakeParquet(t, []directLakeSale{{"us", 80}}))
			putDirectLakeFile(t, st, ws.ID, lake.ID, "Tables/sales/_delta_log/00000000000000000000.json",
				[]byte(`{"add":{"path":"part-0.parquet"}}`))
			model := &store.Item{WorkspaceID: ws.ID, Type: "SemanticModel", DisplayName: "m"}
			parts := []store.DefinitionPart{{Path: "model.bim", PayloadType: "InlineBase64",
				Payload: base64.StdEncoding.EncodeToString(tc.model(ws.ID, lake.ID))}}
			if err := st.CreateItem(model, parts); err != nil {
				t.Fatal(err)
			}
			w := do(a.executeQueries, admin, "POST", everythingQuery, map[string]string{"datasetId": model.ID})
			if w.Code != http.StatusBadRequest || !bytes.Contains(w.Body.Bytes(), []byte(tc.want)) {
				t.Fatalf("= %d %s, want %q", w.Code, w.Body, tc.want)
			}
		})
	}
}
