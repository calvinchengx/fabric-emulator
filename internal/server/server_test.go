package server_test

// Full-stack e2e: a real entra-emulator runs in-process, mints a
// client-credentials token for the Fabric audience, and fabric-emulator
// validates it over entra's JWKS — the exact trust relationship of real
// Fabric ↔ Entra — then the whole workspace/RBAC/item/LRO surface is driven
// through HTTP.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	entra "github.com/calvinchengx/entra-emulator/emulator"
	"github.com/calvinchengx/fabric-emulator/internal/config"
	"github.com/calvinchengx/fabric-emulator/internal/server"
)

type fixture struct {
	t      *testing.T
	emu    *entra.Emulator
	srv    *server.Server
	fabric *httptest.Server
	token  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	emu := entra.StartT(t)

	cfg := &config.Config{
		EntraIssuer: emu.Origin + "/" + emu.TenantID + "/v2.0",
	}
	if err := cfg.Finish(); err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(cfg, emu.HTTPClient())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	fabric := httptest.NewServer(srv.Handler())
	t.Cleanup(fabric.Close)

	// Real client-credentials grant for the Fabric audience.
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {entra.DaemonClientID},
		"client_secret": {entra.DaemonSecret},
		"scope":         {"https://api.fabric.microsoft.com/.default"},
	}
	resp, err := emu.HTTPClient().PostForm(emu.Authority()+"/oauth2/v2.0/token", form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil || tok.AccessToken == "" {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("client credentials failed (%d): %v %s", resp.StatusCode, err, body)
	}
	return &fixture{t: t, emu: emu, srv: srv, fabric: fabric, token: tok.AccessToken}
}

// forgeUserToken mints a delegated Fabric-audience token for a seeded user
// via entra-emulator's token forge — a second, distinct principal.
func (f *fixture) forgeUserToken(userID string) string {
	f.t.Helper()
	body, _ := json.Marshal(map[string]any{
		"userId":   userID,
		"audience": "https://api.fabric.microsoft.com",
	})
	resp, err := f.emu.HTTPClient().Post(f.emu.Origin+"/admin/api/tokens", "application/json", bytes.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		Token       string `json:"token"`
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &tok); err != nil {
		f.t.Fatalf("forge: %v %s", err, raw)
	}
	if tok.AccessToken != "" {
		return tok.AccessToken
	}
	if tok.Token == "" {
		f.t.Fatalf("forge returned no token: %s", raw)
	}
	return tok.Token
}

// call performs an authenticated request and decodes JSON into out (may be nil).
func (f *fixture) call(method, path, token string, body any, out any) *http.Response {
	f.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, f.fabric.URL+path, rd)
	if err != nil {
		f.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := f.fabric.Client().Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			f.t.Fatalf("%s %s: bad JSON %q: %v", method, path, raw, err)
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	return resp
}

func (f *fixture) mustStatus(resp *http.Response, want int, ctx string) {
	f.t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		f.t.Fatalf("%s: status %d; want %d — %s", ctx, resp.StatusCode, want, body)
	}
}

