package api

// P3 coverage tests: the pipeline executor's remaining leaf/reference edges
// (pass-through leaves, invoke reference and waitOnCompletion resolution,
// copy location errors), activity-run query auth, and the identity/shortcut
// handler branches that need no live entra.

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// TestPipelineLeafPassthrough: an inexecutable leaf (an external connector)
// records that orchestration reached it without running any effect.
func TestPipelineLeafPassthrough(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	pl := createPipeline(t, st, ws.ID, `{"properties":{"activities":[
        {"name":"Hit","type":"WebActivity","typeProperties":{"url":"https://example.invalid"}}
      ]}}`)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("pass-through leaf = %s, want Completed", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if out := outputOf(runs, "Hit"); out["activityType"] != "WebActivity" {
		t.Fatalf("pass-through output = %+v", out)
	}
}

// TestPipelineNotebookMissingID: a notebook activity without a notebookId
// fails loudly.
func TestPipelineNotebookMissingID(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	pl := createPipeline(t, st, ws.ID, `{"properties":{"activities":[
        {"name":"RunNb","type":"TridentNotebook","typeProperties":{}}
      ]}}`)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("missing notebookId = %s, want Failed", s)
	}
}

// TestPipelineInvokeRefErrors: an Invoke with no reference at all, a
// pipelineId that fails to resolve, and a parameter expression that fails all
// fail the activity.
func TestPipelineInvokeRefErrors(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	child := createNamedPipeline(t, st, ws.ID, "child", `{"properties":{"activities":[]}}`)

	for name, content := range map[string]string{
		"no reference": `{"properties":{"activities":[
            {"name":"Call","type":"ExecutePipeline","typeProperties":{}}
          ]}}`,
		"bad pipelineId expression": `{"properties":{"activities":[
            {"name":"Call","type":"ExecutePipeline","typeProperties":{"pipelineId":"@nosuchfunc()"}}
          ]}}`,
		"bad parameter expression": `{"properties":{"activities":[
            {"name":"Call","type":"ExecutePipeline","typeProperties":{
              "pipeline":{"referenceName":"` + child.ID + `"},"parameters":{"p":"@nosuchfunc()"}}}
          ]}}`,
	} {
		pl := createPipeline(t, st, ws.ID, content)
		_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
		if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Failed" {
			t.Errorf("%s = %s, want Failed", name, s)
		}
	}
}

// TestPipelineInvokeChildDefinitionErrors: a child whose stored definition is
// not valid pipeline JSON fails the invoke; a child whose sole definition part
// has a non-standard path still loads (the sole-part fallback).
func TestPipelineInvokeChildDefinitionErrors(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)

	bad := createNamedPipeline(t, st, ws.ID, "bad-child", `not json at all`)
	pl := createNamedPipeline(t, st, ws.ID, "parent-bad", `{"properties":{"activities":[
        {"name":"Call","type":"ExecutePipeline","typeProperties":{"pipeline":{"referenceName":"`+bad.ID+`"}}}
      ]}}`)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("unparsable child = %s, want Failed", s)
	}

	// A child stored under a different part path resolves via the sole-part
	// fallback.
	odd := createNamedPipeline(t, st, ws.ID, "odd-child", `{"properties":{"activities":[]}}`)
	if err := st.SetDefinition(odd.ID, []store.DefinitionPart{{
		Path:        "content.json",
		Payload:     base64.StdEncoding.EncodeToString([]byte(`{"properties":{"activities":[]}}`)),
		PayloadType: "InlineBase64",
	}}); err != nil {
		t.Fatal(err)
	}
	pl2 := createNamedPipeline(t, st, ws.ID, "parent-odd", `{"properties":{"activities":[
        {"name":"Call","type":"ExecutePipeline","typeProperties":{"pipeline":{"referenceName":"`+odd.ID+`"}}}
      ]}}`)
	_, jid2 := runJob(t, a, ws.ID, pl2.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl2.ID, jid2); s != "Completed" {
		t.Fatalf("sole-part child = %s, want Completed", s)
	}
}

// TestPipelineInvokeWorkspaceByIDAndName: the invoke's workspace reference
// resolves both as a GUID and as a display name.
func TestPipelineInvokeWorkspaceByIDAndName(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	ws2 := &store.Workspace{DisplayName: "other-ws"}
	if err := st.CreateWorkspace(ws2, store.Principal{ID: admin.ID, Type: admin.Type}); err != nil {
		t.Fatal(err)
	}
	createNamedPipeline(t, st, ws2.ID, "worker", `{"properties":{"activities":[]}}`)

	pl := createNamedPipeline(t, st, ws.ID, "parent", `{"properties":{"activities":[
        {"name":"ByID","type":"ExecutePipeline","typeProperties":{
          "pipeline":{"referenceName":"worker","workspaceId":"`+ws2.ID+`"}}},
        {"name":"ByName","type":"ExecutePipeline","typeProperties":{
          "pipeline":{"referenceName":"worker","workspaceId":"other-ws"}}}
      ]}}`)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("cross-workspace invoke = %s, want Completed", s)
	}
}

