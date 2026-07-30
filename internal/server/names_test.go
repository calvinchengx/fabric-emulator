package server_test

// Display-name uniqueness over HTTP: real Fabric rejects duplicates, and
// every name-addressed contract here (OneLake paths, git logical ids, the
// FABRIC_TARGET toggle, catalog ingest) depends on that holding.

import (
	"net/http"
	"testing"
)

func TestWorkspaceNameConflict(t *testing.T) {
	f := newFixture(t)

	var ws struct {
		ID string `json:"id"`
	}
	resp := f.call("POST", "/v1/workspaces", f.token, map[string]any{"displayName": "dupes"}, &ws)
	f.mustStatus(resp, 201, "first create")

	// Same name -> 409, with the error code in the body AND the header the
	// documented Fabric clients branch on.
	var body struct {
		ErrorCode string `json:"errorCode"`
	}
	resp = f.call("POST", "/v1/workspaces", f.token, map[string]any{"displayName": "dupes"}, &body)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate workspace: want 409, got %d", resp.StatusCode)
	}
	if body.ErrorCode != "WorkspaceNameAlreadyExists" {
		t.Fatalf("errorCode = %q", body.ErrorCode)
	}
	if h := resp.Header.Get("x-ms-public-api-error-code"); h != "WorkspaceNameAlreadyExists" {
		t.Fatalf("x-ms-public-api-error-code = %q", h)
	}

	// Case-insensitive.
	resp = f.call("POST", "/v1/workspaces", f.token, map[string]any{"displayName": "DUPES"}, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("case-variant workspace: want 409, got %d", resp.StatusCode)
	}

	// A distinct name still works.
	resp = f.call("POST", "/v1/workspaces", f.token, map[string]any{"displayName": "dupes-2"}, nil)
	f.mustStatus(resp, 201, "distinct name")
}

func TestWorkspaceRenameConflict(t *testing.T) {
	f := newFixture(t)

	var a, b struct {
		ID string `json:"id"`
	}
	f.mustStatus(f.call("POST", "/v1/workspaces", f.token, map[string]any{"displayName": "alpha"}, &a), 201, "create alpha")
	f.mustStatus(f.call("POST", "/v1/workspaces", f.token, map[string]any{"displayName": "beta"}, &b), 201, "create beta")

	// beta -> alpha is a conflict.
	resp := f.call("PATCH", "/v1/workspaces/"+b.ID, f.token, map[string]any{"displayName": "alpha"}, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("rename into taken name: want 409, got %d", resp.StatusCode)
	}
	// Renaming to its own name is a no-op, not a conflict.
	f.mustStatus(f.call("PATCH", "/v1/workspaces/"+b.ID, f.token,
		map[string]any{"displayName": "beta"}, nil), 200, "self rename")
	// A free name is fine.
	f.mustStatus(f.call("PATCH", "/v1/workspaces/"+b.ID, f.token,
		map[string]any{"displayName": "gamma"}, nil), 200, "rename to free name")
	// Description-only patches must not trip the check.
	f.mustStatus(f.call("PATCH", "/v1/workspaces/"+b.ID, f.token,
		map[string]any{"description": "desc"}, nil), 200, "description only")
}

func TestItemNameConflict(t *testing.T) {
	f := newFixture(t)

	var ws struct {
		ID string `json:"id"`
	}
	f.mustStatus(f.call("POST", "/v1/workspaces", f.token,
		map[string]any{"displayName": "itemdupes"}, &ws), 201, "create workspace")

	mk := func(name, typ string, out any) int {
		return f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token,
			map[string]any{"displayName": name, "type": typ}, out).StatusCode
	}

	var first struct {
		ID string `json:"id"`
	}
	if code := mk("report", "Notebook", &first); code != 201 && code != 202 {
		t.Fatalf("first item: %d", code)
	}

	// Same name + same type -> 409 ItemDisplayNameAlreadyInUse.
	var body struct {
		ErrorCode string `json:"errorCode"`
	}
	if code := mk("report", "Notebook", &body); code != http.StatusConflict {
		t.Fatalf("duplicate item: want 409, got %d", code)
	}
	if body.ErrorCode != "ItemDisplayNameAlreadyInUse" {
		t.Fatalf("errorCode = %q", body.ErrorCode)
	}
	// Case-insensitive on both name and type.
	if code := mk("REPORT", "notebook", nil); code != http.StatusConflict {
		t.Fatalf("case-variant item: want 409, got %d", code)
	}

	// Names ARE reusable across item types — that is why OneLake addresses
	// items as name.Type (onelake-access-api.md).
	if code := mk("report", "Lakehouse", nil); code != 201 && code != 202 {
		t.Fatalf("same name, different type should be allowed, got %d", code)
	}

	// Scoped per workspace: the same name is free in another workspace.
	var ws2 struct {
		ID string `json:"id"`
	}
	f.mustStatus(f.call("POST", "/v1/workspaces", f.token,
		map[string]any{"displayName": "itemdupes-2"}, &ws2), 201, "second workspace")
	code := f.call("POST", "/v1/workspaces/"+ws2.ID+"/items", f.token,
		map[string]any{"displayName": "report", "type": "Notebook"}, nil).StatusCode
	if code != 201 && code != 202 {
		t.Fatalf("same name in another workspace: %d", code)
	}
}

func TestItemRenameConflict(t *testing.T) {
	f := newFixture(t)

	var ws struct {
		ID string `json:"id"`
	}
	f.mustStatus(f.call("POST", "/v1/workspaces", f.token,
		map[string]any{"displayName": "renames"}, &ws), 201, "create workspace")

	var a, b struct {
		ID string `json:"id"`
	}
	f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token,
		map[string]any{"displayName": "one", "type": "Notebook"}, &a)
	f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token,
		map[string]any{"displayName": "two", "type": "Notebook"}, &b)

	resp := f.call("PATCH", "/v1/workspaces/"+ws.ID+"/items/"+b.ID, f.token,
		map[string]any{"displayName": "one"}, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("rename into taken item name: want 409, got %d", resp.StatusCode)
	}
	// Self-rename and description-only patches stay 200.
	f.mustStatus(f.call("PATCH", "/v1/workspaces/"+ws.ID+"/items/"+b.ID, f.token,
		map[string]any{"displayName": "two"}, nil), 200, "self rename")
	f.mustStatus(f.call("PATCH", "/v1/workspaces/"+ws.ID+"/items/"+b.ID, f.token,
		map[string]any{"description": "d"}, nil), 200, "description only")
	f.mustStatus(f.call("PATCH", "/v1/workspaces/"+ws.ID+"/items/"+b.ID, f.token,
		map[string]any{"displayName": "three"}, nil), 200, "rename to free name")
}