func TestEndToEndAgainstEntraEmulator(t *testing.T) {
	f := newFixture(t)

	// Unauthenticated and garbage tokens are rejected.
	f.mustStatus(f.call("GET", "/v1/workspaces", "", nil, nil), http.StatusUnauthorized, "no token")
	f.mustStatus(f.call("GET", "/v1/workspaces", "garbage", nil, nil), http.StatusUnauthorized, "garbage token")

	// Create a workspace; the SP becomes Admin and sees it in its list.
	var ws struct{ ID, DisplayName, Type string }
	resp := f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "Analytics"}, &ws)
	f.mustStatus(resp, http.StatusCreated, "create workspace")
	if ws.Type != "Workspace" || ws.ID == "" {
		t.Fatalf("workspace shape: %+v", ws)
	}
	var list struct {
		Value []struct{ ID string }
	}
	f.mustStatus(f.call("GET", "/v1/workspaces", f.token, nil, &list), http.StatusOK, "list")
	if len(list.Value) != 1 || list.Value[0].ID != ws.ID {
		t.Fatalf("list = %+v", list)
	}

	// The creator's auto-grant is visible and is Admin.
	var ras struct {
		Value []struct {
			ID   string
			Role string
		}
	}
	f.mustStatus(f.call("GET", "/v1/workspaces/"+ws.ID+"/roleAssignments", f.token, nil, &ras), http.StatusOK, "list roles")
	if len(ras.Value) != 1 || ras.Value[0].Role != "Admin" {
		t.Fatalf("role assignments = %+v", ras)
	}

	// Grant + patch + revoke a Viewer.
	var ra struct{ ID string }
	resp = f.call("POST", "/v1/workspaces/"+ws.ID+"/roleAssignments", f.token,
		map[string]any{"principal": map[string]string{"id": "user-9", "type": "User"}, "role": "Viewer"}, &ra)
	f.mustStatus(resp, http.StatusCreated, "grant viewer")
	f.mustStatus(f.call("PATCH", "/v1/workspaces/"+ws.ID+"/roleAssignments/"+ra.ID, f.token,
		map[string]string{"role": "Contributor"}, nil), http.StatusOK, "patch role")
	f.mustStatus(f.call("DELETE", "/v1/workspaces/"+ws.ID+"/roleAssignments/"+ra.ID, f.token, nil, nil),
		http.StatusOK, "revoke")

	// Item without definition: synchronous 201.
	var it struct{ ID, Type string }
	resp = f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token,
		map[string]string{"displayName": "lh", "type": "Lakehouse"}, &it)
	f.mustStatus(resp, http.StatusCreated, "create item sync")

	// Item with definition: 202 + operation headers, then LRO poll.
	req := map[string]any{
		"displayName": "nb", "type": "Notebook",
		"definition": map[string]any{"parts": []map[string]string{
			{"path": ".platform", "payload": "e30=", "payloadType": "InlineBase64"},
		}},
	}
	resp = f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token, req, nil)
	f.mustStatus(resp, http.StatusAccepted, "create item async")
	opID := resp.Header.Get("x-ms-operation-id")
	loc := resp.Header.Get("Location")
	if opID == "" || !strings.Contains(loc, "/v1/operations/"+opID) || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("202 headers: op=%q loc=%q retry=%q", opID, loc, resp.Header.Get("Retry-After"))
	}
	var op struct{ ID, Status string }
	f.mustStatus(f.call("GET", "/v1/operations/"+opID, f.token, nil, &op), http.StatusOK, "poll")
	if op.Status != "Succeeded" { // default LRO delay 0: completes on next poll
		t.Fatalf("operation status = %q; want Succeeded", op.Status)
	}
	var created struct{ ID, WorkspaceID, Type string }
	f.mustStatus(f.call("GET", "/v1/operations/"+opID+"/result", f.token, nil, &created), http.StatusOK, "result")
	if created.Type != "Notebook" || created.WorkspaceID != ws.ID {
		t.Fatalf("operation result = %+v", created)
	}

	// Items list shows both; type filter narrows.
	var items struct{ Value []struct{ Type string } }
	f.mustStatus(f.call("GET", "/v1/workspaces/"+ws.ID+"/items?type=Notebook", f.token, nil, &items), http.StatusOK, "filter")
	if len(items.Value) != 1 || items.Value[0].Type != "Notebook" {
		t.Fatalf("filtered items = %+v", items)
	}

	// Workspace delete cascades; the list is empty again.
	f.mustStatus(f.call("DELETE", "/v1/workspaces/"+ws.ID, f.token, nil, nil), http.StatusOK, "delete ws")
	f.mustStatus(f.call("GET", "/v1/workspaces/"+ws.ID, f.token, nil, nil), http.StatusNotFound, "gone")
}

func TestLROOnTheControllableClock(t *testing.T) {
	f := newFixture(t)

	// Freeze time and make new operations take 300 virtual seconds.
	f.mustStatus(f.call("POST", "/_emulator/clock", "", map[string]bool{"freeze": true}, nil), http.StatusOK, "freeze")
	f.mustStatus(f.call("POST", "/_emulator/faults", "", map[string]int64{"lroDelaySeconds": 300}, nil), http.StatusOK, "slow lro")

	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "w"}, &ws)
	req := map[string]any{
		"displayName": "nb", "type": "Notebook",
		"definition": map[string]any{"parts": []map[string]string{
			{"path": ".platform", "payload": "e30=", "payloadType": "InlineBase64"},
		}},
	}
	resp := f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token, req, nil)
	f.mustStatus(resp, http.StatusAccepted, "async create")
	opID := resp.Header.Get("x-ms-operation-id")

	// Frozen at creation instant → NotStarted; +1s → Running; +300s → Succeeded.
	var op struct{ Status string }
	f.call("GET", "/v1/operations/"+opID, f.token, nil, &op)
	if op.Status != "NotStarted" {
		t.Fatalf("at t0: %q; want NotStarted", op.Status)
	}
	f.call("POST", "/_emulator/clock", "", map[string]int64{"advance": 1}, nil)
	f.call("GET", "/v1/operations/"+opID, f.token, nil, &op)
	if op.Status != "Running" {
		t.Fatalf("at t+1: %q; want Running", op.Status)
	}
	f.call("POST", "/_emulator/clock", "", map[string]int64{"advance": 300}, nil)
	f.call("GET", "/v1/operations/"+opID, f.token, nil, &op)
	if op.Status != "Succeeded" {
		t.Fatalf("at t+301: %q; want Succeeded", op.Status)
	}

	// Fault: force the next operation to fail.
	f.mustStatus(f.call("POST", "/_emulator/faults", "", map[string]int64{"lroDelaySeconds": 0}, nil), http.StatusOK, "reset delay")
	f.call("POST", "/_emulator/faults", "", map[string]int{"failNextOperations": 1}, nil)
	resp = f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token, req, nil)
	opID = resp.Header.Get("x-ms-operation-id")
	f.call("POST", "/_emulator/clock", "", map[string]int64{"advance": 1}, nil)
	var failed struct {
		Status string
		Error  *struct{ ErrorCode string }
	}
	f.call("GET", "/v1/operations/"+opID, f.token, nil, &failed)
	if failed.Status != "Failed" || failed.Error == nil || failed.Error.ErrorCode == "" {
		t.Fatalf("faulted op = %+v; want Failed with errorCode", failed)
	}
	// Result of a failed operation is a 400.
	f.mustStatus(f.call("GET", "/v1/operations/"+opID+"/result", f.token, nil, nil),
		http.StatusBadRequest, "failed result")
}

