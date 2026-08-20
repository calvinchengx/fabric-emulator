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
	_ = json.Unmarshal(w.Body.Bytes(), got)
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

// TestAssignStageWorkspace: the happy path returns the persisted stage, and
// the caller must be able to administer the workspace they are attaching —
// pipeline access alone must not let anyone pull someone else's workspace
// into a promotion path.
func TestAssignStageWorkspace(t *testing.T) {
	a, st := newAPI(t)
	pl, sts := seedPipeline(t, a, "")
	ws := seedWorkspace(t, st) // admin is Admin, viewer is Viewer
	ids := map[string]string{"pid": pl.ID, "sid": sts[0].ID}

	w := do(a.assignStageWorkspace, admin, "POST", `{"workspaceId":"`+ws.ID+`"}`, ids)
	if w.Code != http.StatusOK {
		t.Fatalf("assign = %d: %s", w.Code, w.Body)
	}
	got := &store.DeploymentStage{}
	if err := json.Unmarshal(w.Body.Bytes(), got); err != nil {
		t.Fatal(err)
	}
	if got.WorkspaceID != ws.ID || got.WorkspaceName != ws.DisplayName {
		t.Fatalf("assign response = %+v", got)
	}

	// Being an Admin of the PIPELINE does not let you bind a workspace you
	// hold no role on — otherwise pipeline access would be a way to pull
	// someone else's workspace into a promotion path.
	foreign := &store.Workspace{DisplayName: "someone-elses"}
	if err := st.CreateWorkspace(foreign, store.Principal{ID: viewer.ID, Type: "User"}); err != nil {
		t.Fatal(err)
	}
	if w := do(a.assignStageWorkspace, admin, "POST", `{"workspaceId":"`+foreign.ID+`"}`, ids); w.Code != http.StatusForbidden {
		t.Errorf("assigning a foreign workspace = %d, want 403", w.Code)
	}

	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"no workspaceId":    {`{}`, http.StatusBadRequest},
		"malformed":         {`{`, http.StatusBadRequest},
		"unknown workspace": {`{"workspaceId":"nope"}`, http.StatusNotFound},
	} {
		if w := do(a.assignStageWorkspace, admin, "POST", tc.body, ids); w.Code != tc.want {
			t.Errorf("%s = %d, want %d", name, w.Code, tc.want)
		}
	}
}

// TestUnassignStageWorkspaceAPI: unassign clears the stage and is reachable
// only by a pipeline member.
func TestUnassignStageWorkspaceAPI(t *testing.T) {
	a, st := newAPI(t)
	pl, sts := seedPipeline(t, a, "")
	ws := seedWorkspace(t, st)
	ids := map[string]string{"pid": pl.ID, "sid": sts[0].ID}

	if w := do(a.assignStageWorkspace, admin, "POST", `{"workspaceId":"`+ws.ID+`"}`, ids); w.Code != http.StatusOK {
		t.Fatalf("assign = %d: %s", w.Code, w.Body)
	}
	w := do(a.unassignStageWorkspace, admin, "POST", "", ids)
	if w.Code != http.StatusOK {
		t.Fatalf("unassign = %d: %s", w.Code, w.Body)
	}
	got := &store.DeploymentStage{}
	_ = json.Unmarshal(w.Body.Bytes(), got)
	if got.WorkspaceID != "" {
		t.Fatalf("unassign response still assigned: %+v", got)
	}
	if w := do(a.unassignStageWorkspace, nobody, "POST", "", ids); w.Code != http.StatusNotFound {
		t.Errorf("non-member unassign = %d, want 404", w.Code)
	}
}

