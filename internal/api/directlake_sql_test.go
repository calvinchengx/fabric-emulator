package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// Direct Lake on SQL, stage 1: a Sql.Database expression is RECOGNISED and
// refused by name. It used to fail as "shared expression must contain an
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
		"one SQL source":                      {[]string{sqlEndpointExpr, sqlEndpointExpr}, "Direct Lake on SQL (Sql.Database) is recognised but not served"},
		"the same source by another spelling": {[]string{sqlEndpointExpr, `Sql.Database("x", "803C8E33-C35C-4F1B-9B44-F40DCE69E75E")`}, "is recognised but not served"},
		"two SQL sources":                     {[]string{sqlEndpointExpr, otherSQLExpr}, "Direct Lake on SQL uses a single source"},
		"both flavours":                       {[]string{oneLakeExpr, sqlEndpointExpr}, "mixes Direct Lake on OneLake and Direct Lake on SQL"},
		"an expression of neither flavour":    {[]string{unparseableExpr}, "neither Direct Lake on OneLake"},
		"a missing expression":                {[]string{""}, `references missing expression \"E0\"`},
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
