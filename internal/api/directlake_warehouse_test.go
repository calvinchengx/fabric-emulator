package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/semanticmodel"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/testsupport"
)

// dlPartition is the minimum Direct Lake binding these tests need.
func dlPartition(entity string) *semanticmodel.DirectLakePartition {
	return &semanticmodel.DirectLakePartition{EntityName: entity, SchemaName: "dbo",
		ExpressionSource: "DL_Warehouse"}
}

// Direct Lake over a WAREHOUSE, which is what a model serving gold needs.
//
// WHY THIS EXISTS. Gold is built by dbt-fabric into the warehouse, so a semantic
// model over it had no portable way to get its rows: the examples carried them
// inline in a `data.json` part, which real Fabric has no concept of. The one
// artifact a BI consumer actually reads was the one thing that could not be
// deployed to a tenant.
//
// Real Fabric supports Direct Lake over a warehouse because a warehouse persists
// to OneLake as Delta. The emulator's warehouse is a real SQL Server database, so
// the same rows come over SQL — a backend difference, not a contract one. The
// model's DEFINITION is identical on both targets.
//
// Needs a real SQL Server; skips without one (CI runs one as a service).
func directLakeWarehouseModel(workspaceID, warehouseID string) []byte {
	return []byte(fmt.Sprintf(`{
  "name":"Gold","compatibilityLevel":1604,
  "model":{
    "expressions":[{"name":"DL_Warehouse","kind":"m","expression":"let Source = AzureStorage.DataLake(\"https://onelake.dfs.fabric.microsoft.com/%s/%s\", [HierarchicalNavigation=true]) in Source"}],
    "tables":[{"name":"Revenue","columns":[
      {"name":"Country","dataType":"string","sourceColumn":"country"},
      {"name":"Revenue","dataType":"double","sourceColumn":"revenue"}],
      "measures":[{"name":"Total Revenue","expression":"SUM(Revenue[Revenue])"}],
      "partitions":[{"name":"Revenue","mode":"directLake","source":{"type":"entity","entityName":"fct_daily_revenue","schemaName":"dbo","expressionSource":"DL_Warehouse"}}]}]
  }
}`, workspaceID, warehouseID))
}

func TestDirectLakeOverAWarehouseReadsItsTables(t *testing.T) {
	a, st := newAPI(t)
	db := testsupport.OpenMSSQL(t)
	ws := seedWorkspace(t, st)
	wh := seedItem(t, st, ws.ID, "Warehouse", "dw")
	a.SQLDB = func(context.Context, string) (*sql.DB, error) { return db, nil }

	// The table dbt would have built, in the warehouse rather than in OneLake.
	if _, err := db.Exec(`IF OBJECT_ID('dbo.fct_daily_revenue') IS NOT NULL DROP TABLE dbo.fct_daily_revenue;
		CREATE TABLE dbo.fct_daily_revenue (country VARCHAR(2), revenue FLOAT);
		INSERT INTO dbo.fct_daily_revenue VALUES ('US', 100.5), ('GB', 200.25), ('US', 9.25)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec("DROP TABLE dbo.fct_daily_revenue") })

	model := &store.Item{WorkspaceID: ws.ID, Type: "SemanticModel", DisplayName: "Gold"}
	parts := []store.DefinitionPart{{Path: "model.bim", PayloadType: "InlineBase64",
		Payload: base64.StdEncoding.EncodeToString(directLakeWarehouseModel(ws.ID, wh.ID))}}
	if err := st.CreateItem(model, parts); err != nil {
		t.Fatal(err)
	}

	query := `{"queries":[{"query":"EVALUATE SUMMARIZECOLUMNS(Revenue[Country], \"Total\", [Total Revenue])"}]}`
	w := do(a.executeQueries, admin, "POST", query, map[string]string{"datasetId": model.ID})
	if w.Code != 200 {
		t.Fatalf("query = %d %s", w.Code, w.Body)
	}
	// The rows came from SQL, through the same evaluator a Delta read feeds.
	for _, want := range []string{`"Revenue[Country]":"US"`, `"[Total]":109.75`, `"Revenue[Country]":"GB"`, `"[Total]":200.25`} {
		if !bytes.Contains(w.Body.Bytes(), []byte(want)) {
			t.Fatalf("response %s missing %q", w.Body, want)
		}
	}
}

// An entity name that is not a plain identifier must not reach the SELECT.
func TestDirectLakeWarehouseRefusesAnUnsafeEntityName(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	wh := seedItem(t, st, ws.ID, "Warehouse", "dw")
	a.SQLDB = func(context.Context, string) (*sql.DB, error) {
		t.Fatal("reached the database with an unsafe identifier")
		return nil, nil
	}
	_, err := a.readWarehouseTable(context.Background(), wh, dlPartition("fct]; DROP TABLE x --"))
	if err == nil {
		t.Fatal("accepted an entity name that is not an identifier")
	}
}

// Without a SQL engine, say so rather than reporting a model with no rows —
// which is the fabrication-class failure this surface exists to avoid.
func TestDirectLakeWarehouseWithoutSQLIsHonest(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	wh := seedItem(t, st, ws.ID, "Warehouse", "dw")
	a.SQLDB = nil
	if _, err := a.readWarehouseTable(context.Background(), wh, dlPartition("fct_daily_revenue")); err == nil {
		t.Fatal("claimed to read a warehouse with no SQL engine attached")
	}
}
