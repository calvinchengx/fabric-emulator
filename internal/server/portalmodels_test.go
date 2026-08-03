package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// newPortalServer is the smallest Server the portal handlers need: a store and
// a clock. No listener, no auth — these handlers read the store and shape JSON,
// and standing up the whole server would test the wiring instead of them.
func newPortalServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Server{Store: st, Clock: clock.New()}
}

func seedPortalWorkspace(t *testing.T, s *Server) *store.Workspace {
	t.Helper()
	ws := &store.Workspace{DisplayName: "contoso-analytics"}
	if err := s.Store.CreateWorkspace(ws, store.Principal{ID: "u", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	return ws
}

const tmslModel = `{
  "name": "ContosoRevenue",
  "compatibilityLevel": 1550,
  "model": {
    "tables": [
      {
        "name": "Customer",
        "columns": [{"name": "Country", "dataType": "string", "sourceColumn": "Country"}]
      },
      {
        "name": "Revenue",
        "columns": [
          {"name": "Revenue", "dataType": "double", "sourceColumn": "revenue"},
          {"name": "Country", "dataType": "string", "sourceColumn": "Country"}
        ],
        "measures": [{"name": "Total Revenue", "expression": "SUM(Revenue[Revenue])"}]
      }
    ],
    "relationships": [
      {"name": "Revenue_Customer", "fromTable": "Revenue", "fromColumn": "Country",
       "toTable": "Customer", "toColumn": "Country"}
    ]
  }
}`

func part(path, body string) store.DefinitionPart {
	return store.DefinitionPart{
		Path: path, PayloadType: "InlineBase64",
		Payload: base64.StdEncoding.EncodeToString([]byte(body)),
	}
}

func portalModelsOf(t *testing.T, s *Server) []map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	s.portalModels(w, httptest.NewRequest("GET", "/_emulator/portal/models", nil))
	if w.Code != 200 {
		t.Fatalf("portalModels = %d %s", w.Code, w.Body.Bytes())
	}
	var body struct {
		Value []map[string]any `json:"value"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Value
}

// TestPortalModelsDescribesWhatTheModelContains.
//
// The flow view draws a semantic model as one box. That is right for lineage
// and useless for "why is this measure returning nothing" — the answer to which
// is in the definition the emulator already parses on every query, and was
// visible nowhere.
func TestPortalModelsDescribesWhatTheModelContains(t *testing.T) {
	s := newPortalServer(t)
	ws := seedPortalWorkspace(t, s)
	it := &store.Item{WorkspaceID: ws.ID, Type: "SemanticModel", DisplayName: "revenue"}
	if err := s.Store.CreateItem(it, []store.DefinitionPart{
		part("model.bim", tmslModel),
		part("data.json", `{"Revenue": [{"Country": "GB", "Revenue": 1.0}]}`),
	}); err != nil {
		t.Fatal(err)
	}

	got := portalModelsOf(t, s)
	if len(got) != 1 {
		t.Fatalf("models = %v", got)
	}
	m := got[0]
	if m["format"] != "TMSL" || m["modelName"] != "ContosoRevenue" {
		t.Fatalf("model header = %v", m)
	}
	// data.json present -> an import model can actually answer a query. Without
	// it every DAX result is empty, which looks exactly like a wrong measure.
	if m["rowsLoaded"] != true {
		t.Errorf("rowsLoaded = %v, want true", m["rowsLoaded"])
	}
	tables, _ := m["tables"].([]any)
	if len(tables) != 2 {
		t.Fatalf("tables = %v", tables)
	}
	revenue, _ := tables[1].(map[string]any)
	if revenue["name"] != "Revenue" || revenue["mode"] != "import" {
		t.Fatalf("Revenue table = %v", revenue)
	}
	measures, _ := revenue["measures"].([]any)
	if len(measures) != 1 {
		t.Fatalf("measures = %v", measures)
	}
	// The DAX verbatim. A measure list without its expression tells you a
	// measure exists, which you already knew.
	if ms, _ := measures[0].(map[string]any); ms["expression"] != "SUM(Revenue[Revenue])" {
		t.Errorf("measure = %v", ms)
	}
	rels, _ := m["relationships"].([]any)
	if len(rels) != 1 {
		t.Fatalf("relationships = %v", rels)
	}
	if r, _ := rels[0].(map[string]any); r["from"] != "Revenue[Country]" || r["to"] != "Customer[Country]" {
		t.Errorf("relationship = %v", r)
	}
}

// TestPortalModelsReportsAnUnreadableModelRatherThanDroppingIt.
//
// A model missing from the list reads as "never published". That is a
// different problem from "published and unparseable", and sending someone to
// look for a publish step that already succeeded is the worst kind of wrong.
func TestPortalModelsReportsAnUnreadableModelRatherThanDroppingIt(t *testing.T) {
	s := newPortalServer(t)
	ws := seedPortalWorkspace(t, s)
	it := &store.Item{WorkspaceID: ws.ID, Type: "SemanticModel", DisplayName: "broken"}
	if err := s.Store.CreateItem(it, []store.DefinitionPart{part("model.bim", `{"not":`)}); err != nil {
		t.Fatal(err)
	}

	got := portalModelsOf(t, s)
	if len(got) != 1 {
		t.Fatalf("an unreadable model vanished from the list: %v", got)
	}
	if got[0]["error"] == nil || got[0]["error"] == "" {
		t.Fatalf("no error reported for an unparseable definition: %v", got[0])
	}
	if got[0]["displayName"] != "broken" {
		t.Errorf("the item should still be named: %v", got[0])
	}
}

// TestPortalModelsPrefersTMSLLikeTheQueryPathDoes.
//
// executeQueries answers from model.bim when a definition carries both. A
// portal that preferred the TMDL would describe, convincingly, a model nobody
// is querying.
func TestPortalModelsPrefersTMSLLikeTheQueryPathDoes(t *testing.T) {
	s := newPortalServer(t)
	ws := seedPortalWorkspace(t, s)
	it := &store.Item{WorkspaceID: ws.ID, Type: "SemanticModel", DisplayName: "both"}
	if err := s.Store.CreateItem(it, []store.DefinitionPart{
		part("model.bim", tmslModel),
		part("definition/model.tmdl", "createOrReplace\n\tmodel Model\n"),
	}); err != nil {
		t.Fatal(err)
	}

	got := portalModelsOf(t, s)
	if len(got) != 1 || got[0]["format"] != "TMSL" {
		t.Fatalf("format = %v, want TMSL (what the query path uses)", got[0]["format"])
	}
}

// TestPortalModelsListsOnlySemanticModels: a lakehouse is not a model, and a
// list that included one would make the count meaningless.
func TestPortalModelsListsOnlySemanticModels(t *testing.T) {
	s := newPortalServer(t)
	ws := seedPortalWorkspace(t, s)
	for _, typ := range []string{"Lakehouse", "Notebook", "Warehouse"} {
		if err := s.Store.CreateItem(&store.Item{
			WorkspaceID: ws.ID, Type: typ, DisplayName: "x-" + typ,
		}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := portalModelsOf(t, s); len(got) != 0 {
		t.Fatalf("non-model items appeared: %v", got)
	}
}
