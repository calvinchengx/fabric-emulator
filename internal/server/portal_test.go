package server_test

// The operator portal: unauthenticated read-only /_emulator/portal endpoints
// plus the embedded SPA served at "/". State is created through the real
// authenticated /v1 surface, then observed through the portal's eyes.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/config"
	"github.com/calvinchengx/fabric-emulator/internal/server"
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

// createItemNow creates an item through /v1 and returns its id (item rows
// exist immediately; only the LRO status is clock-derived).
func (f *fixture) createItemNow(t *testing.T, wsID, itemType, name string) string {
	t.Helper()
	resp := f.call("POST", "/v1/workspaces/"+wsID+"/items", f.token,
		map[string]any{"displayName": name, "type": itemType}, nil)
	if resp.StatusCode != 201 && resp.StatusCode != 202 {
		t.Fatalf("create %s: %d", itemType, resp.StatusCode)
	}
	var items struct {
		Value []struct {
			ID          string `json:"id"`
			DisplayName string `json:"displayName"`
		} `json:"value"`
	}
	f.mustStatus(f.call("GET", "/v1/workspaces/"+wsID+"/items", f.token, nil, &items), 200, "list items")
	for _, it := range items.Value {
		if it.DisplayName == name {
			return it.ID
		}
	}
	t.Fatalf("item %q not found after create", name)
	return ""
}