func TestRBACAcrossPrincipals(t *testing.T) {
	f := newFixture(t)
	alice := f.forgeUserToken(entra.AliceOID)

	// The daemon SP creates a workspace; Alice holds no role on it.
	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "locked"}, &ws)
	f.mustStatus(f.call("GET", "/v1/workspaces/"+ws.ID, alice, nil, nil),
		http.StatusForbidden, "ungranted principal reads workspace")
	// And it is absent from her list.
	var list struct{ Value []struct{ ID string } }
	f.mustStatus(f.call("GET", "/v1/workspaces", alice, nil, &list), http.StatusOK, "alice list")
	if len(list.Value) != 0 {
		t.Fatalf("alice sees %d workspaces; want 0", len(list.Value))
	}

	// Viewer: can read, cannot mutate, cannot see the access list.
	f.call("POST", "/v1/workspaces/"+ws.ID+"/roleAssignments", f.token,
		map[string]any{"principal": map[string]string{"id": entra.AliceOID, "type": "User"}, "role": "Viewer"}, nil)
	f.mustStatus(f.call("GET", "/v1/workspaces/"+ws.ID, alice, nil, nil), http.StatusOK, "viewer reads")
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/items", alice,
		map[string]string{"displayName": "x", "type": "Notebook"}, nil), http.StatusForbidden, "viewer creates item")
	f.mustStatus(f.call("GET", "/v1/workspaces/"+ws.ID+"/roleAssignments", alice, nil, nil),
		http.StatusForbidden, "viewer lists roles")
	f.mustStatus(f.call("DELETE", "/v1/workspaces/"+ws.ID, alice, nil, nil),
		http.StatusForbidden, "viewer deletes workspace")

	// Member: can grant ≤ Member, cannot grant Admin, cannot patch workspace.
	var ras struct {
		Value []struct {
			ID        string
			Principal struct{ ID string }
		}
	}
	f.call("GET", "/v1/workspaces/"+ws.ID+"/roleAssignments", f.token, nil, &ras)
	var aliceRA string
	for _, ra := range ras.Value {
		if ra.Principal.ID == entra.AliceOID {
			aliceRA = ra.ID
		}
	}
	f.mustStatus(f.call("PATCH", "/v1/workspaces/"+ws.ID+"/roleAssignments/"+aliceRA, f.token,
		map[string]string{"role": "Member"}, nil), http.StatusOK, "promote to member")
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/roleAssignments", alice,
		map[string]any{"principal": map[string]string{"id": entra.BobOID, "type": "User"}, "role": "Contributor"}, nil),
		http.StatusCreated, "member grants contributor")
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/roleAssignments", alice,
		map[string]any{"principal": map[string]string{"id": "other-user", "type": "User"}, "role": "Admin"}, nil),
		http.StatusForbidden, "member grants admin")
	f.mustStatus(f.call("PATCH", "/v1/workspaces/"+ws.ID, alice,
		map[string]string{"description": "x"}, nil), http.StatusForbidden, "member patches workspace")

	// Member can create items.
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/items", alice,
		map[string]string{"displayName": "nb", "type": "Notebook"}, nil), http.StatusCreated, "member creates item")
}

func TestTokenExpiryFollowsEmulatorClock(t *testing.T) {
	f := newFixture(t)
	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "w"}, &ws)

	// Advancing fabric's clock past the token lifetime invalidates the same
	// token — validation runs on the controllable clock, not wall time.
	f.mustStatus(f.call("GET", "/v1/workspaces/"+ws.ID, f.token, nil, nil), http.StatusOK, "valid before expiry")
	f.call("POST", "/_emulator/clock", "", map[string]int64{"advance": 7200}, nil) // tokens live 1h
	resp := f.call("GET", "/v1/workspaces/"+ws.ID, f.token, nil, nil)
	f.mustStatus(resp, http.StatusUnauthorized, "expired by clock advance")
	if !strings.Contains(resp.Header.Get("WWW-Authenticate"), "Bearer") {
		t.Fatal("401 missing WWW-Authenticate challenge")
	}
}

func TestHealthAndClockEndpoints(t *testing.T) {
	f := newFixture(t)
	var health struct {
		Status string
		Now    int64
	}
	f.mustStatus(f.call("GET", "/health", "", nil, &health), http.StatusOK, "health")
	if health.Status != "ok" || health.Now == 0 {
		t.Fatalf("health = %+v", health)
	}
	var ck struct {
		Offset int64
		Frozen bool
		Now    int64
	}
	f.mustStatus(f.call("POST", "/_emulator/clock", "", map[string]int64{"offset": 100}, &ck), http.StatusOK, "offset")
	if ck.Offset != 100 || ck.Frozen {
		t.Fatalf("clock = %+v", ck)
	}
}