// TestAssignPairsThroughTheAPI: assigning both stages over the handlers
// establishes the pair, and the items then show through stage items.
func TestAssignPairsThroughTheAPI(t *testing.T) {
	a, st := newAPI(t)
	pl, sts := seedPipeline(t, a, "")

	mk := func(name string) *store.Workspace {
		ws := &store.Workspace{DisplayName: name}
		if err := st.CreateWorkspace(ws, store.Principal{ID: admin.ID, Type: admin.Type}); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateItem(&store.Item{
			WorkspaceID: ws.ID, DisplayName: "orders", Type: "Notebook"}, nil); err != nil {
			t.Fatal(err)
		}
		return ws
	}
	dev, tst := mk("dev-ws"), mk("test-ws")

	for i, ws := range []*store.Workspace{dev, tst} {
		ids := map[string]string{"pid": pl.ID, "sid": sts[i].ID}
		if w := do(a.assignStageWorkspace, admin, "POST", `{"workspaceId":"`+ws.ID+`"}`, ids); w.Code != http.StatusOK {
			t.Fatalf("assign stage %d = %d: %s", i, w.Code, w.Body)
		}
	}
	prs, err := st.ListItemPairs(pl.ID, sts[0].ID, sts[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 1 {
		t.Fatalf("pairs after assigning both stages = %+v", prs)
	}
	items := page[store.Item](t, do(a.listDeploymentStageItems, admin, "GET", "",
		map[string]string{"pid": pl.ID, "sid": sts[0].ID}).Body.Bytes())
	if len(items) != 1 || items[0].DisplayName != "orders" {
		t.Fatalf("stage items = %+v", items)
	}
}

// deployReady builds a pipeline with dev/test assigned and one item in dev.
func deployReady(t *testing.T, a *API, st *store.Store) (*store.DeploymentPipeline, []*store.DeploymentStage, *store.Workspace, *store.Workspace) {
	t.Helper()
	pl, sts := seedPipeline(t, a, "")
	mk := func(name string, items ...string) *store.Workspace {
		ws := &store.Workspace{DisplayName: name}
		if err := st.CreateWorkspace(ws, store.Principal{ID: admin.ID, Type: admin.Type}); err != nil {
			t.Fatal(err)
		}
		for _, n := range items {
			if err := st.CreateItem(&store.Item{
				WorkspaceID: ws.ID, DisplayName: n, Type: "Notebook"}, nil); err != nil {
				t.Fatal(err)
			}
		}
		return ws
	}
	dev, tst := mk("dev-ws", "orders"), mk("test-ws")
	for i, ws := range []*store.Workspace{dev, tst} {
		ids := map[string]string{"pid": pl.ID, "sid": sts[i].ID}
		if w := do(a.assignStageWorkspace, admin, "POST", `{"workspaceId":"`+ws.ID+`"}`, ids); w.Code != http.StatusOK {
			t.Fatalf("assign %d = %d: %s", i, w.Code, w.Body)
		}
	}
	return pl, sts, dev, tst
}

// TestDeployStageContentReturnsLRO: deploy answers with the 202 envelope the
// documented scripts poll — x-ms-operation-id, Location and Retry-After.
func TestDeployStageContentReturnsLRO(t *testing.T) {
	a, st := newAPI(t)
	pl, sts, _, tst := deployReady(t, a, st)

	body := `{"sourceStageId":"` + sts[0].ID + `","targetStageId":"` + sts[1].ID + `","note":"promote"}`
	w := do(a.deployStageContent, admin, "POST", body, map[string]string{"pid": pl.ID})
	if w.Code != http.StatusAccepted {
		t.Fatalf("deploy = %d: %s", w.Code, w.Body)
	}
	opID := w.Header().Get("x-ms-operation-id")
	if opID == "" || w.Header().Get("Location") == "" || w.Header().Get("Retry-After") == "" {
		t.Fatalf("202 envelope incomplete: %v", w.Header())
	}
	// The item really landed.
	items, err := st.ListItems(tst.ID, "")
	if err != nil || len(items) != 1 || items[0].DisplayName != "orders" {
		t.Fatalf("target items = %+v, %v", items, err)
	}
	// …and the detail is retrievable under the SAME id the LRO advertised.
	dep, err := st.GetDeploymentOperationByID(opID)
	if err != nil {
		t.Fatalf("deployment detail not stored under the operation id: %v", err)
	}
	if dep.Note != "promote" || len(dep.Items) != 1 || dep.Items[0].Outcome != store.DeployCreated {
		t.Fatalf("deployment detail = %+v", dep)
	}
}

func TestDeployStageContentBadRequests(t *testing.T) {
	a, st := newAPI(t)
	pl, sts, _, _ := deployReady(t, a, st)
	ids := map[string]string{"pid": pl.ID}
	ok := `"sourceStageId":"` + sts[0].ID + `","targetStageId":"` + sts[1].ID + `"`

	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"malformed":      {`{`, http.StatusBadRequest},
		"no stages":      {`{}`, http.StatusBadRequest},
		"no target":      {`{"sourceStageId":"` + sts[0].ID + `"}`, http.StatusBadRequest},
		"item unnamed":   {`{` + ok + `,"items":[{"itemType":"Notebook"}]}`, http.StatusBadRequest},
		"non-adjacent":   {`{"sourceStageId":"` + sts[0].ID + `","targetStageId":"` + sts[2].ID + `"}`, http.StatusBadRequest},
		"unknown stage":  {`{"sourceStageId":"nope","targetStageId":"` + sts[1].ID + `"}`, http.StatusNotFound},
		"unassigned tgt": {`{"sourceStageId":"` + sts[1].ID + `","targetStageId":"` + sts[2].ID + `"}`, http.StatusBadRequest},
	} {
		if w := do(a.deployStageContent, admin, "POST", tc.body, ids); w.Code != tc.want {
			t.Errorf("%s = %d, want %d (%s)", name, w.Code, tc.want, w.Body)
		}
	}
	if w := do(a.deployStageContent, nobody, "POST", `{`+ok+`}`, ids); w.Code != http.StatusNotFound {
		t.Errorf("non-member deploy = %d, want 404", w.Code)
	}
}

// TestDeployUnpairedCollisionIs409: the emulator refuses rather than
// duplicating or renaming (docs/23 Q1).
func TestDeployUnpairedCollisionIs409(t *testing.T) {
	a, st := newAPI(t)
	pl, sts, _, tst := deployReady(t, a, st)
	// Added AFTER assignment, so it is not paired.
	if err := st.CreateItem(&store.Item{
		WorkspaceID: tst.ID, DisplayName: "orders", Type: "Notebook"}, nil); err != nil {
		t.Fatal(err)
	}
	body := `{"sourceStageId":"` + sts[0].ID + `","targetStageId":"` + sts[1].ID + `"}`
	w := do(a.deployStageContent, admin, "POST", body, map[string]string{"pid": pl.ID})
	if w.Code != http.StatusConflict {
		t.Fatalf("collision = %d, want 409: %s", w.Code, w.Body)
	}
}

// TestDeploymentOperationsAPI: recorded deployments are listable and
// gettable, and gated by pipeline membership.
func TestDeploymentOperationsAPI(t *testing.T) {
	a, st := newAPI(t)
	pl, sts, _, _ := deployReady(t, a, st)
	ids := map[string]string{"pid": pl.ID}
	body := `{"sourceStageId":"` + sts[0].ID + `","targetStageId":"` + sts[1].ID + `"}`
	w := do(a.deployStageContent, admin, "POST", body, ids)
	if w.Code != http.StatusAccepted {
		t.Fatalf("deploy = %d", w.Code)
	}
	opID := w.Header().Get("x-ms-operation-id")

	ops := page[store.DeploymentOperation](t, do(a.listDeploymentOperations, admin, "GET", "", ids).Body.Bytes())
	if len(ops) != 1 || ops[0].ID != opID {
		t.Fatalf("list operations = %+v", ops)
	}
	withOp := map[string]string{"pid": pl.ID, "oid": opID}
	got := do(a.getDeploymentOperation, admin, "GET", "", withOp)
	if got.Code != http.StatusOK {
		t.Fatalf("get operation = %d: %s", got.Code, got.Body)
	}
	one := &store.DeploymentOperation{}
	_ = json.Unmarshal(got.Body.Bytes(), one)
	if one.ID != opID || len(one.Items) != 1 {
		t.Fatalf("get operation body = %+v", one)
	}
	if w := do(a.getDeploymentOperation, admin, "GET", "",
		map[string]string{"pid": pl.ID, "oid": "nope"}); w.Code != http.StatusNotFound {
		t.Errorf("unknown operation = %d, want 404", w.Code)
	}
	for name, h := range map[string]handler{
		"list": a.listDeploymentOperations, "get": a.getDeploymentOperation,
	} {
		if w := do(h, nobody, "GET", "", withOp); w.Code != http.StatusNotFound {
			t.Errorf("non-member %s = %d, want 404", name, w.Code)
		}
	}
}

// TestDeploymentPipelineRoleAssignments: grants make a pipeline visible to
// another principal, and revocation takes it away again.
func TestDeploymentPipelineRoleAssignments(t *testing.T) {
	a, _ := newAPI(t)
	pl, _ := seedPipeline(t, a, "")
	ids := map[string]string{"pid": pl.ID}

	// Before the grant, viewer sees nothing.
	if got := page[store.DeploymentPipeline](t,
		do(a.listDeploymentPipelines, viewer, "GET", "", nil).Body.Bytes()); len(got) != 0 {
		t.Fatalf("viewer sees %+v before any grant", got)
	}

	body := `{"principal":{"id":"` + viewer.ID + `","type":"User"},"role":"Admin"}`
	w := do(a.addDeploymentPipelineRole, admin, "POST", body, ids)
	if w.Code != http.StatusCreated {
		t.Fatalf("grant = %d: %s", w.Code, w.Body)
	}
	if got := page[store.DeploymentPipeline](t,
		do(a.listDeploymentPipelines, viewer, "GET", "", nil).Body.Bytes()); len(got) != 1 {
		t.Fatalf("viewer still cannot see the pipeline after a grant: %+v", got)
	}

	ras := page[store.DeploymentPipelineRoleAssignment](t,
		do(a.listDeploymentPipelineRoles, admin, "GET", "", ids).Body.Bytes())
	if len(ras) != 2 {
		t.Fatalf("role assignments = %+v", ras)
	}

	revoke := map[string]string{"pid": pl.ID, "prid": viewer.ID}
	if w := do(a.deleteDeploymentPipelineRole, admin, "DELETE", "", revoke); w.Code != http.StatusOK {
		t.Fatalf("revoke = %d: %s", w.Code, w.Body)
	}
	if got := page[store.DeploymentPipeline](t,
		do(a.listDeploymentPipelines, viewer, "GET", "", nil).Body.Bytes()); len(got) != 0 {
		t.Fatalf("viewer still sees the pipeline after revocation: %+v", got)
	}
	if w := do(a.deleteDeploymentPipelineRole, admin, "DELETE", "", revoke); w.Code != http.StatusNotFound {
		t.Errorf("revoking twice = %d, want 404", w.Code)
	}
}

// TestDeploymentPipelineRoleOnlyAdmin: Admin is the only role a deployment
// pipeline defines — unlike workspaces. An omitted role defaults to it; any
// other value is rejected rather than stored as something meaningless.
func TestDeploymentPipelineRoleOnlyAdmin(t *testing.T) {
	a, st := newAPI(t)
	pl, _ := seedPipeline(t, a, "")
	ids := map[string]string{"pid": pl.ID}

	w := do(a.addDeploymentPipelineRole, admin, "POST", `{"principal":{"id":"someone"}}`, ids)
	if w.Code != http.StatusCreated {
		t.Fatalf("default role = %d: %s", w.Code, w.Body)
	}
	role, err := st.DeploymentPipelineRole(pl.ID, "someone")
	if err != nil || role != store.RoleAdmin {
		t.Fatalf("defaulted role = %q, %v", role, err)
	}
	// Type defaults to User rather than being stored empty.
	ras, _ := st.ListDeploymentPipelineRoles(pl.ID)
	for _, ra := range ras {
		if ra.Principal.ID == "someone" && ra.Principal.Type != "User" {
			t.Errorf("principal type = %q, want User", ra.Principal.Type)
		}
	}

	for name, body := range map[string]string{
		"workspace role": `{"principal":{"id":"x"},"role":"Contributor"}`,
		"nonsense role":  `{"principal":{"id":"x"},"role":"Wizard"}`,
		"no principal":   `{"role":"Admin"}`,
		"malformed":      `{`,
	} {
		if w := do(a.addDeploymentPipelineRole, admin, "POST", body, ids); w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, w.Code)
		}
	}
}