func TestPortalConnections(t *testing.T) {
	f := newFixture(t)

	var empty struct {
		Value []json.RawMessage `json:"value"`
	}
	if code := f.portalJSON(t, "/_emulator/portal/connections", &empty); code != 200 {
		t.Fatalf("portal connections: %d", code)
	}
	if len(empty.Value) != 0 {
		t.Fatalf("expected no connections, got %d", len(empty.Value))
	}

	resp := f.call("POST", "/v1/connections", f.token, map[string]any{
		"displayName":       "sql-basic",
		"connectivityType":  "ShareableCloud",
		"connectionDetails": map[string]any{"type": "SQL", "path": "srv;db"},
		"credentialDetails": map[string]any{
			"connectionEncryption": "NotEncrypted",
			"credentials":          map[string]any{"credentialType": "Basic", "username": "sa", "password": "hunter2"},
		},
	}, nil)
	f.mustStatus(resp, 201, "create connection")

	// Raw body: metadata present, secret material never serialized.
	httpResp, err := http.Get(f.fabric.URL + "/_emulator/portal/connections")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(httpResp.Body)
	httpResp.Body.Close()
	body := string(raw)
	for _, want := range []string{`"connectivityType":"ShareableCloud"`, `"credentialType":"Basic"`, `"displayName":"sql-basic"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("connections missing %s: %s", want, body)
		}
	}
	for _, leak := range []string{"hunter2", "password", "credentials", "connectionDetails", "srv;db"} {
		if strings.Contains(body, leak) {
			t.Fatalf("connections leaked %q: %s", leak, body)
		}
	}
}

func TestPortalShortcuts(t *testing.T) {
	f := newFixture(t)

	var ws struct {
		ID string `json:"id"`
	}
	f.mustStatus(f.call("POST", "/v1/workspaces", f.token, map[string]any{"displayName": "sc-ws"}, &ws), 201, "create workspace")
	src := f.createItemNow(t, ws.ID, "Lakehouse", "src-lh")
	dst := f.createItemNow(t, ws.ID, "Lakehouse", "dst-lh")

	resp := f.call("POST", "/v1/workspaces/"+ws.ID+"/items/"+src+"/shortcuts", f.token, map[string]any{
		"path": "Files", "name": "linked",
		"target": map[string]any{"oneLake": map[string]any{"workspaceId": ws.ID, "itemId": dst, "path": "Files/raw"}},
	}, nil)
	f.mustStatus(resp, 201, "create shortcut")

	var list struct {
		Value []struct {
			WorkspaceName     string `json:"workspaceName"`
			ItemID            string `json:"itemId"`
			ItemName          string `json:"itemName"`
			Path              string `json:"path"`
			Name              string `json:"name"`
			TargetWorkspaceID string `json:"targetWorkspaceId"`
			TargetItemID      string `json:"targetItemId"`
			TargetPath        string `json:"targetPath"`
			Dangling          bool   `json:"dangling"`
		} `json:"value"`
	}
	if code := f.portalJSON(t, "/_emulator/portal/shortcuts", &list); code != 200 {
		t.Fatalf("portal shortcuts: %d", code)
	}
	if len(list.Value) != 1 {
		t.Fatalf("want 1 shortcut, got %d", len(list.Value))
	}
	sc := list.Value[0]
	if sc.WorkspaceName != "sc-ws" || sc.ItemID != src || sc.ItemName != "src-lh" ||
		sc.Path != "Files" || sc.Name != "linked" ||
		sc.TargetWorkspaceID != ws.ID || sc.TargetItemID != dst || sc.TargetPath != "Files/raw" {
		t.Fatalf("shortcut row wrong: %+v", sc)
	}
	if sc.Dangling {
		t.Fatal("live target reported dangling")
	}

	// Delete the target item: the shortcut now dangles.
	f.mustStatus(f.call("DELETE", "/v1/workspaces/"+ws.ID+"/items/"+dst, f.token, nil, nil), 200, "delete target")
	if code := f.portalJSON(t, "/_emulator/portal/shortcuts", &list); code != 200 {
		t.Fatalf("portal shortcuts: %d", code)
	}
	if len(list.Value) != 1 || !list.Value[0].Dangling {
		t.Fatalf("deleted target should dangle: %+v", list.Value)
	}
}

func TestPortalCapacities(t *testing.T) {
	f := newFixture(t)

	var ws struct {
		ID string `json:"id"`
	}
	f.mustStatus(f.call("POST", "/v1/workspaces", f.token, map[string]any{"displayName": "cap-ws"}, &ws), 201, "create workspace")

	var list struct {
		Value []struct {
			ID          string `json:"id"`
			DisplayName string `json:"displayName"`
			SKU         string `json:"sku"`
			State       string `json:"state"`
			Workspaces  []struct {
				ID          string `json:"id"`
				DisplayName string `json:"displayName"`
			} `json:"workspaces"`
		} `json:"value"`
	}
	if code := f.portalJSON(t, "/_emulator/portal/capacities", &list); code != 200 {
		t.Fatalf("portal capacities: %d", code)
	}
	if len(list.Value) != 1 {
		t.Fatalf("want the seeded capacity, got %d", len(list.Value))
	}
	cap := list.Value[0]
	if cap.DisplayName != "Emulator Capacity" || cap.SKU != "F64" || cap.State != "Active" {
		t.Fatalf("seeded capacity wrong: %+v", cap)
	}
	// Workspaces auto-assign to the seeded capacity.
	if len(cap.Workspaces) != 1 || cap.Workspaces[0].ID != ws.ID || cap.Workspaces[0].DisplayName != "cap-ws" {
		t.Fatalf("assignment wrong: %+v", cap.Workspaces)
	}
}

func TestPortalJobs(t *testing.T) {
	f := newFixture(t)

	var empty struct {
		Value []json.RawMessage `json:"value"`
	}
	if code := f.portalJSON(t, "/_emulator/portal/jobs", &empty); code != 200 {
		t.Fatalf("portal jobs: %d", code)
	}
	if len(empty.Value) != 0 {
		t.Fatalf("expected no jobs, got %d", len(empty.Value))
	}

	var ws struct {
		ID string `json:"id"`
	}
	f.mustStatus(f.call("POST", "/v1/workspaces", f.token, map[string]any{"displayName": "job-ws"}, &ws), 201, "create workspace")
	item := f.createItemNow(t, ws.ID, "Notebook", "job-nb")
	resp := f.call("POST", "/v1/workspaces/"+ws.ID+"/items/"+item+"/jobs/instances?jobType=DefaultJob", f.token, nil, nil)
	f.mustStatus(resp, 202, "create job")

	var jobs struct {
		Value []struct {
			ID          string `json:"id"`
			ItemID      string `json:"itemId"`
			ItemName    string `json:"itemName"`
			ItemType    string `json:"itemType"`
			WorkspaceID string `json:"workspaceId"`
			JobType     string `json:"jobType"`
			Status      string `json:"status"`
		} `json:"value"`
	}
	if code := f.portalJSON(t, "/_emulator/portal/jobs", &jobs); code != 200 {
		t.Fatalf("portal jobs: %d", code)
	}
	if len(jobs.Value) != 1 {
		t.Fatalf("want 1 job, got %d", len(jobs.Value))
	}
	j := jobs.Value[0]
	if j.ItemID != item || j.ItemName != "job-nb" || j.ItemType != "Notebook" ||
		j.WorkspaceID != ws.ID || j.JobType != "DefaultJob" || j.ID == "" {
		t.Fatalf("job row wrong: %+v", j)
	}
	switch j.Status {
	case "NotStarted", "InProgress", "Completed", "Failed", "Cancelled":
	default:
		t.Fatalf("bad derived status %q", j.Status)
	}
}

func TestPortalWarehouse(t *testing.T) {
	f := newFixture(t)

	var wh struct {
		SQLTDSConfigured       bool   `json:"sqlTdsConfigured"`
		WarehouseSQLConfigured bool   `json:"warehouseSqlConfigured"`
		TDSListener            string `json:"tdsListener"`
	}
	if code := f.portalJSON(t, "/_emulator/portal/warehouse", &wh); code != 200 {
		t.Fatalf("portal warehouse: %d", code)
	}
	if wh.SQLTDSConfigured || wh.WarehouseSQLConfigured || wh.TDSListener != "off" {
		t.Fatalf("unconfigured fixture should report off: %+v", wh)
	}
}

func TestPortalWarehouseTDSConfigured(t *testing.T) {
	f := newFixture(t)

	// A server wired with a TDS address (but no SQL backend) reports the stub
	// listener — and never echoes config values, only presence.
	cfg := &config.Config{EntraIssuer: f.emu.Origin + "/" + f.emu.TenantID + "/v2.0", SQLTDSAddr: ":1433"}
	if err := cfg.Finish(); err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(cfg, f.emu.HTTPClient())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	fabric := httptest.NewServer(srv.Handler())
	t.Cleanup(fabric.Close)

	resp, err := http.Get(fabric.URL + "/_emulator/portal/warehouse")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("portal warehouse: %d", resp.StatusCode)
	}
	var wh struct {
		SQLTDSConfigured       bool   `json:"sqlTdsConfigured"`
		WarehouseSQLConfigured bool   `json:"warehouseSqlConfigured"`
		TDSListener            string `json:"tdsListener"`
	}
	if err := json.Unmarshal(raw, &wh); err != nil {
		t.Fatal(err)
	}
	if !wh.SQLTDSConfigured || wh.WarehouseSQLConfigured || wh.TDSListener != "stub" {
		t.Fatalf("TDS-configured server should report stub: %+v", wh)
	}
	if strings.Contains(string(raw), "1433") {
		t.Fatalf("config value echoed: %s", raw)
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