// TestPipelineInvokeWaitOnCompletionExpressions: waitOnCompletion accepts an
// expression — one resolving false makes a failing child fire-and-forget; one
// that fails to resolve falls back to the blocking default.
func TestPipelineInvokeWaitOnCompletionExpressions(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	failing := createNamedPipeline(t, st, ws.ID, "failing-child", `{"properties":{"activities":[
        {"name":"Boom","type":"Fail","typeProperties":{"message":"nope"}}
      ]}}`)

	fire := createNamedPipeline(t, st, ws.ID, "fire", `{"properties":{"activities":[
        {"name":"Call","type":"ExecutePipeline","typeProperties":{
          "pipeline":{"referenceName":"`+failing.ID+`"},"waitOnCompletion":"@equals(1,2)"}}
      ]}}`)
	_, jid := runJob(t, a, ws.ID, fire.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, fire.ID, jid); s != "Completed" {
		t.Fatalf("waitOnCompletion=false expression = %s, want Completed", s)
	}

	block := createNamedPipeline(t, st, ws.ID, "block", `{"properties":{"activities":[
        {"name":"Call","type":"ExecutePipeline","typeProperties":{
          "pipeline":{"referenceName":"`+failing.ID+`"},"waitOnCompletion":"@nosuchfunc()"}}
      ]}}`)
	_, jid2 := runJob(t, a, ws.ID, block.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, block.ID, jid2); s != "Failed" {
		t.Fatalf("unresolvable waitOnCompletion = %s, want Failed (blocking default)", s)
	}
}

// TestPipelineCopyLocationEdges: sink/field resolution errors, an unknown item
// reference, a workspace referenced by GUID, a null-resolving field, and a
// source subtree containing directory rows.
func TestPipelineCopyLocationEdges(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := seedLakehouse(t, st, ws.ID, "src")
	dst := seedLakehouse(t, st, ws.ID, "dst")
	seedFile(t, st, ws.ID, src.ID, "Files/a", []byte("hi"))

	srcLoc := `"source":{"location":{"itemId":"` + src.ID + `","path":"Files/a"}}`
	for name, content := range map[string]string{
		"malformed sink": `{"properties":{"activities":[
            {"name":"Move","type":"Copy","typeProperties":{` + srcLoc + `,"sink":"not-an-object"}}]}}`,
		"bad workspaceId expression": `{"properties":{"activities":[
            {"name":"Move","type":"Copy","typeProperties":{
              "source":{"location":{"workspaceId":"@nosuchfunc()","itemId":"` + src.ID + `","path":"Files/a"}},
              "sink":{"location":{"itemId":"` + dst.ID + `","path":"Files/x"}}}}]}}`,
		"bad itemId expression": `{"properties":{"activities":[
            {"name":"Move","type":"Copy","typeProperties":{
              "source":{"location":{"itemId":"@nosuchfunc()","path":"Files/a"}},
              "sink":{"location":{"itemId":"` + dst.ID + `","path":"Files/x"}}}}]}}`,
		"bad path expression": `{"properties":{"activities":[
            {"name":"Move","type":"Copy","typeProperties":{
              "source":{"location":{"itemId":"` + src.ID + `","path":"@nosuchfunc()"}},
              "sink":{"location":{"itemId":"` + dst.ID + `","path":"Files/x"}}}}]}}`,
		"unknown item": `{"properties":{"activities":[
            {"name":"Move","type":"Copy","typeProperties":{
              "source":{"location":{"itemId":"totally-unknown","path":"Files/a"}},
              "sink":{"location":{"itemId":"` + dst.ID + `","path":"Files/x"}}}}]}}`,
	} {
		pl := createPipeline(t, st, ws.ID, content)
		_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
		if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Failed" {
			t.Errorf("%s = %s, want Failed", name, s)
		}
	}

	// Workspace referenced by GUID, and a workspaceId expression resolving to
	// null (treated as absent → the pipeline's own workspace).
	ok := `{"properties":{"activities":[
        {"name":"Move","type":"Copy","typeProperties":{
          "source":{"location":{"workspaceId":"` + ws.ID + `","itemId":"` + src.ID + `","path":"Files/a"}},
          "sink":{"location":{"workspaceId":"@null","itemId":"` + dst.ID + `","path":"Files/ok"}}}}]}}`
	pl := createPipeline(t, st, ws.ID, ok)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("GUID workspace copy = %s, want Completed", s)
	}
	if got, err := st.GetOneLakePath(dst.ID, "Files/ok"); err != nil || string(got.Content) != "hi" {
		t.Fatalf("GUID workspace sink = %q (err %v)", got.Content, err)
	}

	// A directory copy skips directory rows in the subtree listing.
	seedFile(t, st, ws.ID, src.ID, "Files/dir/one.txt", []byte("1"))
	if err := st.CreateOneLakePath(&store.OneLakePath{WorkspaceID: ws.ID, ItemID: src.ID, RelPath: "Files/dir", IsDir: true, Content: []byte{}}, false); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateOneLakePath(&store.OneLakePath{WorkspaceID: ws.ID, ItemID: src.ID, RelPath: "Files/dir/sub", IsDir: true, Content: []byte{}}, false); err != nil {
		t.Fatal(err)
	}
	dircp := `{"properties":{"activities":[
        {"name":"Move","type":"Copy","typeProperties":{
          "source":{"location":{"itemId":"` + src.ID + `","path":"Files/dir"}},
          "sink":{"location":{"itemId":"` + dst.ID + `","path":"Files/dircp"}}}}]}}`
	pl2 := createPipeline(t, st, ws.ID, dircp)
	_, jid2 := runJob(t, a, ws.ID, pl2.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl2.ID, jid2); s != "Completed" {
		t.Fatalf("dir copy with dir rows = %s, want Completed", s)
	}
	if _, err := st.GetOneLakePath(dst.ID, "Files/dircp/one.txt"); err != nil {
		t.Fatalf("dir copy file missing: %v", err)
	}
}

