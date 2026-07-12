package server_test

// The operator portal: unauthenticated read-only /_emulator/portal endpoints
// plus the embedded SPA served at "/". State is created through the real
// authenticated /v1 surface, then observed through the portal's eyes.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func (f *fixture) portalJSON(t *testing.T, path string, out any) int {
	t.Helper()
	resp, err := http.Get(f.fabric.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
	}
	return resp.StatusCode
}

func TestPortalEndpoints(t *testing.T) {
	f := newFixture(t)

	// Empty state first.
	var empty struct {
		Value []json.RawMessage `json:"value"`
	}
	if code := f.portalJSON(t, "/_emulator/portal/workspaces", &empty); code != 200 {
		t.Fatalf("portal workspaces: %d", code)
	}
	if len(empty.Value) != 0 {
		t.Fatalf("expected no workspaces, got %d", len(empty.Value))
	}

	// Create a workspace + item through the real API.
	var ws struct {
		ID string `json:"id"`
	}
	resp := f.call("POST", "/v1/workspaces", f.token, map[string]any{"displayName": "portal-ws"}, &ws)
	f.mustStatus(resp, 201, "create workspace")
	resp = f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token,
		map[string]any{"displayName": "nb", "type": "Notebook"}, nil)
	if resp.StatusCode != 201 && resp.StatusCode != 202 {
		t.Fatalf("create item: %d", resp.StatusCode)
	}

	// List view: enriched row.
	var list struct {
		Value []struct {
			ID                string          `json:"id"`
			DisplayName       string          `json:"displayName"`
			CapacityID        string          `json:"capacityId"`
			ItemCount         int             `json:"itemCount"`
			RoleCount         int             `json:"roleCount"`
			WorkspaceIdentity json.RawMessage `json:"workspaceIdentity"`
		} `json:"value"`
	}
	if code := f.portalJSON(t, "/_emulator/portal/workspaces", &list); code != 200 {
		t.Fatalf("portal workspaces: %d", code)
	}
	if len(list.Value) != 1 {
		t.Fatalf("want 1 workspace, got %d", len(list.Value))
	}
	row := list.Value[0]
	if row.DisplayName != "portal-ws" || row.ItemCount != 1 || row.RoleCount != 1 || row.CapacityID == "" {
		t.Fatalf("enriched row wrong: %+v", row)
	}
	if string(row.WorkspaceIdentity) != "null" {
		t.Fatalf("identity should be null, got %s", row.WorkspaceIdentity)
	}

	// Detail view.
	var detail struct {
		Workspace       struct{ ID string }     `json:"workspace"`
		Items           []struct{ Type string } `json:"items"`
		RoleAssignments []struct{ Role string } `json:"roleAssignments"`
		Git             json.RawMessage         `json:"git"`
	}
	if code := f.portalJSON(t, "/_emulator/portal/workspaces/"+ws.ID, &detail); code != 200 {
		t.Fatalf("portal detail: %d", code)
	}
	if detail.Workspace.ID != ws.ID || len(detail.Items) != 1 || detail.Items[0].Type != "Notebook" ||
		len(detail.RoleAssignments) != 1 || detail.RoleAssignments[0].Role != "Admin" {
		t.Fatalf("detail wrong: %+v", detail)
	}
	if code := f.portalJSON(t, "/_emulator/portal/workspaces/nope", nil); code != 404 {
		t.Fatalf("missing workspace: want 404, got %d", code)
	}

	// Operations view: the item create above enqueued an LRO (or not — the
	// workspace create is sync). Either way the endpoint must answer.
	var ops struct {
		Value []struct {
			Status string `json:"status"`
			Kind   string `json:"kind"`
		} `json:"value"`
	}
	if code := f.portalJSON(t, "/_emulator/portal/operations", &ops); code != 200 {
		t.Fatalf("portal operations: %d", code)
	}
	for _, op := range ops.Value {
		switch op.Status {
		case "NotStarted", "Running", "Succeeded", "Failed":
		default:
			t.Fatalf("bad derived status %q", op.Status)
		}
	}
}

func TestPortalSPAServing(t *testing.T) {
	f := newFixture(t)

	for _, path := range []string{"/", "/some/deep/link"} {
		resp, err := http.Get(f.fabric.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s: %d", path, resp.StatusCode)
		}
		if !strings.Contains(string(body), `<div id="app">`) {
			t.Fatalf("GET %s did not serve the SPA shell", path)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Fatalf("GET %s content-type %q", path, ct)
		}
	}

	// Real assets serve with their own type, not the SPA fallback.
	resp, err := http.Get(f.fabric.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	shell, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// Extract the fingerprinted JS asset path from the shell.
	i := strings.Index(string(shell), "assets/")
	j := strings.Index(string(shell[i:]), `"`)
	asset := "/" + string(shell[i:i+j])
	resp, err = http.Get(f.fabric.URL + asset)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "javascript") {
		t.Fatalf("asset %s: %d %s", asset, resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	// The API surfaces still win over the SPA fallback.
	resp, err = http.Get(f.fabric.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatal("/health should still serve JSON")
	}
}
