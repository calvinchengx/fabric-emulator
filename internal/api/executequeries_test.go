package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/semanticmodel"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/parquet-go/parquet-go"
)

func smFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "e2e", "semantic-model", "fixtures", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func createSemanticModel(t *testing.T, st *store.Store, wid string) *store.Item {
	t.Helper()
	part := func(path string, data []byte) store.DefinitionPart {
		return store.DefinitionPart{Path: path, PayloadType: "InlineBase64",
			Payload: base64.StdEncoding.EncodeToString(data)}
	}
	it := &store.Item{WorkspaceID: wid, Type: "SemanticModel", DisplayName: "RetailAnalysis"}
	parts := []store.DefinitionPart{
		part("model.bim", smFixture(t, "retail.bim")),
		part("data.json", smFixture(t, "seed_data.json")),
	}
	if err := st.CreateItem(it, parts); err != nil {
		t.Fatal(err)
	}
	return it
}

func rowsMatch(got, want []map[string]any) bool {
	if len(got) != len(want) {
		return false
	}
	used := make([]bool, len(got))
	for _, wrow := range want {
		found := false
		for i, grow := range got {
			if used[i] {
				continue
			}
			ok := true
			for k, wv := range wrow {
				if fmtVal(grow[k]) != fmtVal(wv) {
					ok = false
					break
				}
			}
			if ok {
				used[i], found = true, true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// fmtVal normalizes JSON numbers (60000 == 60000.0) and strings for comparison.
func fmtVal(v any) string {
	if f, ok := v.(float64); ok {
		return fmt.Sprintf("%g", f)
	}
	return fmt.Sprintf("%v", v)
}

func TestExecuteQueriesGolden(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	ds := createSemanticModel(t, st, ws.ID)

	var golden goldenExq
	if err := json.Unmarshal(smFixture(t, "golden_queries.json"), &golden); err != nil {
		t.Fatal(err)
	}
	ran := 0
	for _, q := range golden.Queries {
		if q.Handler != "dax" {
			continue
		}
		ran++
		body := map[string]any{"queries": []map[string]string{{"query": q.DAX}}}
		raw, _ := json.Marshal(body)
		w := do(a.executeQueries, admin, "POST", string(raw), map[string]string{"datasetId": ds.ID, "groupId": ws.ID})
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", q.Name, w.Code, w.Body.Bytes())
		}
		var resp struct {
			Results []struct {
				Tables []struct {
					Rows []map[string]any `json:"rows"`
				} `json:"tables"`
			} `json:"results"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if len(resp.Results) != 1 || len(resp.Results[0].Tables) != 1 {
			t.Fatalf("%s: unexpected response shape %s", q.Name, w.Body.Bytes())
		}
		if !rowsMatch(resp.Results[0].Tables[0].Rows, q.Expected.Rows) {
			t.Errorf("%s: rows mismatch\n got=%v\nwant=%v", q.Name, resp.Results[0].Tables[0].Rows, q.Expected.Rows)
		}
	}
	if ran != 6 {
		t.Fatalf("ran %d DAX queries, want 6", ran)
	}
}

// TestExecuteQueriesEveryPublishedMeasureAnswers is the witness for issue #42.
//
// A measure whose expression the evaluator cannot parse still publishes: the
// definition is stored verbatim and nothing reads the DAX until a query names
// the measure. So a model can be created, listed and shown complete in the
// portal while part of it is unqueryable — `Gross Revenue = [Revenue USD] +
// [Cancelled Revenue]` sat green for weeks in a downstream repo because nothing
// had asked for it. Creation is therefore no evidence at all; only a query is.
//
// This walks every measure in the published model, which keeps the guarantee
// tied to the fixture rather than to a hand-picked list: add a measure to
// retail.bim and it must answer here too.
func TestExecuteQueriesEveryPublishedMeasureAnswers(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	ds := createSemanticModel(t, st, ws.ID) // publish — proves nothing on its own

	model, err := semanticmodel.ParseTMSL(smFixture(t, "retail.bim"))
	if err != nil {
		t.Fatal(err)
	}
	var measures []string
	for _, tbl := range model.Tables {
		for _, m := range tbl.Measures {
			measures = append(measures, m.Name)
		}
	}
	if len(measures) < 6 {
		t.Fatalf("fixture has %d measures; expected the operator ones too", len(measures))
	}

	ask := func(t *testing.T, expr string) any {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"queries": []map[string]string{
			{"query": `EVALUATE SUMMARIZECOLUMNS("v", ` + expr + `)`}}})
		w := do(a.executeQueries, admin, "POST", string(raw), map[string]string{"datasetId": ds.ID, "groupId": ws.ID})
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", expr, w.Code, w.Body.Bytes())
		}
		var resp struct {
			Results []struct {
				Tables []struct {
					Rows []map[string]any `json:"rows"`
				} `json:"tables"`
			} `json:"results"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Results) != 1 || len(resp.Results[0].Tables) != 1 || len(resp.Results[0].Tables[0].Rows) != 1 {
			t.Fatalf("%s: unexpected response shape %s", expr, w.Body.Bytes())
		}
		return resp.Results[0].Tables[0].Rows[0]["[v]"]
	}

	for _, name := range measures {
		t.Run(name, func(t *testing.T) {
			if v := ask(t, "["+name+"]"); v == nil {
				t.Errorf("[%s] answered blank", name)
			}
		})
	}

	// The arithmetic itself, not just a 200: Σ TY − Σ LY = 4900 − 3800.
	if got := ask(t, "[Units Delta]"); fmt.Sprintf("%v", got) != "1100" {
		t.Errorf("[Units Delta] = %v, want 1100", got)
	}
	// And inline, so the operator is exercised from the query string too.
	if got := ask(t, "[Total Units This Year] - [Total Units Last Year]"); fmt.Sprintf("%v", got) != "1100" {
		t.Errorf("inline subtraction = %v, want 1100", got)
	}
}

type goldenExq struct {
	Queries []struct {
		Name     string `json:"name"`
		DAX      string `json:"dax"`
		Handler  string `json:"handler"`
		Expected struct {
			Rows []map[string]any `json:"rows"`
		} `json:"expected"`
	} `json:"queries"`
}

func TestExecuteQueriesErrorsAndRBAC(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	ds := createSemanticModel(t, st, ws.ID)
	q := `{"queries":[{"query":"EVALUATE 'Store'"}]}`

	// Viewer (seeded) can query; ungranted 403.
	if w := do(a.executeQueries, viewer, "POST", q, map[string]string{"datasetId": ds.ID}); w.Code != 200 {
		t.Fatalf("viewer query = %d", w.Code)
	}
	if w := do(a.executeQueries, &authNobody, "POST", q, map[string]string{"datasetId": ds.ID}); w.Code != 403 {
		t.Fatalf("ungranted = %d; want 403", w.Code)
	}

	// Unknown dataset / wrong type → 404.
	if w := do(a.executeQueries, admin, "POST", q, map[string]string{"datasetId": "nope"}); w.Code != 404 {
		t.Fatalf("unknown dataset = %d", w.Code)
	}
	nb := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "nb"}
	_ = st.CreateItem(nb, nil)
	if w := do(a.executeQueries, admin, "POST", q, map[string]string{"datasetId": nb.ID}); w.Code != 404 {
		t.Fatalf("non-semantic-model = %d; want 404", w.Code)
	}
	// Wrong group for the dataset → 404.
	if w := do(a.executeQueries, admin, "POST", q, map[string]string{"datasetId": ds.ID, "groupId": "other-ws"}); w.Code != 404 {
		t.Fatalf("wrong group = %d; want 404", w.Code)
	}

	// Bad request body / bad DAX → 400.
	if w := do(a.executeQueries, admin, "POST", `{"queries":[]}`, map[string]string{"datasetId": ds.ID}); w.Code != 400 {
		t.Fatalf("empty queries = %d; want 400", w.Code)
	}
	if w := do(a.executeQueries, admin, "POST", `{"queries":[{"query":"SELECT 1"}]}`, map[string]string{"datasetId": ds.ID}); w.Code != 400 {
		t.Fatalf("bad DAX = %d; want 400", w.Code)
	}

	// A SemanticModel with no model.bim → 400.
	empty := &store.Item{WorkspaceID: ws.ID, Type: "SemanticModel", DisplayName: "empty"}
	_ = st.CreateItem(empty, nil)
	if w := do(a.executeQueries, admin, "POST", q, map[string]string{"datasetId": empty.ID}); w.Code != 400 {
		t.Fatalf("no model.bim = %d; want 400", w.Code)
	}
}

// TestExecuteQueriesUnconfigured: without a Power BI validator the route 501s.
func TestExecuteQueriesUnconfigured(t *testing.T) {
	a, _ := newAPI(t)
	a.PBIAuth = nil
	r := httptest.NewRequest("POST", "/v1.0/myorg/datasets/x/executeQueries", nil)
	w := httptest.NewRecorder()
	a.withPBIAuth(a.executeQueries)(w, r)
	if w.Code != 501 {
		t.Fatalf("unconfigured = %d; want 501", w.Code)
	}
}

type directLakeSale struct {
	Region string `parquet:"region"`
	Amount int64  `parquet:"amount"`
}

func directLakeParquet(t *testing.T, rows []directLakeSale) []byte {
	t.Helper()
	var b bytes.Buffer
	w := parquet.NewGenericWriter[directLakeSale](&b)
	if _, err := w.Write(rows); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func directLakeModel(workspaceID, lakehouseID string) []byte {
	return []byte(fmt.Sprintf(`{
  "name":"DirectSales","compatibilityLevel":1604,
  "model":{
    "expressions":[{"name":"DL_Lakehouse","kind":"m","expression":"let Source = AzureStorage.DataLake(\"https://onelake.dfs.fabric.microsoft.com/%s/%s\", [HierarchicalNavigation=true]) in Source"}],
    "tables":[{"name":"Sales","columns":[
      {"name":"Region","dataType":"string","sourceColumn":"region"},
      {"name":"Amount","dataType":"int64","sourceColumn":"amount"}],
      "measures":[{"name":"Total","expression":"SUM(Sales[Amount])"}],
      "partitions":[{"name":"Sales","mode":"directLake","source":{"type":"entity","entityName":"sales","schemaName":"dbo","expressionSource":"DL_Lakehouse"}}]}]
  }
}`, workspaceID, lakehouseID))
}

func putDirectLakeFile(t *testing.T, st *store.Store, wid, iid, rel string, content []byte) {
	t.Helper()
	if err := st.CreateOneLakePath(&store.OneLakePath{WorkspaceID: wid, ItemID: iid, RelPath: rel, Content: content}, false); err != nil {
		t.Fatal(err)
	}
}

func TestExecuteQueriesDirectLakeReadsCurrentDelta(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "sales-lake"}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}
	putDirectLakeFile(t, st, ws.ID, lake.ID, "Tables/sales/part-0.parquet", directLakeParquet(t, []directLakeSale{{"us", 80}, {"eu", 60}}))
	putDirectLakeFile(t, st, ws.ID, lake.ID, "Tables/sales/_delta_log/00000000000000000000.json", []byte(`{"add":{"path":"part-0.parquet"}}`))
	model := &store.Item{WorkspaceID: ws.ID, Type: "SemanticModel", DisplayName: "Direct Sales"}
	parts := []store.DefinitionPart{{Path: "model.bim", PayloadType: "InlineBase64", Payload: base64.StdEncoding.EncodeToString(directLakeModel(ws.ID, lake.ID))}}
	if err := st.CreateItem(model, parts); err != nil {
		t.Fatal(err)
	}

	query := `{"queries":[{"query":"EVALUATE SUMMARIZECOLUMNS(Sales[Region], \"Total\", [Total])"}]}`
	assertRows := func(want ...string) {
		t.Helper()
		w := do(a.executeQueries, admin, "POST", query, map[string]string{"datasetId": model.ID})
		if w.Code != 200 {
			t.Fatalf("query=%d %s", w.Code, w.Body.String())
		}
		for _, value := range want {
			if !bytes.Contains(w.Body.Bytes(), []byte(value)) {
				t.Fatalf("response %s missing %q", w.Body.String(), value)
			}
		}
	}
	assertRows(`"Sales[Region]":"us"`, `"[Total]":80`, `"Sales[Region]":"eu"`, `"[Total]":60`)

	putDirectLakeFile(t, st, ws.ID, lake.ID, "Tables/sales/part-1.parquet", directLakeParquet(t, []directLakeSale{{"apac", 125}}))
	putDirectLakeFile(t, st, ws.ID, lake.ID, "Tables/sales/_delta_log/00000000000000000001.json", []byte("{\"remove\":{\"path\":\"part-0.parquet\"}}\n{\"add\":{\"path\":\"part-1.parquet\"}}"))
	assertRows(`"Sales[Region]":"apac"`, `"[Total]":125`)
}

func TestDirectLakeErrorsAndSourceRBAC(t *testing.T) {
	a, st := newAPI(t)
	modelWS := seedWorkspace(t, st)
	sourceWS := &store.Workspace{DisplayName: "source"}
	if err := st.CreateWorkspace(sourceWS, store.Principal{ID: admin.ID, Type: admin.Type}); err != nil {
		t.Fatal(err)
	}
	lake := &store.Item{WorkspaceID: sourceWS.ID, Type: "Lakehouse", DisplayName: "lake"}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}
	putDirectLakeFile(t, st, sourceWS.ID, lake.ID, "Tables/sales/part.parquet", directLakeParquet(t, []directLakeSale{{"x", 1}}))
	putDirectLakeFile(t, st, sourceWS.ID, lake.ID, "Tables/sales/_delta_log/00000000000000000000.json", []byte(`{"add":{"path":"part.parquet"}}`))
	model := &store.Item{WorkspaceID: modelWS.ID, Type: "SemanticModel", DisplayName: "cross"}
	parts := []store.DefinitionPart{{Path: "model.bim", PayloadType: "InlineBase64", Payload: base64.StdEncoding.EncodeToString(directLakeModel(sourceWS.ID, lake.ID))}}
	if err := st.CreateItem(model, parts); err != nil {
		t.Fatal(err)
	}
	q := `{"queries":[{"query":"EVALUATE Sales"}]}`
	if w := do(a.executeQueries, viewer, "POST", q, map[string]string{"datasetId": model.ID}); w.Code != 400 || !bytes.Contains(w.Body.Bytes(), []byte("cannot read source workspace")) {
		t.Fatalf("source RBAC=%d %s", w.Code, w.Body.String())
	}

	bad := directLakeModel(sourceWS.ID, lake.ID)
	bad = bytes.Replace(bad, []byte("onelake.dfs.fabric.microsoft.com"), []byte("example.invalid"), 1)
	if err := st.SetDefinition(model.ID, []store.DefinitionPart{{Path: "model.bim", PayloadType: "InlineBase64", Payload: base64.StdEncoding.EncodeToString(bad)}}); err != nil {
		t.Fatal(err)
	}
	if w := do(a.executeQueries, admin, "POST", q, map[string]string{"datasetId": model.ID}); w.Code != 400 || !bytes.Contains(w.Body.Bytes(), []byte("shared expression")) {
		t.Fatalf("bad expression=%d %s", w.Code, w.Body.String())
	}
}

func TestDirectLakeSchemaQualifiedTableFallback(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "schema-lake"}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}
	root := "Tables/sales_schema/sales"
	putDirectLakeFile(t, st, ws.ID, lake.ID, root+"/part.parquet", directLakeParquet(t, []directLakeSale{{"us", 9}}))
	putDirectLakeFile(t, st, ws.ID, lake.ID, root+"/_delta_log/00000000000000000000.json", []byte(`{"add":{"path":"part.parquet"}}`))
	bim := bytes.Replace(directLakeModel(ws.ID, lake.ID), []byte(`"schemaName":"dbo"`), []byte(`"schemaName":"sales_schema"`), 1)
	model := &store.Item{WorkspaceID: ws.ID, Type: "SemanticModel", DisplayName: "schema model"}
	if err := st.CreateItem(model, []store.DefinitionPart{{Path: "model.bim", PayloadType: "InlineBase64", Payload: base64.StdEncoding.EncodeToString(bim)}}); err != nil {
		t.Fatal(err)
	}
	w := do(a.executeQueries, admin, "POST", `{"queries":[{"query":"EVALUATE Sales"}]}`, map[string]string{"datasetId": model.ID})
	if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(`"Sales[Amount]":9`)) {
		t.Fatalf("schema-qualified Direct Lake = %d %s", w.Code, w.Body.String())
	}
}
