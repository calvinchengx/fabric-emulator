package api

// Deployment pipelines D0 (docs/23): the model and its read surface.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// seedPipeline creates a pipeline owned by admin and returns it with its stages.
func seedPipeline(t *testing.T, a *API, body string) (*store.DeploymentPipeline, []*store.DeploymentStage) {
	t.Helper()
	if body == "" {
		body = `{"displayName":"release"}`
	}
	w := do(a.createDeploymentPipeline, admin, "POST", body, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body)
	}
	pl := &store.DeploymentPipeline{}
	if err := json.Unmarshal(w.Body.Bytes(), pl); err != nil {
		t.Fatal(err)
	}
	sts, err := a.Store.ListDeploymentStages(pl.ID)
	if err != nil {
		t.Fatal(err)
	}
	return pl, sts
}

// page decodes a writePage envelope.
func page[T any](t *testing.T, b []byte) []T {
	t.Helper()
	var env struct {
		Value []T `json:"value"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatal(err)
	}
	return env.Value
}

// TestCreateDeploymentPipelineDefaultStages: omitting stages seeds the
// documented default three, in order, unassigned.
func TestCreateDeploymentPipelineDefaultStages(t *testing.T) {
	a, _ := newAPI(t)
	pl, sts := seedPipeline(t, a, "")
	if pl.ID == "" || pl.DisplayName != "release" {
		t.Fatalf("pipeline = %+v", pl)
	}
	if len(sts) != store.DefaultStages {
		t.Fatalf("stages = %d, want %d", len(sts), store.DefaultStages)
	}
	for i, st := range sts {
		if st.Order != i {
			t.Errorf("stage %d order = %d", i, st.Order)
		}
		if st.DisplayName != store.DefaultStageNames[i] {
			t.Errorf("stage %d name = %q, want %q", i, st.DisplayName, store.DefaultStageNames[i])
		}
		if st.WorkspaceID != "" {
			t.Errorf("stage %d is assigned at create: %q", i, st.WorkspaceID)
		}
	}
}

// TestCreateDeploymentPipelineExplicitStages: caller-supplied stages are kept
// in order and renumbered densely from 0.
func TestCreateDeploymentPipelineExplicitStages(t *testing.T) {
	a, _ := newAPI(t)
	_, sts := seedPipeline(t, a, `{"displayName":"two-stage","stages":[
		{"displayName":"Dev","description":"d"},{"displayName":"Prod","isPublic":true}]}`)
	if len(sts) != 2 {
		t.Fatalf("stages = %d", len(sts))
	}
	if sts[0].DisplayName != "Dev" || sts[0].Description != "d" || sts[0].Order != 0 {
		t.Errorf("stage 0 = %+v", sts[0])
	}
	if sts[1].DisplayName != "Prod" || !sts[1].IsPublic || sts[1].Order != 1 {
		t.Errorf("stage 1 = %+v", sts[1])
	}
}

// TestCreateDeploymentPipelineStageBounds: 2–10 stages, enforced at the API
// (400) and independently at the store (ErrStageCount).
func TestCreateDeploymentPipelineStageBounds(t *testing.T) {
	a, st := newAPI(t)
	one := `{"displayName":"x","stages":[{"displayName":"only"}]}`
	if w := do(a.createDeploymentPipeline, admin, "POST", one, nil); w.Code != http.StatusBadRequest {
		t.Errorf("1 stage = %d, want 400", w.Code)
	}
	many := `{"displayName":"x","stages":[`
	for i := 0; i < store.MaxStages+1; i++ {
		if i > 0 {
			many += ","
		}
		many += `{"displayName":"s"}`
	}
	many += `]}`
	if w := do(a.createDeploymentPipeline, admin, "POST", many, nil); w.Code != http.StatusBadRequest {
		t.Errorf("11 stages = %d, want 400", w.Code)
	}
	// The store refuses independently of the handler's pre-check.
	err := st.CreateDeploymentPipeline(&store.DeploymentPipeline{DisplayName: "x"},
		[]*store.DeploymentStage{{DisplayName: "only"}}, store.Principal{ID: admin.ID})
	if err == nil {
		t.Error("store accepted a 1-stage pipeline")
	}
}

func TestCreateDeploymentPipelineBadRequests(t *testing.T) {
	a, _ := newAPI(t)
	for name, body := range map[string]string{
		"no displayName":  `{}`,
		"malformed":       `{`,
		"stage unnamed":   `{"displayName":"x","stages":[{"displayName":"a"},{"description":"b"}]}`,
		"empty stage set": `{"displayName":"x","stages":[]}`,
	} {
		w := do(a.createDeploymentPipeline, admin, "POST", body, nil)
		// An empty stages array falls back to the default three, so it is the
		// one case here that succeeds.
		want := http.StatusBadRequest
		if name == "empty stage set" {
			want = http.StatusCreated
		}
		if w.Code != want {
			t.Errorf("%s = %d, want %d", name, w.Code, want)
		}
	}
}

// TestDeploymentPipelineAccessIsPerPipeline: List shows only pipelines the
// caller holds a role on, and a non-member gets 404 (not 403) on every read.
func TestDeploymentPipelineAccessIsPerPipeline(t *testing.T) {
	a, _ := newAPI(t)
	pl, sts := seedPipeline(t, a, "")

	w := do(a.listDeploymentPipelines, admin, "GET", "", nil)
	if got := page[store.DeploymentPipeline](t, w.Body.Bytes()); len(got) != 1 || got[0].ID != pl.ID {
		t.Fatalf("admin list = %+v", got)
	}
	if got := page[store.DeploymentPipeline](t,
		do(a.listDeploymentPipelines, nobody, "GET", "", nil).Body.Bytes()); len(got) != 0 {
		t.Errorf("non-member list = %+v, want empty", got)
	}

	ids := map[string]string{"pid": pl.ID, "sid": sts[0].ID}
	for name, h := range map[string]handler{
		"get":         a.getDeploymentPipeline,
		"update":      a.updateDeploymentPipeline,
		"delete":      a.deleteDeploymentPipeline,
		"stages":      a.listDeploymentStages,
		"stage":       a.getDeploymentStage,
		"stage items": a.listDeploymentStageItems,
	} {
		if w := do(h, nobody, "GET", `{}`, ids); w.Code != http.StatusNotFound {
			t.Errorf("non-member %s = %d, want 404", name, w.Code)
		}
	}
}

// TestDeploymentReadHandlersHappyPath drives the read surface through the
// handlers rather than the store, so the success paths are actually exercised
// (seedPipeline reads stages via the store, which would otherwise leave every
// 200 branch here untested).
func TestDeploymentReadHandlersHappyPath(t *testing.T) {
	a, _ := newAPI(t)
	pl, sts := seedPipeline(t, a, "")
	ids := map[string]string{"pid": pl.ID, "sid": sts[1].ID}

	w := do(a.getDeploymentPipeline, admin, "GET", "", ids)
	if w.Code != http.StatusOK {
		t.Fatalf("get pipeline = %d", w.Code)
	}
	got := &store.DeploymentPipeline{}
	if err := json.Unmarshal(w.Body.Bytes(), got); err != nil || got.ID != pl.ID {
		t.Fatalf("get pipeline body = %s", w.Body)
	}

	w = do(a.listDeploymentStages, admin, "GET", "", ids)
	if w.Code != http.StatusOK {
		t.Fatalf("list stages = %d", w.Code)
	}
	stages := page[store.DeploymentStage](t, w.Body.Bytes())
	if len(stages) != store.DefaultStages {
		t.Fatalf("list stages = %+v", stages)
	}
	for i, st := range stages {
		if st.Order != i || st.DisplayName != store.DefaultStageNames[i] {
			t.Errorf("stage %d over the wire = %+v", i, st)
		}
	}

	w = do(a.getDeploymentStage, admin, "GET", "", ids)
	if w.Code != http.StatusOK {
		t.Fatalf("get stage = %d", w.Code)
	}
	one := &store.DeploymentStage{}
	if err := json.Unmarshal(w.Body.Bytes(), one); err != nil {
		t.Fatal(err)
	}
	if one.ID != sts[1].ID || one.Order != 1 {
		t.Fatalf("get stage body = %+v", one)
	}
}

func TestDeploymentPipelineNotFound(t *testing.T) {
	a, _ := newAPI(t)
	pl, _ := seedPipeline(t, a, "")
	if w := do(a.getDeploymentPipeline, admin, "GET", "", map[string]string{"pid": "nope"}); w.Code != http.StatusNotFound {
		t.Errorf("unknown pipeline = %d", w.Code)
	}
	w := do(a.getDeploymentStage, admin, "GET", "", map[string]string{"pid": pl.ID, "sid": "nope"})
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown stage = %d", w.Code)
	}
}

// TestUpdateDeploymentPipelinePatchSemantics: an absent field is left alone,
// so a rename doesn't blank the description.
func TestUpdateDeploymentPipelinePatchSemantics(t *testing.T) {
	a, _ := newAPI(t)
	pl, _ := seedPipeline(t, a, `{"displayName":"release","description":"keep me"}`)
	ids := map[string]string{"pid": pl.ID}

	w := do(a.updateDeploymentPipeline, admin, "PATCH", `{"displayName":"renamed"}`, ids)
	if w.Code != http.StatusOK {
		t.Fatalf("patch = %d: %s", w.Code, w.Body)
	}
	got := &store.DeploymentPipeline{}
	json.Unmarshal(w.Body.Bytes(), got)
	if got.DisplayName != "renamed" || got.Description != "keep me" {
		t.Fatalf("patch lost a field: %+v", got)
	}
	if w := do(a.updateDeploymentPipeline, admin, "PATCH", `{"displayName":""}`, ids); w.Code != http.StatusBadRequest {
		t.Errorf("empty displayName = %d, want 400", w.Code)
	}
	if w := do(a.updateDeploymentPipeline, admin, "PATCH", `{`, ids); w.Code != http.StatusBadRequest {
		t.Errorf("malformed = %d, want 400", w.Code)
	}
}

func TestUpdateDeploymentStage(t *testing.T) {
	a, _ := newAPI(t)
	pl, sts := seedPipeline(t, a, "")
	ids := map[string]string{"pid": pl.ID, "sid": sts[0].ID}

	w := do(a.updateDeploymentStage, admin, "PATCH", `{"displayName":"Dev","isPublic":true}`, ids)
	if w.Code != http.StatusOK {
		t.Fatalf("patch stage = %d: %s", w.Code, w.Body)
	}
	got, err := a.Store.GetDeploymentStage(pl.ID, sts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != "Dev" || !got.IsPublic || got.Order != 0 {
		t.Fatalf("stage = %+v", got)
	}
	if w := do(a.updateDeploymentStage, admin, "PATCH", `{"displayName":""}`, ids); w.Code != http.StatusBadRequest {
		t.Errorf("empty displayName = %d, want 400", w.Code)
	}
	if w := do(a.updateDeploymentStage, admin, "PATCH", `{`, ids); w.Code != http.StatusBadRequest {
		t.Errorf("malformed = %d, want 400", w.Code)
	}
}

// TestListDeploymentStageItems: an unassigned stage is an empty page, not an
// error — a freshly created pipeline is in exactly that state. Once a
// workspace is assigned, its items show through.
func TestListDeploymentStageItems(t *testing.T) {
	a, st := newAPI(t)
	pl, sts := seedPipeline(t, a, "")
	ids := map[string]string{"pid": pl.ID, "sid": sts[0].ID}

	w := do(a.listDeploymentStageItems, admin, "GET", "", ids)
	if w.Code != http.StatusOK {
		t.Fatalf("unassigned stage = %d", w.Code)
	}
	if got := page[store.Item](t, w.Body.Bytes()); len(got) != 0 {
		t.Fatalf("unassigned stage items = %+v, want empty", got)
	}

	ws := seedWorkspace(t, st)
	item := &store.Item{WorkspaceID: ws.ID, DisplayName: "nb", Type: "Notebook"}
	if err := st.CreateItem(item, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.SetStageWorkspace(pl.ID, sts[0].ID, ws.ID); err != nil {
		t.Fatal(err)
	}
	got := page[store.Item](t, do(a.listDeploymentStageItems, admin, "GET", "", ids).Body.Bytes())
	if len(got) != 1 || got[0].DisplayName != "nb" {
		t.Fatalf("assigned stage items = %+v", got)
	}

	// The stage now reports the workspace, and its name is resolved live.
	stage, err := a.Store.GetDeploymentStage(pl.ID, sts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if stage.WorkspaceID != ws.ID || stage.WorkspaceName != ws.DisplayName {
		t.Fatalf("stage assignment = %+v", stage)
	}
}

// TestStageWorkspaceUnassignsOnWorkspaceDelete: the FK is ON DELETE SET NULL,
// so deleting the workspace leaves the stage unassigned rather than pointing
// at a workspace that no longer exists.
func TestStageWorkspaceUnassignsOnWorkspaceDelete(t *testing.T) {
	a, st := newAPI(t)
	pl, sts := seedPipeline(t, a, "")
	ws := seedWorkspace(t, st)
	if err := st.SetStageWorkspace(pl.ID, sts[0].ID, ws.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteWorkspace(ws.ID); err != nil {
		t.Fatal(err)
	}
	stage, err := st.GetDeploymentStage(pl.ID, sts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if stage.WorkspaceID != "" {
		t.Fatalf("stage still assigned to a deleted workspace: %+v", stage)
	}
	// And the items read stays a clean empty page rather than a 500.
	w := do(a.listDeploymentStageItems, admin, "GET", "",
		map[string]string{"pid": pl.ID, "sid": sts[0].ID})
	if w.Code != http.StatusOK {
		t.Fatalf("items after workspace delete = %d: %s", w.Code, w.Body)
	}
}

// TestDeleteDeploymentPipelineCascades: stages and roles go with it.
func TestDeleteDeploymentPipelineCascades(t *testing.T) {
	a, st := newAPI(t)
	pl, _ := seedPipeline(t, a, "")
	if w := do(a.deleteDeploymentPipeline, admin, "DELETE", "", map[string]string{"pid": pl.ID}); w.Code != http.StatusOK {
		t.Fatalf("delete = %d", w.Code)
	}
	if _, err := st.GetDeploymentPipeline(pl.ID); err == nil {
		t.Error("pipeline survived delete")
	}
	sts, err := st.ListDeploymentStages(pl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sts) != 0 {
		t.Errorf("stages survived delete: %+v", sts)
	}
	if _, err := st.DeploymentPipelineRole(pl.ID, admin.ID); err == nil {
		t.Error("role survived delete")
	}
}

// TestDeploymentStoreErrors: a closed store surfaces as 500s, not panics.
func TestDeploymentStoreErrors(t *testing.T) {
	a, st := newAPI(t)
	pl, sts := seedPipeline(t, a, "")
	ws := seedWorkspace(t, st)
	if err := st.SetStageWorkspace(pl.ID, sts[0].ID, ws.ID); err != nil {
		t.Fatal(err)
	}
	st.Close()
	ids := map[string]string{"pid": pl.ID, "sid": sts[0].ID}
	for name, h := range map[string]handler{
		"list":   a.listDeploymentPipelines,
		"create": a.createDeploymentPipeline,
		"get":    a.getDeploymentPipeline,
		"stages": a.listDeploymentStages,
	} {
		body := ""
		if name == "create" {
			body = `{"displayName":"x"}`
		}
		if w := do(h, admin, "POST", body, ids); w.Code != http.StatusInternalServerError {
			t.Errorf("%s on a closed store = %d, want 500", name, w.Code)
		}
	}
}