// TestQueryActivityRunsAuthAndEdges: RBAC applies, an unknown job 404s, and a
// run whose stored detail is empty answers with an empty list.
func TestQueryActivityRunsAuthAndEdges(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	pl := createPipeline(t, st, ws.ID, `{"properties":{"activities":[]}}`)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")

	if w := do(a.queryActivityRuns, nobody, "POST", "",
		map[string]string{"wid": ws.ID, "iid": pl.ID, "jid": jid}); w.Code != http.StatusForbidden {
		t.Fatalf("ungranted queryActivityRuns = %d", w.Code)
	}
	if w := do(a.queryActivityRuns, admin, "POST", "",
		map[string]string{"wid": ws.ID, "iid": pl.ID, "jid": "nope"}); w.Code != http.StatusNotFound {
		t.Fatalf("unknown job = %d", w.Code)
	}

	// Empty stored run detail still answers a well-formed empty list.
	if err := st.SetPipelineRun(jid, "Succeeded", ""); err != nil {
		t.Fatal(err)
	}
	w := do(a.queryActivityRuns, admin, "POST", "",
		map[string]string{"wid": ws.ID, "iid": pl.ID, "jid": jid})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"value":[]`) {
		t.Fatalf("empty run detail = %d %s", w.Code, w.Body.Bytes())
	}
}

// TestIdentityLifecycleWithoutEntra: RBAC gates deprovision, and the
// name-follows/cascade hooks are no-ops (not failures) when no entra client is
// configured but an identity row exists.
func TestIdentityLifecycleWithoutEntra(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	wid := map[string]string{"wid": ws.ID}

	if w := do(a.deprovisionIdentity, viewer, "POST", "", wid); w.Code != http.StatusForbidden {
		t.Fatalf("viewer deprovision = %d", w.Code)
	}

	if err := st.SetWorkspaceIdentity(&store.WorkspaceIdentity{WorkspaceID: ws.ID, IdentityID: "sp-x", AppID: "app-x"}); err != nil {
		t.Fatal(err)
	}
	// Rename follows: with no entra client the rename is a local no-op.
	if w := do(a.updateWorkspace, admin, "PATCH", `{"displayName":"renamed"}`, wid); w.Code != http.StatusOK {
		t.Fatalf("rename with identity, nil entra = %d %s", w.Code, w.Body.Bytes())
	}
	// Cascade delete: likewise.
	if w := do(a.deleteWorkspace, admin, "DELETE", "", wid); w.Code != http.StatusOK {
		t.Fatalf("delete with identity, nil entra = %d %s", w.Code, w.Body.Bytes())
	}
}

// TestShortcutAuthAndMissingItem: each shortcut handler enforces RBAC and
// 404s on an unknown item before touching shortcut state.
func TestShortcutAuthAndMissingItem(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	missing := map[string]string{"wid": ws.ID, "iid": "nope", "path": "Files", "name": "sc"}

	if w := do(a.createShortcut, admin, "POST", `{"path":"Files","name":"sc"}`, missing); w.Code != http.StatusNotFound {
		t.Fatalf("createShortcut missing item = %d", w.Code)
	}
	if w := do(a.listShortcuts, nobody, "GET", "", missing); w.Code != http.StatusForbidden {
		t.Fatalf("ungranted listShortcuts = %d", w.Code)
	}
	if w := do(a.getShortcut, nobody, "GET", "", missing); w.Code != http.StatusForbidden {
		t.Fatalf("ungranted getShortcut = %d", w.Code)
	}
	if w := do(a.getShortcut, admin, "GET", "", missing); w.Code != http.StatusNotFound {
		t.Fatalf("getShortcut missing item = %d", w.Code)
	}
	if w := do(a.deleteShortcut, nobody, "DELETE", "", missing); w.Code != http.StatusForbidden {
		t.Fatalf("ungranted deleteShortcut = %d", w.Code)
	}
	if w := do(a.deleteShortcut, admin, "DELETE", "", missing); w.Code != http.StatusNotFound {
		t.Fatalf("deleteShortcut missing item = %d", w.Code)
	}
}
