package api

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/semanticmodel"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// Direct Lake on SQL. Stage 1: a Sql.Database expression is RECOGNISED, and
// what cannot be served is refused by name. It used to fail as "shared expression must contain an
// onelake.dfs.fabric.microsoft.com workspace/lakehouse URL", which blamed a model
// for not being the other flavour.

var dlModels atomic.Int64

// dlModel builds a model whose Direct Lake tables each bind to one of exprs, by
// name: table i uses expression "E<i>".
func dlModel(t *testing.T, st *store.Store, wsID string, exprs ...string) *store.Item {
	t.Helper()
	type expression struct {
		Name       string `json:"name"`
		Kind       string `json:"kind"`
		Expression string `json:"expression"`
	}
	var es []expression
	var tables []map[string]any
	for i, e := range exprs {
		name := "E" + string(rune('0'+i))
		if e != "" {
			es = append(es, expression{Name: name, Kind: "m", Expression: e})
		}
		tables = append(tables, map[string]any{
			"name":    "T" + string(rune('0'+i)),
			"columns": []map[string]string{{"name": "C", "dataType": "string", "sourceColumn": "c"}},
			"partitions": []map[string]any{{"name": "p", "mode": "directLake",
				"source": map[string]string{"type": "entity", "entityName": "t", "expressionSource": name}}},
		})
	}
	bim, err := json.Marshal(map[string]any{"name": "DL", "compatibilityLevel": 1604,
		"model": map[string]any{"expressions": es, "tables": tables}})
	if err != nil {
		t.Fatal(err)
	}
	it := &store.Item{WorkspaceID: wsID, Type: "SemanticModel", DisplayName: fmt.Sprintf("DL%d", dlModels.Add(1))}
	if err := st.CreateItem(it, []store.DefinitionPart{{Path: "model.bim", PayloadType: "InlineBase64",
		Payload: base64.StdEncoding.EncodeToString(bim)}}); err != nil {
		t.Fatal(err)
	}
	return it
}

const (
	sqlEndpointExpr = `let database = Sql.Database("abc.datawarehouse.fabric.microsoft.com", "803c8e33-c35c-4f1b-9b44-f40dce69e75e") in database`
	otherSQLExpr    = `Sql.Database("abc.datawarehouse.fabric.microsoft.com", "11111111-2222-3333-4444-555555555555")`
	oneLakeExpr     = `AzureStorage.DataLake("https://onelake.dfs.fabric.microsoft.com/ws/lake")`
	unparseableExpr = `Lakehouse.Contents(null)`
)

func TestDirectLakeOnSQLIsRecognisedAndRefusedByName(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	for name, tc := range map[string]struct {
		exprs []string
		want  string
	}{
		"one SQL source, which does not exist": {[]string{sqlEndpointExpr, sqlEndpointExpr}, "caller cannot read the source"},
		"the same source by another spelling":  {[]string{sqlEndpointExpr, `Sql.Database("x", "803C8E33-C35C-4F1B-9B44-F40DCE69E75E")`}, "caller cannot read the source"},
		"two SQL sources":                      {[]string{sqlEndpointExpr, otherSQLExpr}, "Direct Lake on SQL uses a single source"},
		"both flavours":                        {[]string{oneLakeExpr, sqlEndpointExpr}, "mixes Direct Lake on OneLake and Direct Lake on SQL"},
		"an expression of neither flavour":     {[]string{unparseableExpr}, "neither Direct Lake on OneLake"},
		"a missing expression":                 {[]string{""}, `references missing expression \"E0\"`},
	} {
		model := dlModel(t, st, ws.ID, tc.exprs...)
		w := do(a.executeQueries, admin, "POST", `{"queries":[{"query":"EVALUATE 'T0'"}]}`, map[string]string{"datasetId": model.ID})
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.want) || strings.Contains(w.Body.String(), "must contain an onelake") {
			t.Errorf("%s: %d %s, want a refusal naming %q", name, w.Code, w.Body, tc.want)
		}
	}
}

