package server_test

// P2 e2e: the workspace-identity handshake. fabric-emulator provisions the
// identity by driving entra-emulator's admin API; entra mints an app-only
// token for it (no caller-held credential); that token calls back into
// fabric-emulator and passes RBAC as the workspace's own identity.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/akv"
)

type identityShape struct {
	ApplicationID      string `json:"applicationId"`
	ServicePrincipalID string `json:"servicePrincipalId"`
}

// entraGET fetches a JSON document from the in-process entra emulator.
func (f *fixture) entraJSON(t *testing.T, method, path string, out any) int {
	t.Helper()
	req, err := http.NewRequest(method, f.emu.Origin+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.emu.HTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && len(raw) > 0 && resp.StatusCode < 300 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s: bad JSON %q: %v", method, path, raw, err)
		}
	}
	return resp.StatusCode
}

func TestWorkspaceIdentityHandshake(t *testing.T) {
	f := newFixture(t)

	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "identity-ws"}, &ws)

	// Provision: 202 LRO; the identity appears on the workspace shape.
	resp := f.call("POST", "/v1/workspaces/"+ws.ID+"/provisionIdentity", f.token, nil, nil)
	f.mustStatus(resp, http.StatusAccepted, "provision")
	var op struct{ Status string }
	f.call("GET", "/v1/operations/"+resp.Header.Get("x-ms-operation-id"), f.token, nil, &op)
	if op.Status != "Succeeded" {
		t.Fatalf("provision op = %q", op.Status)
	}
	var got struct {
		WorkspaceIdentity *identityShape `json:"workspaceIdentity"`
	}
	f.mustStatus(f.call("GET", "/v1/workspaces/"+ws.ID, f.token, nil, &got), http.StatusOK, "get ws")
	if got.WorkspaceIdentity == nil || got.WorkspaceIdentity.ApplicationID == "" || got.WorkspaceIdentity.ServicePrincipalID == "" {
		t.Fatalf("workspaceIdentity = %+v", got.WorkspaceIdentity)
	}
	spID := got.WorkspaceIdentity.ServicePrincipalID

	// Double provision → 409.
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/provisionIdentity", f.token, nil, nil),
		http.StatusConflict, "double provision")

	// entra mints an app-only token for the identity (no secret involved)…
	var mint struct {
		AccessToken string `json:"access_token"`
		WorkspaceID string `json:"workspace_id"`
	}
	if code := f.entraJSON(t, "GET", "/fabric/workspaceidentities/"+spID+"/token", &mint); code != http.StatusOK {
		t.Fatalf("mint = %d", code)
	}
	if mint.WorkspaceID != ws.ID || mint.AccessToken == "" {
		t.Fatalf("mint = %+v", mint)
	}
	// …and that token calls back into fabric as the workspace's own identity:
	// it reads its workspace and creates items (it was granted Admin).
	f.mustStatus(f.call("GET", "/v1/workspaces/"+ws.ID, mint.AccessToken, nil, nil),
		http.StatusOK, "identity reads its workspace")
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/items", mint.AccessToken,
		map[string]string{"displayName": "by-identity", "type": "Lakehouse"}, nil),
		http.StatusCreated, "identity creates an item")

	// Name-follows-workspace: rename propagates to the entra identity.
	f.mustStatus(f.call("PATCH", "/v1/workspaces/"+ws.ID, f.token,
		map[string]string{"displayName": "renamed-ws"}, nil), http.StatusOK, "rename")
	var entraWI struct{ WorkspaceName string }
	if code := f.entraJSON(t, "GET", "/admin/api/workspace-identities/"+spID, &entraWI); code != http.StatusOK {
		t.Fatalf("entra get identity = %d", code)
	}
	if entraWI.WorkspaceName != "renamed-ws" {
		t.Fatalf("identity name = %q; want renamed-ws (name-follows-workspace)", entraWI.WorkspaceName)
	}

	// Deprovision: identity gone in entra, token grant revoked in fabric.
	resp = f.call("POST", "/v1/workspaces/"+ws.ID+"/deprovisionIdentity", f.token, nil, nil)
	f.mustStatus(resp, http.StatusAccepted, "deprovision")
	if code := f.entraJSON(t, "GET", "/admin/api/workspace-identities/"+spID, nil); code != http.StatusNotFound {
		t.Fatalf("entra identity after deprovision = %d; want 404", code)
	}
	f.mustStatus(f.call("GET", "/v1/workspaces/"+ws.ID, mint.AccessToken, nil, nil),
		http.StatusForbidden, "revoked identity token")
	var after struct {
		WorkspaceIdentity *identityShape `json:"workspaceIdentity"`
	}
	f.call("GET", "/v1/workspaces/"+ws.ID, f.token, nil, &after)
	if after.WorkspaceIdentity != nil {
		t.Fatalf("workspaceIdentity survived deprovision: %+v", after.WorkspaceIdentity)
	}
	// Deprovision again → 404.
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/deprovisionIdentity", f.token, nil, nil),
		http.StatusNotFound, "double deprovision")
}

