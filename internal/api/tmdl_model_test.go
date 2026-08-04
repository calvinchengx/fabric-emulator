package api

// Querying a model that was published as TMDL rather than TMSL.
//
// Fabric accepts both serialisations and loadSemanticModel says so, but every
// test here published `model.bim` — so the TMDL half of parseModelDefinition
// had never executed. internal/semanticmodel proves ParseTMDL is correct in
// isolation; nothing proved the query surface can reach it. A `.pbip` project
// from Power BI Desktop is TMDL, so this is the serialisation a real client is
// most likely to arrive with.

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// A minimal model in TMDL, in the folder layout Power BI Desktop writes.
var tmdlParts = map[string]string{
	"definition/model.tmdl": "model SalesModel\n\tculture: en-US\n\tcompatibilityLevel: 1550\n",
	"definition/tables/Sales.tmdl": "table Sales\n" +
		"\n\tcolumn Region\n\t\tdataType: string\n\t\tsourceColumn: Region\n" +
		"\n\tcolumn Amount\n\t\tdataType: double\n\t\tsourceColumn: Amount\n" +
		"\n\tmeasure 'Total Amount' = SUM(Sales[Amount])\n",
}

const tmdlData = `{"Sales":[
  {"Region":"us","Amount":80},
  {"Region":"us","Amount":20},
  {"Region":"eu","Amount":60}]}`

// createTMDLModel publishes a SemanticModel whose definition carries ONLY
// .tmdl parts — no model.bim — plus the rows to evaluate against.
func createTMDLModel(t *testing.T, st *store.Store, wid, name string, extra ...store.DefinitionPart) *store.Item {
	t.Helper()
	part := func(path, data string) store.DefinitionPart {
		return store.DefinitionPart{Path: path, PayloadType: "InlineBase64",
			Payload: base64.StdEncoding.EncodeToString([]byte(data))}
	}
	parts := []store.DefinitionPart{part("data.json", tmdlData)}
	for path, body := range tmdlParts {
		parts = append(parts, part(path, body))
	}
	parts = append(parts, extra...)
	it := &store.Item{WorkspaceID: wid, Type: "SemanticModel", DisplayName: name}
	if err := st.CreateItem(it, parts); err != nil {
		t.Fatal(err)
	}
	return it
}

// daxRows runs one DAX query and returns the result rows. includeNulls matters
// whenever the assertion is about which COLUMNS the model has: rowsToJSON drops
// null cells, so a column that exists but has no data is invisible without it.
func daxRows(t *testing.T, a *API, dsID, dax string, includeNulls ...bool) []map[string]any {
	t.Helper()
	req := map[string]any{"queries": []map[string]string{{"query": dax}}}
	if len(includeNulls) > 0 && includeNulls[0] {
		req["serializerSettings"] = map[string]any{"includeNulls": true}
	}
	body, _ := json.Marshal(req)
	w := do(a.executeQueries, admin, "POST", string(body), map[string]string{"datasetId": dsID})
	if w.Code != 200 {
		t.Fatalf("executeQueries(%s) = %d %s", dax, w.Code, w.Body.Bytes())
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
	if len(resp.Results) != 1 || len(resp.Results[0].Tables) != 1 {
		t.Fatalf("unexpected response shape: %s", w.Body.Bytes())
	}
	return resp.Results[0].Tables[0].Rows
}

// TestExecuteQueriesAnswersFromATMDLModel is the witness the TMDL branch never
// had: a model published as a .tmdl folder answers real DAX.
func TestExecuteQueriesAnswersFromATMDLModel(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	ds := createTMDLModel(t, st, ws.ID, "FromTMDL")

	// The table and its columns came through the TMDL parser.
	rows := daxRows(t, a, ds.ID, "EVALUATE Sales")
	if len(rows) != 3 {
		t.Fatalf("EVALUATE Sales returned %d row(s); want 3: %v", len(rows), rows)
	}

	// The MEASURE came through too — a parser that read tables but dropped
	// measures would pass the assertion above and fail every real query.
	rows = daxRows(t, a, ds.ID,
		`EVALUATE SUMMARIZECOLUMNS(Sales[Region], "Total", [Total Amount])`)
	got := map[string]string{}
	for _, r := range rows {
		region, _ := r["Sales[Region]"].(string)
		got[region] = fmtVal(r["[Total]"])
	}
	if got["us"] != "100" || got["eu"] != "60" {
		t.Fatalf("measure over a TMDL model = %v; want us=100 eu=60 (raw rows %v)", got, rows)
	}
}

// TestExecuteQueriesPrefersTMSLOverTMDL pins the documented precedence. A
// definition carrying both is answered by model.bim, and the comment is
// explicit that this is not a place to start guessing — so the test uses a TMDL
// that would give a DIFFERENT answer, which is the only way to tell which
// parser actually ran.
func TestExecuteQueriesPrefersTMSLOverTMDL(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)

	// model.bim describes the same table with an extra column the TMDL lacks.
	bim := `{"name":"Both","compatibilityLevel":1550,"model":{"tables":[
	  {"name":"Sales","columns":[
	    {"name":"Region","dataType":"string","sourceColumn":"Region"},
	    {"name":"Amount","dataType":"double","sourceColumn":"Amount"},
	    {"name":"OnlyInTMSL","dataType":"string","sourceColumn":"OnlyInTMSL"}]}]}}`
	ds := createTMDLModel(t, st, ws.ID, "Both", store.DefinitionPart{
		Path: "model.bim", PayloadType: "InlineBase64",
		Payload: base64.StdEncoding.EncodeToString([]byte(bim)),
	})

	rows := daxRows(t, a, ds.ID, "EVALUATE Sales", true)
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
	// The TMDL declares no such column, so its presence is proof that model.bim
	// was the definition actually parsed.
	if _, ok := rows[0]["Sales[OnlyInTMSL]"]; !ok {
		var keys []string
		for k := range rows[0] {
			keys = append(keys, k)
		}
		t.Fatalf("the TMDL parser answered a definition that carries model.bim; "+
			"columns were %v", keys)
	}
}