// TestDeploymentPipelineRoleMutationsNeedAdmin is the escalation guard:
// holding a role on the pipeline lets you READ it, but only an Admin may
// change who can reach it. Without this, any member could revoke the owner.
func TestDeploymentPipelineRoleMutationsNeedAdmin(t *testing.T) {
	a, st := newAPI(t)
	pl, _ := seedPipeline(t, a, "")
	ids := map[string]string{"pid": pl.ID}

	// Grant viewer a NON-Admin role directly in the store — the API refuses
	// to mint one, but the store must still be gated correctly if it exists.
	if err := st.AddDeploymentPipelineRole(pl.ID,
		store.Principal{ID: viewer.ID, Type: "User"}, store.RoleViewer); err != nil {
		t.Fatal(err)
	}
	// They can read…
	if w := do(a.getDeploymentPipeline, viewer, "GET", "", ids); w.Code != http.StatusOK {
		t.Fatalf("member read = %d, want 200", w.Code)
	}
	if w := do(a.listDeploymentPipelineRoles, viewer, "GET", "", ids); w.Code != http.StatusOK {
		t.Fatalf("member role list = %d, want 200", w.Code)
	}
	// …but not change access.
	grant := `{"principal":{"id":"mallory"},"role":"Admin"}`
	if w := do(a.addDeploymentPipelineRole, viewer, "POST", grant, ids); w.Code != http.StatusForbidden {
		t.Errorf("non-admin grant = %d, want 403", w.Code)
	}
	revoke := map[string]string{"pid": pl.ID, "prid": admin.ID}
	if w := do(a.deleteDeploymentPipelineRole, viewer, "DELETE", "", revoke); w.Code != http.StatusForbidden {
		t.Errorf("non-admin revoke of the owner = %d, want 403", w.Code)
	}
	if role, err := st.DeploymentPipelineRole(pl.ID, admin.ID); err != nil || role != store.RoleAdmin {
		t.Fatalf("owner lost their role: %q, %v", role, err)
	}
	// A complete non-member gets 404 on all three.
	for name, h := range map[string]handler{
		"list": a.listDeploymentPipelineRoles,
		"add":  a.addDeploymentPipelineRole,
	} {
		if w := do(h, nobody, "POST", grant, ids); w.Code != http.StatusNotFound {
			t.Errorf("non-member %s = %d, want 404", name, w.Code)
		}
	}
	if w := do(a.deleteDeploymentPipelineRole, nobody, "DELETE", "", revoke); w.Code != http.StatusNotFound {
		t.Errorf("non-member revoke = %d, want 404", w.Code)
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
	_ = st.Close()
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

// TestUpdateDeploymentDescriptionIsWritable completes the PATCH semantics the
// test above only half covers: it proves an ABSENT description is preserved,
// which passes just as well if the field is ignored entirely. Neither the
// pipeline's nor the stage's description-writing branch was executed by
// anything.
//
// Clearing to "" is asserted too, and is the case a nil-vs-empty mix-up breaks:
// the field is a *string precisely so that "absent" and "set me to empty" stay
// distinguishable, and a handler that tested the value rather than the pointer
// would silently refuse to clear.
func TestUpdateDeploymentDescriptionIsWritable(t *testing.T) {
	a, _ := newAPI(t)
	pl, sts := seedPipeline(t, a, `{"displayName":"release","description":"before"}`)

	pids := map[string]string{"pid": pl.ID}
	w := do(a.updateDeploymentPipeline, admin, "PATCH", `{"description":"after"}`, pids)
	if w.Code != http.StatusOK {
		t.Fatalf("pipeline patch = %d: %s", w.Code, w.Body)
	}
	got := &store.DeploymentPipeline{}
	if err := json.Unmarshal(w.Body.Bytes(), got); err != nil {
		t.Fatal(err)
	}
	if got.Description != "after" || got.DisplayName != "release" {
		t.Fatalf("pipeline = %+v; want description updated and name untouched", got)
	}
	// Explicitly clearing must work, not be mistaken for "absent".
	w = do(a.updateDeploymentPipeline, admin, "PATCH", `{"description":""}`, pids)
	got = &store.DeploymentPipeline{}
	if err := json.Unmarshal(w.Body.Bytes(), got); err != nil {
		t.Fatal(err)
	}
	if got.Description != "" {
		t.Fatalf("description = %q; an explicit empty string must clear it", got.Description)
	}

	sids := map[string]string{"pid": pl.ID, "sid": sts[0].ID}
	w = do(a.updateDeploymentStage, admin, "PATCH", `{"description":"stage note"}`, sids)
	if w.Code != http.StatusOK {
		t.Fatalf("stage patch = %d: %s", w.Code, w.Body)
	}
	stage := &store.DeploymentStage{}
	if err := json.Unmarshal(w.Body.Bytes(), stage); err != nil {
		t.Fatal(err)
	}
	if stage.Description != "stage note" {
		t.Fatalf("stage = %+v; want the description written", stage)
	}
}