func TestWorkspaceIdentityCascadeDelete(t *testing.T) {
	f := newFixture(t)
	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "doomed"}, &ws)
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/provisionIdentity", f.token, nil, nil),
		http.StatusAccepted, "provision")
	var got struct {
		WorkspaceIdentity *identityShape `json:"workspaceIdentity"`
	}
	f.call("GET", "/v1/workspaces/"+ws.ID, f.token, nil, &got)
	spID := got.WorkspaceIdentity.ServicePrincipalID

	// Deleting the workspace cascades the entra identity.
	f.mustStatus(f.call("DELETE", "/v1/workspaces/"+ws.ID, f.token, nil, nil), http.StatusOK, "delete ws")
	if code := f.entraJSON(t, "GET", "/admin/api/workspace-identities/"+spID, nil); code != http.StatusNotFound {
		t.Fatalf("entra identity after workspace delete = %d; want 404 (cascade)", code)
	}
}

func TestAKVReferenceConnectionViaWorkspaceIdentity(t *testing.T) {
	f := newFixture(t)

	// Provision a real workspace identity.
	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "akv-ws"}, &ws)
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/provisionIdentity", f.token, nil, nil),
		http.StatusAccepted, "provision")

	// A vault that requires a real-looking JWT minted by entra (three JWS
	// segments) — the wire-faithful stand-in for azure-keyvault-emulator;
	// the true three-emulator chain lives in that repo's compose e2e.
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if len(strings.Split(tok, ".")) != 3 {
			http.Error(w, `{"error":{"code":"Unauthorized"}}`, http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("api-version") == "" || !strings.HasSuffix(r.URL.Path, "/secrets/db-password") {
			http.Error(w, `{"error":{"code":"SecretNotFound"}}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"value":"s3cret"}`))
	}))
	defer vault.Close()
	f.srv.API.AKV = akv.New(false, vault.Client())

	body := map[string]any{
		"displayName": "akv-conn",
		"credentialDetails": map[string]any{
			"credentials": map[string]string{
				"credentialType": "AzureKeyVaultReference",
				"workspaceId":    ws.ID,
				"vaultUri":       vault.URL,
				"secretName":     "db-password",
			},
		},
	}
	resp := f.call("POST", "/v1/connections", f.token, body, nil)
	f.mustStatus(resp, http.StatusCreated, "akv-reference connection")
	raw, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(raw), "s3cret") {
		t.Fatalf("resolved secret echoed: %s", raw)
	}
	// A missing secret fails the reference at create.
	body["credentialDetails"].(map[string]any)["credentials"].(map[string]string)["secretName"] = "nope"
	f.mustStatus(f.call("POST", "/v1/connections", f.token, body, nil),
		http.StatusBadRequest, "missing secret reference")
}

func TestLivyPassthroughE2E(t *testing.T) {
	f := newFixture(t)

	// A stub "Livy" backend records the path it received.
	var gotPath string
	livy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1,"state":"starting"}`))
	}))
	defer livy.Close()
	if err := f.srv.API.SetLivyBackend(livy.URL); err != nil {
		t.Fatal(err)
	}

	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "spark-ws"}, &ws)
	var lake struct{ ID string }
	f.call("POST", "/v1/workspaces/"+ws.ID+"/lakehouses", f.token, map[string]any{"displayName": "lh"}, &lake)

	// Submit a Livy batch through the documented Fabric endpoint; a real
	// entra token is validated, RBAC checked, then proxied to the backend.
	base := "/v1/workspaces/" + ws.ID + "/lakehouses/" + lake.ID + "/livyapi/versions/2023-12-01/"
	resp := f.call("POST", base+"batches", f.token, map[string]string{"file": "abfss://…/nb.py"}, nil)
	f.mustStatus(resp, http.StatusCreated, "livy batch submit")
	if gotPath != "/batches" {
		t.Fatalf("backend path = %q; want /batches", gotPath)
	}

	// Without a backend, the endpoint is honestly 501.
	if err := f.srv.API.SetLivyBackend(""); err != nil {
		t.Fatal(err)
	}
	f.mustStatus(f.call("GET", base+"sessions", f.token, nil, nil), http.StatusNotImplemented, "no backend")
}