// The datasources endpoint and lineage share the parser, so they can never
// report a SQL-flavour model as a OneLake one.
func TestDirectLakeOnSQLIsNotReportedAsAOneLakeSource(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	for _, expr := range []string{sqlEndpointExpr, unparseableExpr} {
		model := dlModel(t, st, ws.ID, expr)
		w := do(a.listDatasources, admin, "GET", "", map[string]string{"datasetId": model.ID})
		if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "onelake.dfs.fabric.microsoft.com/") {
			t.Errorf("datasources for %s = %d %s, want a refusal", expr, w.Code, w.Body)
		}
		m, err := a.parseModelDefinition(model.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := a.directLakeSource(m, &m.Tables[0], admin); ok {
			t.Errorf("lineage resolved a source for %s", expr)
		}
	}
}

// Stage 2: the database argument resolves to the lakehouse behind a SQL
// analytics endpoint, or to a warehouse, for a caller holding Read on it.

// sqlSources is a workspace with a lakehouse, its SQL analytics endpoint, a
// warehouse and a notebook, with SQL engines stubbed as attached: resolution
// never opens them.
func sqlSources(t *testing.T) (*API, *store.Store, *store.Workspace, map[string]*store.Item) {
	t.Helper()
	a, st := newAPI(t)
	a.LakehouseDB = func(context.Context, string) (*sql.DB, error) { return nil, errors.New("not opened by resolution") }
	a.SQLDB = a.LakehouseDB
	ws := seedWorkspace(t, st)
	items := map[string]*store.Item{}
	for _, it := range []*store.Item{
		{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lake"},
		{WorkspaceID: ws.ID, Type: "Warehouse", DisplayName: "dw"},
		{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "nb"},
	} {
		if err := st.CreateItem(it, nil); err != nil {
			t.Fatal(err)
		}
		items[it.Type] = it
	}
	ep, err := st.GetItemByID(a.ensureSQLEndpointItem(items["Lakehouse"]))
	if err != nil {
		t.Fatal(err)
	}
	items["SQLEndpoint"] = ep
	return a, st, ws, items
}

func sqlExpr(database string) string {
	return `Sql.Database("abc.datawarehouse.fabric.microsoft.com", "` + database + `")`
}

func queryDL(a *API, p *auth.Principal, model *store.Item) (int, string) {
	w := do(a.executeQueries, p, "POST", `{"queries":[{"query":"EVALUATE 'T0'"}]}`, map[string]string{"datasetId": model.ID})
	return w.Code, w.Body.String()
}

func TestDirectLakeOnSQLResolvesItsSource(t *testing.T) {
	a, st, ws, items := sqlSources(t)
	for name, tc := range map[string]struct {
		database string
		want     string
	}{
		"a lakehouse's endpoint by GUID":        {items["SQLEndpoint"].ID, "recognised but not served"},
		"a warehouse by GUID":                   {items["Warehouse"].ID, "recognised but not served"},
		"a warehouse by name":                   {"dw", "recognised but not served"},
		"an endpoint by name":                   {"lake", "recognised but not served"},
		"the lakehouse instead of its endpoint": {items["Lakehouse"].ID, "is a lakehouse; Sql.Database names its SQL analytics endpoint"},
		"an item that is no SQL source":         {items["Notebook"].ID, "is a Notebook"},
		"a name nothing carries":                {"nowhere", "caller cannot read the source"},
	} {
		code, body := queryDL(a, admin, dlModel(t, st, ws.ID, sqlExpr(tc.database)))
		if code != http.StatusBadRequest || !strings.Contains(body, tc.want) {
			t.Errorf("%s: %d %s, want %q", name, code, body, tc.want)
		}
	}

	// A name both an endpoint and a warehouse carry is ambiguous, not a pick.
	if err := st.CreateItem(&store.Item{WorkspaceID: ws.ID, Type: "Warehouse", DisplayName: "lake"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, body := queryDL(a, admin, dlModel(t, st, ws.ID, sqlExpr("lake"))); !strings.Contains(body, "name the source by its GUID") {
		t.Errorf("an ambiguous name: %s", body)
	}
}

// Read on the source decides, before anything about the item is described: a
// principal without it learns neither the item's type nor whether it exists.
func TestDirectLakeOnSQLNeedsReadOnTheSource(t *testing.T) {
	a, st, ws, items := sqlSources(t)
	for _, target := range []*store.Item{items["SQLEndpoint"], items["Lakehouse"], items["Notebook"]} {
		model := dlModel(t, st, ws.ID, sqlExpr(target.ID))
		grantBuild(t, st, model, stranger.ID)
		if _, body := queryDL(a, stranger, model); !strings.Contains(body, "caller cannot read the source") {
			t.Errorf("stranger on %s: %s, want only that they cannot read it", target.Type, body)
		}
	}

	// Read on the lakehouse admits a stranger through its endpoint; revoking it
	// refuses them again. A workspace Viewer holds Read by role.
	lake := items["Lakehouse"]
	model := dlModel(t, st, ws.ID, sqlExpr(items["SQLEndpoint"].ID))
	grantBuild(t, st, model, stranger.ID)
	grantBuild(t, st, model, viewer.ID)
	if err := st.PutItemAccess(store.ItemAccess{ItemID: lake.ID, PrincipalID: stranger.ID, PrincipalType: "User",
		Permissions: []string{store.PermRead}}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []*auth.Principal{stranger, viewer} {
		if _, body := queryDL(a, p, model); !strings.Contains(body, "recognised but not served") {
			t.Errorf("%s with Read: %s", p.ID, body)
		}
	}
	if err := st.DeleteItemAccess(lake.ID, stranger.ID); err != nil {
		t.Fatal(err)
	}
	if _, body := queryDL(a, stranger, model); !strings.Contains(body, "caller cannot read the source") {
		t.Errorf("stranger after revoke: %s", body)
	}
}

func TestDirectLakeOnSQLWithoutAnEngineIsRefusedByName(t *testing.T) {
	a, st, ws, items := sqlSources(t)
	a.LakehouseDB, a.SQLDB = nil, nil
	for _, target := range []*store.Item{items["SQLEndpoint"], items["Warehouse"]} {
		if _, body := queryDL(a, admin, dlModel(t, st, ws.ID, sqlExpr(target.ID))); !strings.Contains(body, "serves no SQL") {
			t.Errorf("%s without an engine: %s", target.Type, body)
		}
	}
}

func TestDirectLakeOnSQLResolutionFailsClosed(t *testing.T) {
	t.Run("an endpoint that serves no lakehouse", func(t *testing.T) {
		a, st, ws, _ := sqlSources(t)
		orphan := &store.Item{WorkspaceID: ws.ID, Type: "SQLEndpoint", DisplayName: "orphan"}
		if err := st.CreateItem(orphan, nil); err != nil {
			t.Fatal(err)
		}
		if _, body := queryDL(a, admin, dlModel(t, st, ws.ID, sqlExpr(orphan.ID))); !strings.Contains(body, "serves no lakehouse") {
			t.Errorf("orphan endpoint: %s", body)
		}
	})
	t.Run("the caller's access", func(t *testing.T) {
		a, st, dir := newDiskAPI(t)
		ws := seedWorkspace(t, st)
		wh := &store.Item{WorkspaceID: ws.ID, Type: "Warehouse", DisplayName: "dw"}
		if err := st.CreateItem(wh, nil); err != nil {
			t.Fatal(err)
		}
		model := dlModel(t, st, ws.ID, sqlExpr(wh.ID))
		dropTable(t, dir, "role_assignments")
		if _, _, err := a.loadSemanticModel(t.Context(), model.ID, admin); err == nil || strings.Contains(err.Error(), "not served") {
			t.Fatalf("an unreadable role reached the source: %v", err)
		}
	})
	t.Run("the endpoint's properties", func(t *testing.T) {
		a, st, dir := newDiskAPI(t)
		ws := seedWorkspace(t, st)
		lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lake"}
		if err := st.CreateItem(lake, nil); err != nil {
			t.Fatal(err)
		}
		epID := a.ensureSQLEndpointItem(lake)
		model := dlModel(t, st, ws.ID, sqlExpr(epID))
		dropTable(t, dir, "item_properties")
		if _, _, err := a.loadSemanticModel(t.Context(), model.ID, admin); err == nil || strings.Contains(err.Error(), "not served") {
			t.Fatalf("unreadable endpoint properties reached the source: %v", err)
		}
	})
	t.Run("the model item, when resolving by name", func(t *testing.T) {
		a, _, _, _ := sqlSources(t)
		src := &semanticmodel.DirectLakeSource{Flavor: semanticmodel.DirectLakeOnSQL, Server: "h", Database: "dw"}
		if _, err := a.resolveDirectLakeSQLSource("no-such-model", src, admin); err == nil {
			t.Fatal("a name was resolved without the model's workspace")
		}
	})
}
