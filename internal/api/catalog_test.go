package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func TestCatalogSearchFindsAcrossWorkspaces(t *testing.T) {
	a, st := newAPI(t)
	sales := seedWorkspace(t, st)
	if err := st.UpdateWorkspace(&store.Workspace{ID: sales.ID, DisplayName: "Sales Analytics", Description: sales.Description, CapacityID: sales.CapacityID}); err != nil {
		t.Fatal(err)
	}
	other := &store.Workspace{DisplayName: "Finance Platform"}
	if err := st.CreateWorkspace(other, store.Principal{ID: admin.ID, Type: admin.Type}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateItem(&store.Item{WorkspaceID: sales.ID, Type: "Report", DisplayName: "Monthly Sales Revenue", Description: "Consolidated revenue"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateItem(&store.Item{WorkspaceID: other.ID, Type: "Lakehouse", DisplayName: "Sales Revenue Lakehouse", Description: "Central lakehouse"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateItem(&store.Item{WorkspaceID: sales.ID, Type: "Notebook", DisplayName: "scratch", Description: "unrelated"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateItem(&store.Item{WorkspaceID: sales.ID, Type: "Dashboard", DisplayName: "Sales Dashboard"}, nil); err != nil {
		t.Fatal(err)
	}

	w := do(a.searchCatalog, admin, "POST", `{"search":"Sales Revenue","filter":"Type eq 'Report' or Type eq 'Lakehouse'"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("search = %d %s", w.Code, w.Body.Bytes())
	}
	var got struct {
		Value []struct {
			Type, DisplayName, CatalogEntryType string
			Hierarchy                           struct {
				Workspace struct{ DisplayName string }
			}
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Value) != 2 {
		t.Fatalf("hits = %d (%s)", len(got.Value), w.Body.Bytes())
	}
	for _, e := range got.Value {
		if e.CatalogEntryType != "FabricItem" {
			t.Errorf("catalogEntryType = %q", e.CatalogEntryType)
		}
		if e.Type == "Dashboard" {
			t.Error("Dashboard must be excluded")
		}
	}

	// A principal with no grants sees nothing, even when items exist.
	if w := do(a.searchCatalog, nobody, "POST", `{"search":"Sales"}`, nil); w.Code != http.StatusOK {
		t.Fatalf("nobody = %d", w.Code)
	} else if body := w.Body.String(); body != `{"value":[]}`+"\n" {
		t.Fatalf("nobody body = %q", body)
	}

	// Search also matches the workspace display name, not only the item.
	w = do(a.searchCatalog, admin, "POST", `{"search":"Sales Analytics"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("workspace-name search = %d", w.Code)
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Value) == 0 {
		t.Fatalf("workspace-name search missed items: %s", w.Body.Bytes())
	}
}

func TestCatalogSearchFilterAndPaging(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	for _, name := range []string{"a", "b", "c"} {
		if err := st.CreateItem(&store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: name}, nil); err != nil {
			t.Fatal(err)
		}
	}
	w := do(a.searchCatalog, admin, "POST", `{"search":"","pageSize":2,"filter":"Type eq 'Notebook'"}`, nil)
	var page struct {
		Value             []struct{ DisplayName string }
		ContinuationToken string
	}
	_ = json.Unmarshal(w.Body.Bytes(), &page)
	if len(page.Value) != 2 || page.ContinuationToken == "" {
		t.Fatalf("page = %+v %s", page, w.Body.Bytes())
	}
	w = do(a.searchCatalog, admin, "POST", `{"pageSize":2,"continuationToken":"`+page.ContinuationToken+`"}`, nil)
	_ = json.Unmarshal(w.Body.Bytes(), &page)
	if len(page.Value) != 1 {
		t.Fatalf("page 2 = %d", len(page.Value))
	}

	if w := do(a.searchCatalog, admin, "POST", `{"pageSize":0}`, nil); w.Code != http.StatusOK {
		t.Fatalf("default pageSize 0 should become 50, got %d", w.Code)
	}
	if w := do(a.searchCatalog, admin, "POST", `{"pageSize":1001}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("oversize page = %d", w.Code)
	}
	if w := do(a.searchCatalog, admin, "POST", `{"filter":"nope"}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("bad filter = %d %s", w.Code, w.Body.Bytes())
	}
	if w := do(a.searchCatalog, admin, "POST", `{`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("malformed = %d", w.Code)
	}
	if w := do(a.searchCatalog, admin, "POST", "", nil); w.Code != http.StatusOK {
		t.Fatalf("empty body = %d", w.Code)
	}
	past := encodePageToken(100)
	w = do(a.searchCatalog, admin, "POST", `{"continuationToken":"`+past+`"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("past-end token = %d %s", w.Code, w.Body.Bytes())
	}
	var empty struct{ Value []struct{} }
	if err := json.Unmarshal(w.Body.Bytes(), &empty); err != nil || len(empty.Value) != 0 {
		t.Fatalf("past-end page = %s", w.Body.Bytes())
	}
}

func TestTypeFilterAllows(t *testing.T) {
	ok, err := typeFilterAllows("Type eq 'Lakehouse'", "Lakehouse")
	if err != nil || !ok {
		t.Fatalf("eq lakehouse: %v %v", ok, err)
	}
	ok, err = typeFilterAllows("Type ne 'Notebook'", "Lakehouse")
	if err != nil || !ok {
		t.Fatalf("ne notebook: %v %v", ok, err)
	}
	ok, err = typeFilterAllows("Type eq 'Report' or Type eq 'Lakehouse'", "Notebook")
	if err != nil || ok {
		t.Fatalf("or miss: %v %v", ok, err)
	}
	ok, err = typeFilterAllows("(Type eq 'Report' or Type eq 'Lakehouse') and Type ne 'Dashboard'", "Lakehouse")
	if err != nil || !ok {
		t.Fatalf("grouped: %v %v", ok, err)
	}
	ok, err = typeFilterAllows("Type ne 'Notebook'", "Notebook")
	if err != nil || ok {
		t.Fatalf("ne hit: %v %v", ok, err)
	}
	if _, err := typeFilterAllows("Type eq 'Lakehouse' extra", "Lakehouse"); err == nil {
		t.Fatal("trailing token should fail")
	}
	if _, err := typeFilterAllows("Type eq 'Lakehouse' or nope", "Lakehouse"); err == nil {
		t.Fatal("or rhs")
	}
	if _, err := typeFilterAllows("Type eq 'Lakehouse' and nope", "Lakehouse"); err == nil {
		t.Fatal("and rhs")
	}
	if _, err := typeFilterAllows("(Type eq 'Lakehouse'", "Lakehouse"); err == nil {
		t.Fatal("missing )")
	}
	if _, err := typeFilterAllows("Type eq 'Lakehouse", "Lakehouse"); err == nil {
		t.Fatal("unterminated")
	}
	if _, err := typeFilterAllows("Name eq 'x'", "Lakehouse"); err == nil {
		t.Fatal("unknown field")
	}
	ok, err = typeFilterAllows(`Type eq "Lakehouse"`, "Lakehouse")
	if err != nil || !ok {
		t.Fatalf("double quotes: %v %v", ok, err)
	}
	ok, err = typeFilterAllows("Type eq 'Report' and Type eq 'Lakehouse'", "Lakehouse")
	if err != nil || ok {
		t.Fatalf("and miss: %v %v", ok, err)
	}
	if _, err := typeFilterAllows("Type foo 'Lakehouse'", "Lakehouse"); err == nil {
		t.Fatal("missing eq/ne")
	}
	if _, err := typeFilterAllows("Typeeq 'Lakehouse'", "Lakehouse"); err == nil {
		t.Fatal("keyword boundary")
	}
	ok, err = typeFilterAllows("Type eq ''", "Lakehouse")
	if err != nil || ok {
		t.Fatalf("empty literal: %v %v", ok, err)
	}
}

func TestCatalogSearchExcludesDataflow(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	if err := st.CreateItem(&store.Item{WorkspaceID: ws.ID, Type: "Dataflow", DisplayName: "flow"}, nil); err != nil {
		t.Fatal(err)
	}
	w := do(a.searchCatalog, admin, "POST", `{"search":"flow"}`, nil)
	if w.Body.String() != `{"value":[]}`+"\n" {
		t.Fatalf("dataflow leaked: %s", w.Body.Bytes())
	}
}
