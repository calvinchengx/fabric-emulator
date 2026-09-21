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
// warehouse and a notebook, with a stub engine that reports which item a read
// reached — so a test can see resolution land on the right one.
func sqlSources(t *testing.T) (*API, *store.Store, *store.Workspace, map[string]*store.Item) {
	t.Helper()
	a, st := newAPI(t)
	a.SQLDBAs = func(_ context.Context, itemID, _ string) (*sql.DB, error) {
		return nil, errors.New("engine reached for " + itemID)
	}
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
		"a lakehouse's endpoint by GUID":        {items["SQLEndpoint"].ID, "engine reached for " + items["Lakehouse"].ID},
		"a warehouse by GUID":                   {items["Warehouse"].ID, "engine reached for " + items["Warehouse"].ID},
		"a warehouse by name":                   {"dw", "engine reached for " + items["Warehouse"].ID},
		"an endpoint by name":                   {"lake", "engine reached for " + items["Lakehouse"].ID},
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
		if _, body := queryDL(a, p, model); !strings.Contains(body, "engine reached for "+lake.ID) {
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
	a.SQLDBAs = nil
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
		if _, _, err := a.loadSemanticModel(t.Context(), model.ID, admin); err == nil || strings.Contains(err.Error(), "engine reached") {
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
		if _, _, err := a.loadSemanticModel(t.Context(), model.ID, admin); err == nil || strings.Contains(err.Error(), "engine reached") {
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

// Stage 3: the rows are read through the endpoint, by name, from the database
// the hook opened for the caller. Here the hook opens SQLite with a dbo schema;
// what the engine decides for a real caller is witnessed against SQL Server in
// internal/server/directlake_sql_test.go.

func sqliteEndpoint(t *testing.T, stmts ...string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1) // ATTACH is per connection
	t.Cleanup(func() { _ = db.Close() })
	for _, s := range append([]string{`ATTACH DATABASE ':memory:' AS dbo`, `ATTACH DATABASE ':memory:' AS gold`}, stmts...) {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return db
}

// sqlModel is a Direct Lake on SQL model over a warehouse, one table with a
// source column whose name needs quoting.
func sqlModel(t *testing.T, st *store.Store, wsID, warehouseID, schema, entity string, columns ...[2]string) *store.Item {
	t.Helper()
	var cols []map[string]string
	for _, c := range columns {
		col := map[string]string{"name": c[0], "dataType": "string"}
		if c[1] != "" {
			col["sourceColumn"] = c[1]
		}
		cols = append(cols, col)
	}
	source := map[string]string{"type": "entity", "entityName": entity, "expressionSource": "DL"}
	if schema != "" {
		source["schemaName"] = schema
	}
	bim, err := json.Marshal(map[string]any{"name": "DLSQL", "compatibilityLevel": 1604, "model": map[string]any{
		"expressions": []map[string]string{{"name": "DL", "kind": "m", "expression": sqlExpr(warehouseID)}},
		"tables": []map[string]any{{"name": "Sales", "columns": cols,
			"measures":   []map[string]string{{"name": "Rows", "expression": "COUNTROWS(Sales)"}},
			"partitions": []map[string]any{{"name": "p", "mode": "directLake", "source": source}}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	it := &store.Item{WorkspaceID: wsID, Type: "SemanticModel", DisplayName: fmt.Sprintf("DLSQL%d", dlModels.Add(1))}
	if err := st.CreateItem(it, []store.DefinitionPart{{Path: "model.bim", PayloadType: "InlineBase64",
		Payload: base64.StdEncoding.EncodeToString(bim)}}); err != nil {
		t.Fatal(err)
	}
	return it
}

func TestDirectLakeOnSQLReadsThroughTheEndpointAsTheCaller(t *testing.T) {
	a, st, ws, items := sqlSources(t)
	var opened []string
	var handles []*sql.DB
	a.SQLDBAs = func(_ context.Context, itemID, principalID string) (*sql.DB, error) {
		opened = append(opened, itemID+" as "+principalID)
		db := sqliteEndpoint(t, `CREATE TABLE dbo.sales ([region code] TEXT, [amount] TEXT, [secret] TEXT)`,
			`INSERT INTO dbo.sales VALUES ('us', CAST('10' AS BLOB), 'x'), ('eu', '20', 'y')`, // a driver may hand text back as bytes
			`CREATE TABLE gold.sales ([region code] TEXT, [amount] TEXT)`, `INSERT INTO gold.sales VALUES ('apac', '5')`)
		handles = append(handles, db)
		return db, nil
	}
	wh := items["Warehouse"]
	model := sqlModel(t, st, ws.ID, wh.ID, "", "sales", [2]string{"Region", "region code"}, [2]string{"Amount", "amount"})
	grantBuild(t, st, model, viewer.ID)
	w := do(a.executeQueries, viewer, "POST", `{"queries":[{"query":"EVALUATE 'Sales'"}]}`, map[string]string{"datasetId": model.ID})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"Sales[Region]":"us"`) ||
		!strings.Contains(w.Body.String(), `"Sales[Amount]":"20"`) || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("viewer = %d %s", w.Code, w.Body)
	}
	if len(opened) != 1 || opened[0] != wh.ID+" as "+viewer.ID {
		t.Errorf("the hook opened %v, want the warehouse once as the viewer", opened)
	}

	// A column with no sourceColumn is read by its own name, and a table that is
	// not Direct Lake is left to its own data.
	byName := sqlModel(t, st, ws.ID, wh.ID, "", "sales", [2]string{"amount", ""})
	if err := st.SetDefinition(byName.ID, append(mustParts(t, st, byName.ID), store.DefinitionPart{Path: "data.json",
		PayloadType: "InlineBase64", Payload: base64.StdEncoding.EncodeToString([]byte(`{"Notes":[{"Text":"kept"}]}`))})); err != nil {
		t.Fatal(err)
	}
	withImport(t, st, byName.ID)
	w = do(a.executeQueries, admin, "POST", `{"queries":[{"query":"EVALUATE 'Sales'"},{"query":"EVALUATE 'Notes'"}]}`, map[string]string{"datasetId": byName.ID})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"Sales[amount]":"10"`) || !strings.Contains(w.Body.String(), `"kept"`) {
		t.Errorf("by name, beside an import table = %d %s", w.Code, w.Body)
	}

	// A schema other than dbo is read from that schema.
	gold := sqlModel(t, st, ws.ID, wh.ID, "gold", "sales", [2]string{"Region", "region code"})
	if w := do(a.executeQueries, admin, "POST", `{"queries":[{"query":"EVALUATE 'Sales'"}]}`, map[string]string{"datasetId": gold.ID}); !strings.Contains(w.Body.String(), `"apac"`) {
		t.Errorf("gold schema = %s", w.Body)
	}
	// Each read's handle is closed: a pool kept open would outlive a revoke.
	for i, h := range handles {
		if err := h.Ping(); err == nil {
			t.Errorf("handle %d was left open", i)
		}
	}
}

func TestDirectLakeOnSQLReadRefusals(t *testing.T) {
	a, st, ws, items := sqlSources(t)
	a.SQLDBAs = func(context.Context, string, string) (*sql.DB, error) {
		return sqliteEndpoint(t, `CREATE TABLE dbo.sales ([amount] TEXT)`, `INSERT INTO dbo.sales VALUES ('1')`), nil
	}
	wh := items["Warehouse"].ID
	for name, tc := range map[string]struct {
		model *store.Item
		want  string
	}{
		// A column the endpoint cannot give the caller fails the read, not the row.
		"a column the endpoint does not return": {sqlModel(t, st, ws.ID, wh, "", "sales", [2]string{"Amount", "amount"}, [2]string{"Hidden", "salary"}),
			"the SQL analytics endpoint refused the read"},
		"a table the endpoint does not have": {sqlModel(t, st, ws.ID, wh, "", "missing", [2]string{"Amount", "amount"}),
			"the SQL analytics endpoint refused the read"},
		"a name no Fabric table can carry": {sqlModel(t, st, ws.ID, wh, "", "bad\nname", [2]string{"Amount", "amount"}),
			"unusable SQL name"},
		"a column name no Fabric column can carry": {sqlModel(t, st, ws.ID, wh, "", "sales", [2]string{"Amount", strings.Repeat("a", 129)}),
			"unusable SQL name"},
		"a table with no columns": {sqlModel(t, st, ws.ID, wh, "", "sales"), "has no columns to read"},
	} {
		if code, body := queryDL(a, admin, tc.model); code != http.StatusBadRequest || !strings.Contains(body, tc.want) {
			t.Errorf("%s: %d %s, want %q", name, code, body, tc.want)
		}
	}

	// The hook's own refusal — the caller's access, decided by the server — is
	// reported as Direct Lake on SQL's.
	a.SQLDBAs = func(context.Context, string, string) (*sql.DB, error) { return nil, errors.New("access denied") }
	if _, body := queryDL(a, admin, sqlModel(t, st, ws.ID, wh, "", "sales", [2]string{"Amount", "amount"})); !strings.Contains(body, "Direct Lake on SQL: access denied") {
		t.Errorf("hook refusal = %s", body)
	}
}

func mustParts(t *testing.T, st *store.Store, itemID string) []store.DefinitionPart {
	t.Helper()
	parts, err := st.GetDefinition(itemID)
	if err != nil {
		t.Fatal(err)
	}
	return parts
}

// withImport adds an import table, Notes, to a model's model.bim.
func withImport(t *testing.T, st *store.Store, itemID string) {
	t.Helper()
	parts := mustParts(t, st, itemID)
	for i, p := range parts {
		if p.Path != "model.bim" {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(p.Payload)
		if err != nil {
			t.Fatal(err)
		}
		var bim map[string]any
		if err := json.Unmarshal(raw, &bim); err != nil {
			t.Fatal(err)
		}
		model := bim["model"].(map[string]any)
		model["tables"] = append(model["tables"].([]any), map[string]any{"name": "Notes",
			"columns": []map[string]string{{"name": "Text", "dataType": "string"}}})
		out, err := json.Marshal(bim)
		if err != nil {
			t.Fatal(err)
		}
		parts[i].Payload = base64.StdEncoding.EncodeToString(out)
	}
	if err := st.SetDefinition(itemID, parts); err != nil {
		t.Fatal(err)
	}
}

// Stage 4: directLakeBehavior. Under directLakeOnly a table the endpoint would
// send to DirectQuery fails; under automatic and directQueryOnly it is served as
// the caller, and the catalog is not even asked.

// setBehavior rewrites a model's directLakeBehavior.
func setBehavior(t *testing.T, st *store.Store, itemID, behavior string) {
	t.Helper()
	parts := mustParts(t, st, itemID)
	for i, p := range parts {
		if p.Path != "model.bim" {
			continue
		}
		raw, _ := base64.StdEncoding.DecodeString(p.Payload)
		var bim map[string]any
		if err := json.Unmarshal(raw, &bim); err != nil {
			t.Fatal(err)
		}
		bim["model"].(map[string]any)["directLakeBehavior"] = behavior
		out, _ := json.Marshal(bim)
		parts[i].Payload = base64.StdEncoding.EncodeToString(out)
	}
	if err := st.SetDefinition(itemID, parts); err != nil {
		t.Fatal(err)
	}
}

func TestDirectLakeOnlyRefusesWhatWouldFallBack(t *testing.T) {
	a, st, ws, items := sqlSources(t)
	a.SQLDBAs = func(context.Context, string, string) (*sql.DB, error) {
		return sqliteEndpoint(t, `CREATE TABLE dbo.sales ([amount] TEXT)`, `INSERT INTO dbo.sales VALUES ('1')`), nil
	}
	catalog := sqliteEndpoint(t)
	var catalogFor []string
	a.SQLDB = func(_ context.Context, id string) (*sql.DB, error) {
		catalogFor = append(catalogFor, "warehouse "+id)
		return catalog, nil
	}
	a.LakehouseDB = func(_ context.Context, id string) (*sql.DB, error) {
		catalogFor = append(catalogFor, "lakehouse "+id)
		return catalog, nil
	}
	causes := []string{"the SQL analytics endpoint enforces row-level security on [dbo].[sales]"}
	var asked []string
	orig := directQueryFallbackCauses
	t.Cleanup(func() { directQueryFallbackCauses = orig })
	directQueryFallbackCauses = func(_ context.Context, db *sql.DB, object string) ([]string, error) {
		if db != catalog {
			t.Error("the catalog was not read through the service connection")
		}
		asked = append(asked, object)
		return causes, nil
	}

	for _, target := range []*store.Item{items["Warehouse"], items["SQLEndpoint"]} {
		model := sqlModel(t, st, ws.ID, target.ID, "", "sales", [2]string{"Amount", "amount"})
		for behavior, wantOK := range map[string]bool{"automatic": true, "directQueryOnly": true, "directLakeOnly": false} {
			setBehavior(t, st, model.ID, behavior)
			asked = nil
			code, body := querySales(a, admin, model)
			if wantOK && (code != http.StatusOK || len(asked) != 0) {
				t.Errorf("%s over a %s: %d %s, asked %v; want served without asking", behavior, target.Type, code, body, asked)
			}
			if !wantOK && (code != http.StatusBadRequest || !strings.Contains(body, "directLakeOnly, which disables DirectQuery fallback") ||
				!strings.Contains(body, "row-level security on [dbo].[sales]") || len(asked) != 1 || asked[0] != "[dbo].[sales]") {
				t.Errorf("%s over a %s: %d %s, asked %v; want the refusal", behavior, target.Type, code, body, asked)
			}
		}
	}
	if len(catalogFor) != 2 || catalogFor[0] != "warehouse "+items["Warehouse"].ID || catalogFor[1] != "lakehouse "+items["Lakehouse"].ID {
		t.Errorf("catalog connections = %v, want the warehouse's then the lakehouse's", catalogFor)
	}

	// Nothing to fall back from: served under directLakeOnly too.
	causes = nil
	model := sqlModel(t, st, ws.ID, items["Warehouse"].ID, "", "sales", [2]string{"Amount", "amount"})
	setBehavior(t, st, model.ID, "directLakeOnly")
	if code, body := querySales(a, admin, model); code != http.StatusOK {
		t.Errorf("directLakeOnly with nothing to fall back from = %d %s", code, body)
	}

	// Failing to read the catalog fails the query rather than serving it.
	directQueryFallbackCauses = func(context.Context, *sql.DB, string) ([]string, error) { return nil, errors.New("catalog down") }
	if _, body := querySales(a, admin, model); !strings.Contains(body, "reading the endpoint's catalog: catalog down") {
		t.Errorf("catalog failure = %s", body)
	}
	a.SQLDB = func(context.Context, string) (*sql.DB, error) { return nil, errors.New("no service login") }
	if _, body := querySales(a, admin, model); !strings.Contains(body, "reading the endpoint's catalog: no service login") {
		t.Errorf("service connection failure = %s", body)
	}
	a.SQLDB = nil
	if _, body := querySales(a, admin, model); !strings.Contains(body, "serves no service connection") {
		t.Errorf("no service connection = %s", body)
	}
}

func querySales(a *API, p *auth.Principal, model *store.Item) (int, string) {
	w := do(a.executeQueries, p, "POST", `{"queries":[{"query":"EVALUATE 'Sales'"}]}`, map[string]string{"datasetId": model.ID})
	return w.Code, w.Body.String()
}

// Over a lakehouse whose endpoint is in user identity access mode, Direct Lake
// on SQL "falls back to DirectQuery 100% of the time": directLakeOnly fails every
// table without consulting the catalog, and automatic serves the caller.
func TestDirectLakeOnlyOverAUserIdentityEndpoint(t *testing.T) {
	a, st, ws, items := sqlSources(t)
	a.SQLDBAs = func(context.Context, string, string) (*sql.DB, error) {
		return sqliteEndpoint(t, `CREATE TABLE dbo.sales ([amount] TEXT)`, `INSERT INTO dbo.sales VALUES ('1')`), nil
	}
	a.LakehouseDB = func(context.Context, string) (*sql.DB, error) { return sqliteEndpoint(t), nil }
	orig := directQueryFallbackCauses
	t.Cleanup(func() { directQueryFallbackCauses = orig })
	directQueryFallbackCauses = func(context.Context, *sql.DB, string) ([]string, error) { return nil, nil }
	if err := st.SetItemProperties(items["SQLEndpoint"].ID, map[string]string{store.PropDataAccessMode: store.AccessModeUserIdentity}); err != nil {
		t.Fatal(err)
	}
	model := sqlModel(t, st, ws.ID, items["SQLEndpoint"].ID, "", "sales", [2]string{"Amount", "amount"})
	setBehavior(t, st, model.ID, "directLakeOnly")
	if code, body := querySales(a, admin, model); code != http.StatusBadRequest || !strings.Contains(body, "user identity access mode") {
		t.Errorf("directLakeOnly over a user identity endpoint = %d %s", code, body)
	}
	setBehavior(t, st, model.ID, "automatic")
	if code, body := querySales(a, admin, model); code != http.StatusOK {
		t.Errorf("automatic over a user identity endpoint = %d %s", code, body)
	}
}

func TestDirectLakeOnlyFailsClosedOnAnUnreadableAccessMode(t *testing.T) {
	a, st, dir := newDiskAPI(t)
	ws := seedWorkspace(t, st)
	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lake"}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}
	a.LakehouseDB = func(context.Context, string) (*sql.DB, error) { return sqliteEndpoint(t), nil }
	table := &semanticmodel.Table{Name: "Sales", DirectLake: &semanticmodel.DirectLakePartition{EntityName: "sales"}}
	dropTable(t, dir, "items")
	if err := a.refuseDirectQueryFallback(t.Context(), lake, table, "[dbo].[sales]"); err == nil ||
		!strings.Contains(err.Error(), "reading the endpoint's access mode") {
		t.Errorf("an unreadable access mode = %v", err)
	}
}
