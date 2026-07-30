package server_test

// Deployment pipelines D0 over the real mux (docs/23): a real entra-minted
// bearer token through the actual routes, which the handler-level tests in
// internal/api bypass. Would have caught a route registered on the wrong
// method or path — the SPA fallback swallows unknown GETs, so a typo shows up
// as HTML, not a 404.

import (
	"net/http"
	"testing"

	entra "github.com/calvinchengx/entra-emulator/emulator"
)

type wirePipeline struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
}

type wireStage struct {
	ID            string `json:"id"`
	Order         int    `json:"order"`
	DisplayName   string `json:"displayName"`
	IsPublic      bool   `json:"isPublic"`
	WorkspaceID   string `json:"workspaceId"`
	WorkspaceName string `json:"workspaceName"`
}

func TestDeploymentPipelinesOverTheWire(t *testing.T) {
	f := newFixture(t)
	tok := f.forgeUserToken(entra.AliceOID)

	var pl wirePipeline
	resp := f.call("POST", "/v1/deploymentPipelines", tok,
		map[string]any{"displayName": "release", "description": "d"}, &pl)
	f.mustStatus(resp, http.StatusCreated, "create pipeline")
	if pl.ID == "" {
		t.Fatalf("no id: %+v", pl)
	}

	// The default three stages, in order, over the wire.
	var stages struct {
		Value []wireStage `json:"value"`
	}
	resp = f.call("GET", "/v1/deploymentPipelines/"+pl.ID+"/stages", tok, nil, &stages)
	f.mustStatus(resp, http.StatusOK, "list stages")
	if len(stages.Value) != 3 {
		t.Fatalf("stages = %+v", stages.Value)
	}
	for i, st := range stages.Value {
		if st.Order != i {
			t.Errorf("stage %d order = %d", i, st.Order)
		}
		if st.WorkspaceID != "" {
			t.Errorf("stage %d assigned at create: %+v", i, st)
		}
	}

	// Single stage, and PATCH on both levels.
	var one wireStage
	resp = f.call("GET", "/v1/deploymentPipelines/"+pl.ID+"/stages/"+stages.Value[0].ID, tok, nil, &one)
	f.mustStatus(resp, http.StatusOK, "get stage")
	if one.ID != stages.Value[0].ID {
		t.Fatalf("get stage = %+v", one)
	}

	resp = f.call("PATCH", "/v1/deploymentPipelines/"+pl.ID+"/stages/"+one.ID, tok,
		map[string]any{"displayName": "Dev", "isPublic": true}, &one)
	f.mustStatus(resp, http.StatusOK, "patch stage")
	if one.DisplayName != "Dev" || !one.IsPublic {
		t.Fatalf("patched stage = %+v", one)
	}

	resp = f.call("PATCH", "/v1/deploymentPipelines/"+pl.ID, tok,
		map[string]any{"displayName": "renamed"}, &pl)
	f.mustStatus(resp, http.StatusOK, "patch pipeline")
	if pl.DisplayName != "renamed" || pl.Description != "d" {
		t.Fatalf("patched pipeline = %+v", pl)
	}

	// Unassigned stage items: an empty page, not an error.
	var items struct {
		Value []map[string]any `json:"value"`
	}
	resp = f.call("GET", "/v1/deploymentPipelines/"+pl.ID+"/stages/"+one.ID+"/items", tok, nil, &items)
	f.mustStatus(resp, http.StatusOK, "stage items")
	if len(items.Value) != 0 {
		t.Fatalf("unassigned stage items = %+v", items.Value)
	}

	// List shows it to its creator.
	var list struct {
		Value []wirePipeline `json:"value"`
	}
	resp = f.call("GET", "/v1/deploymentPipelines", tok, nil, &list)
	f.mustStatus(resp, http.StatusOK, "list pipelines")
	if len(list.Value) != 1 || list.Value[0].ID != pl.ID {
		t.Fatalf("list = %+v", list.Value)
	}

	// A different principal (Bob) can't see it at all.
	other := f.forgeUserToken(entra.BobOID)
	var otherList struct {
		Value []wirePipeline `json:"value"`
	}
	resp = f.call("GET", "/v1/deploymentPipelines", other, nil, &otherList)
	f.mustStatus(resp, http.StatusOK, "list for other")
	if len(otherList.Value) != 0 {
		t.Fatalf("Bob sees Alice's pipeline: %+v", otherList.Value)
	}
	resp = f.call("GET", "/v1/deploymentPipelines/"+pl.ID, other, nil, nil)
	f.mustStatus(resp, http.StatusNotFound, "get as non-member")

	// The /v1 contract stays bearer-only.
	resp = f.call("GET", "/v1/deploymentPipelines", "", nil, nil)
	f.mustStatus(resp, http.StatusUnauthorized, "unauthenticated")

	// D1: assign a workspace over the wire, then unassign.
	var ws struct {
		ID string `json:"id"`
	}
	resp = f.call("POST", "/v1/workspaces", tok, map[string]any{"displayName": "dp-dev"}, &ws)
	f.mustStatus(resp, http.StatusCreated, "create workspace")

	var assigned wireStage
	resp = f.call("POST", "/v1/deploymentPipelines/"+pl.ID+"/stages/"+one.ID+"/assignWorkspace",
		tok, map[string]any{"workspaceId": ws.ID}, &assigned)
	f.mustStatus(resp, http.StatusOK, "assign workspace")
	if assigned.WorkspaceID != ws.ID || assigned.WorkspaceName != "dp-dev" {
		t.Fatalf("assigned stage = %+v", assigned)
	}

	// Bob holds no role on the pipeline, so the route is invisible to him.
	resp = f.call("POST", "/v1/deploymentPipelines/"+pl.ID+"/stages/"+one.ID+"/unassignWorkspace",
		other, nil, nil)
	f.mustStatus(resp, http.StatusNotFound, "unassign as non-member")

	// Decode into a FRESH struct: workspaceId is omitempty, so unmarshalling
	// an unassigned stage over a populated value would leave the old id in
	// place and the assertion would pass for the wrong reason.
	var cleared wireStage
	resp = f.call("POST", "/v1/deploymentPipelines/"+pl.ID+"/stages/"+one.ID+"/unassignWorkspace",
		tok, nil, &cleared)
	f.mustStatus(resp, http.StatusOK, "unassign workspace")
	if cleared.WorkspaceID != "" || cleared.WorkspaceName != "" {
		t.Fatalf("unassigned stage = %+v", cleared)
	}
	// …and confirmed independently by a fresh read.
	var reread wireStage
	resp = f.call("GET", "/v1/deploymentPipelines/"+pl.ID+"/stages/"+one.ID, tok, nil, &reread)
	f.mustStatus(resp, http.StatusOK, "re-read stage")
	if reread.WorkspaceID != "" {
		t.Fatalf("stage still assigned on re-read: %+v", reread)
	}

	resp = f.call("DELETE", "/v1/deploymentPipelines/"+pl.ID, tok, nil, nil)
	f.mustStatus(resp, http.StatusOK, "delete")
	resp = f.call("GET", "/v1/deploymentPipelines/"+pl.ID, tok, nil, nil)
	f.mustStatus(resp, http.StatusNotFound, "get after delete")
}
