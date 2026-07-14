package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
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
	if ran != 3 {
		t.Fatalf("ran %d DAX queries, want 3", ran)
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