// TestExecuteQueriesRejectsADefinitionWithNoModel: a SemanticModel whose
// definition carries neither serialisation must say so. Falling through to an
// empty model would answer every query with no rows, which reads as "the data
// is missing" rather than "the model is".
func TestExecuteQueriesRejectsADefinitionWithNoModel(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	it := &store.Item{WorkspaceID: ws.ID, Type: "SemanticModel", DisplayName: "Empty"}
	if err := st.CreateItem(it, []store.DefinitionPart{{
		Path: "data.json", PayloadType: "InlineBase64",
		Payload: base64.StdEncoding.EncodeToString([]byte(tmdlData)),
	}}); err != nil {
		t.Fatal(err)
	}

	w := do(a.executeQueries, admin, "POST", `{"queries":[{"query":"EVALUATE Sales"}]}`,
		map[string]string{"datasetId": it.ID})
	if w.Code != 400 {
		t.Fatalf("status = %d; want 400", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, "no model.bim and no .tmdl parts") {
		t.Errorf("the error does not name what is missing: %s", body)
	}
}

// TestExecuteQueriesReportsAnUndecodableTMDLPart: a definition part whose
// payload is not valid base64 is a corrupt upload, not an absent model, and
// must not be silently skipped into "no .tmdl parts".
func TestExecuteQueriesReportsAnUndecodableTMDLPart(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	it := &store.Item{WorkspaceID: ws.ID, Type: "SemanticModel", DisplayName: "Corrupt"}
	if err := st.CreateItem(it, []store.DefinitionPart{{
		Path: "definition/tables/Sales.tmdl", PayloadType: "InlineBase64",
		Payload: "!!!! not base64 !!!!",
	}}); err != nil {
		t.Fatal(err)
	}

	w := do(a.executeQueries, admin, "POST", `{"queries":[{"query":"EVALUATE Sales"}]}`,
		map[string]string{"datasetId": it.ID})
	if w.Code != 400 {
		t.Fatalf("status = %d; want 400", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, "decoding definition/tables/Sales.tmdl") {
		t.Errorf("the error does not name the unreadable part: %s", body)
	}
}

// TestExecuteQueriesReportsUnparseableTMDL: TMDL that decodes but does not
// parse is reported rather than turned into an empty model.
func TestExecuteQueriesReportsUnparseableTMDL(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	it := &store.Item{WorkspaceID: ws.ID, Type: "SemanticModel", DisplayName: "Garbage"}
	if err := st.CreateItem(it, []store.DefinitionPart{{
		Path: "definition/tables/Sales.tmdl", PayloadType: "InlineBase64",
		Payload: base64.StdEncoding.EncodeToString([]byte("this is not tmdl at all\n")),
	}}); err != nil {
		t.Fatal(err)
	}

	w := do(a.executeQueries, admin, "POST", `{"queries":[{"query":"EVALUATE Sales"}]}`,
		map[string]string{"datasetId": it.ID})
	// Either a parse error or an empty-model error is acceptable; answering 200
	// with no rows is not — that reports missing DATA for a broken MODEL.
	if w.Code == 200 {
		t.Fatalf("unparseable TMDL answered 200: %s", w.Body.Bytes())
	}
}

// TestExecuteQueriesReadsASingleFileTMDLModel is the regression test for a
// defect this coverage pass found. definitionPart falls back to an item's SOLE
// definition part when the requested path is absent — right for notebooks,
// whose content part is named inconsistently, and wrong here: a model whose
// definition is one .tmdl file answered the `model.bim` lookup with the TMDL,
// which then failed as TMSL. The user saw a JSON parse error about a file that
// contains no JSON, and the TMDL branch was never reached.
//
// The same model in TWO .tmdl files always worked, which is why nothing caught
// it: every fixture with a real TMDL folder has more than one part.
func TestExecuteQueriesReadsASingleFileTMDLModel(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)

	only := "table Sales\n" +
		"\n\tcolumn Region\n\t\tdataType: string\n\t\tsourceColumn: Region\n" +
		"\n\tcolumn Amount\n\t\tdataType: double\n\t\tsourceColumn: Amount\n"
	it := &store.Item{WorkspaceID: ws.ID, Type: "SemanticModel", DisplayName: "SingleFile"}
	if err := st.CreateItem(it, []store.DefinitionPart{{
		Path: "definition/tables/Sales.tmdl", PayloadType: "InlineBase64",
		Payload: base64.StdEncoding.EncodeToString([]byte(only)),
	}}); err != nil {
		t.Fatal(err)
	}

	m, err := a.parseModelDefinition(it.ID)
	if err != nil {
		t.Fatalf("a definition holding one valid .tmdl file did not parse: %v", err)
	}
	if len(m.Tables) != 1 || m.Tables[0].Name != "Sales" {
		t.Fatalf("parsed model = %+v; want the single Sales table", m.Tables)
	}
}
