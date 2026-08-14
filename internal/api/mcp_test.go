package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func mcpRPC(t *testing.T, a *API, method, params string) map[string]any {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `"`
	if params != "" {
		body += `,"params":` + params
	}
	body += `}`
	w := do(a.handleMCPPost, admin, "POST", body, nil)
	var env map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("%s: %s", method, w.Body.Bytes())
	}
	return env
}

func mcpText(t *testing.T, env map[string]any) string {
	t.Helper()
	return env["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
}

func TestMCPInitializeAndToolList(t *testing.T) {
	a, _ := newAPI(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	w := do(a.handleMCPPost, admin, "POST", body, nil)
	if w.Result().Header.Get("Mcp-Session-Id") == "" {
		t.Fatal("missing Mcp-Session-Id")
	}
	var env map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	result := env["result"].(map[string]any)
	if result["protocolVersion"] != "2025-03-26" {
		t.Fatalf("protocol = %v", result["protocolVersion"])
	}
	if result["serverInfo"].(map[string]any)["name"] != "fabric-core" {
		t.Fatalf("server = %v", result["serverInfo"])
	}

	env = mcpRPC(t, a, "tools/list", "")
	tools := env["result"].(map[string]any)["tools"].([]any)
	got := map[string]bool{}
	for _, raw := range tools {
		got[raw.(map[string]any)["name"].(string)] = true
	}
	for _, name := range mcpToolNames {
		if !got[name] {
			t.Errorf("missing tool %s", name)
		}
	}
	if len(got) != len(mcpToolNames) {
		t.Fatalf("tool count = %d want %d", len(got), len(mcpToolNames))
	}
}

func TestMCPListAndCreateWorkspace(t *testing.T) {
	a, _ := newAPI(t)
	text := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"create_workspace","arguments":{"display_name":"Sales Analytics Dev"}}`))
	var wrap struct {
		Status int
		Body   struct{ DisplayName string }
	}
	if err := json.Unmarshal([]byte(text), &wrap); err != nil {
		t.Fatal(text, err)
	}
	if wrap.Status != http.StatusCreated || wrap.Body.DisplayName != "Sales Analytics Dev" {
		t.Fatalf("create = %s", text)
	}
	list := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"list_workspaces","arguments":{}}`))
	if !strings.Contains(list, "Sales Analytics Dev") {
		t.Fatalf("list = %s", list)
	}
}

func TestMCPSearchCatalogAndRBAC(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	if err := st.CreateItem(&store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "CustomerData"}, nil); err != nil {
		t.Fatal(err)
	}
	text := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"search_catalog","arguments":{"search":"Customer","filter":"Type eq 'Lakehouse'"}}`))
	if !strings.Contains(text, "CustomerData") {
		t.Fatalf("search = %s", text)
	}

	w := do(a.handleMCPPost, nobody, "POST",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_catalog","arguments":{"search":"Customer"}}}`, nil)
	var env map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	text = mcpText(t, env)
	if strings.Contains(text, "CustomerData") {
		t.Fatalf("nobody saw the lakehouse: %s", text)
	}
}

func TestMCPUnknownToolAndMethod(t *testing.T) {
	a, _ := newAPI(t)
	env := mcpRPC(t, a, "tools/call", `{"name":"not_a_tool","arguments":{}}`)
	if env["result"].(map[string]any)["isError"] != true {
		t.Fatalf("unknown tool = %v", env)
	}
	env = mcpRPC(t, a, "nope", "")
	if env["error"] == nil {
		t.Fatalf("unknown method = %v", env)
	}
	w := do(a.handleMCPPost, admin, "POST", `{`, nil)
	var env3 map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &env3)
	if env3["error"] == nil {
		t.Fatalf("parse = %s", w.Body.Bytes())
	}
	if w := do(a.handleMCPGet, admin, "GET", "", nil); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d", w.Code)
	}
	if w := do(a.handleMCPDelete, admin, "DELETE", "", nil); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d", w.Code)
	}
	if w := do(a.handleMCPPost, admin, "POST", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil); w.Code != http.StatusAccepted {
		t.Fatalf("notification = %d", w.Code)
	}
}

func TestMCPCreateItemAndFolder(t *testing.T) {
	a, _ := newAPI(t)
	text := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"create_workspace","arguments":{"display_name":"Dev"}}`))
	var created struct {
		Body struct{ ID string }
	}
	_ = json.Unmarshal([]byte(text), &created)
	wid := created.Body.ID

	text = mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"create_folder","arguments":{"workspace_id":"`+wid+`","display_name":"src"}}`))
	var folder struct {
		Status int
		Body   struct{ ID string }
	}
	_ = json.Unmarshal([]byte(text), &folder)
	if folder.Status != http.StatusCreated {
		t.Fatalf("folder = %s", text)
	}

	text = mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"create_item","arguments":{"workspace_id":"`+wid+`","display_name":"CustomerData","type":"Lakehouse","folder_id":"`+folder.Body.ID+`"}}`))
	if !strings.Contains(text, "CustomerData") {
		t.Fatalf("item = %s", text)
	}

	text = mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"get_knowledge","arguments":{"item_type":"Lakehouse"}}`))
	if !strings.Contains(text, "Lakehouse") {
		t.Fatalf("knowledge = %s", text)
	}
}

func TestMCPToolRoundTrip(t *testing.T) {
	a, _ := newAPI(t)
	text := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"create_workspace","arguments":{"display_name":"RoundTrip","description":"d"}}`))
	var created struct {
		Status int
		Body   struct{ ID string }
	}
	if err := json.Unmarshal([]byte(text), &created); err != nil || created.Body.ID == "" {
		t.Fatalf("create ws = %s", text)
	}
	wid := created.Body.ID

	if ping := mcpRPC(t, a, "ping", ""); ping["result"] == nil {
		t.Fatalf("ping = %v", ping)
	}

	get := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"get_workspace","arguments":{"workspace_id":"`+wid+`"}}`))
	if !strings.Contains(get, "RoundTrip") {
		t.Fatalf("get ws = %s", get)
	}
	upd := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"update_workspace","arguments":{"workspace_id":"`+wid+`","description":"updated"}}`))
	if !strings.Contains(upd, "updated") {
		t.Fatalf("update ws = %s", upd)
	}
	upd = mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"update_workspace","arguments":{"workspace_id":"`+wid+`","display_name":"RoundTrip2"}}`))
	if !strings.Contains(upd, "RoundTrip2") {
		t.Fatalf("rename ws = %s", upd)
	}

	caps := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"list_capacities","arguments":{}}`))
	if !strings.Contains(caps, "Emulator Capacity") && !strings.Contains(caps, "displayName") {
		t.Fatalf("capacities = %s", caps)
	}

	role := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"add_workspace_role","arguments":{"workspace_id":"`+wid+`","principal_id":"user-9","role":"Contributor"}}`))
	var ra struct {
		Body struct{ ID string }
	}
	_ = json.Unmarshal([]byte(role), &ra)
	if ra.Body.ID == "" {
		t.Fatalf("add role = %s", role)
	}
	if list := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"list_workspace_roles","arguments":{"workspace_id":"`+wid+`"}}`)); !strings.Contains(list, "user-9") {
		t.Fatalf("list roles = %s", list)
	}
	if got := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"get_workspace_role","arguments":{"workspace_id":"`+wid+`","role_assignment_id":"`+ra.Body.ID+`"}}`)); !strings.Contains(got, "Contributor") {
		t.Fatalf("get role = %s", got)
	}
	if upd := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"update_workspace_role","arguments":{"workspace_id":"`+wid+`","role_assignment_id":"`+ra.Body.ID+`","role":"Viewer"}}`)); !strings.Contains(upd, "Viewer") {
		t.Fatalf("update role = %s", upd)
	}

	item := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"create_item","arguments":{"workspace_id":"`+wid+`","display_name":"nb","type":"Notebook"}}`))
	var it struct {
		Body struct{ ID string }
	}
	_ = json.Unmarshal([]byte(item), &it)
	if it.Body.ID == "" {
		t.Fatalf("create item = %s", item)
	}
	if got := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"get_item","arguments":{"workspace_id":"`+wid+`","item_id":"`+it.Body.ID+`"}}`)); !strings.Contains(got, "nb") {
		t.Fatalf("get item = %s", got)
	}
	if list := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"list_items","arguments":{"workspace_id":"`+wid+`","type":"Notebook"}}`)); !strings.Contains(list, "nb") {
		t.Fatalf("list items = %s", list)
	}
	if upd := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"update_item","arguments":{"workspace_id":"`+wid+`","item_id":"`+it.Body.ID+`","description":"n"}}`)); !strings.Contains(upd, `"status":200`) {
		t.Fatalf("update item = %s", upd)
	}
	if upd := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"update_item","arguments":{"workspace_id":"`+wid+`","item_id":"`+it.Body.ID+`","display_name":"nb2"}}`)); !strings.Contains(upd, "nb2") {
		t.Fatalf("rename item = %s", upd)
	}
	def := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"update_item_definition","arguments":{"workspace_id":"`+wid+`","item_id":"`+it.Body.ID+`","definition":{"parts":[{"path":"notebook-content.py","payload":"cA==","payloadType":"InlineBase64"}]}}}`))
	if !strings.Contains(def, `"status":202`) && !strings.Contains(def, `"status":200`) {
		t.Fatalf("update def = %s", def)
	}
	if !strings.Contains(def, `"operationId"`) || !strings.Contains(def, `"location"`) {
		t.Fatalf("LRO wrap missing operationId/location: %s", def)
	}
	if got := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"get_item_definition","arguments":{"workspace_id":"`+wid+`","item_id":"`+it.Body.ID+`"}}`)); !strings.Contains(got, "parts") && !strings.Contains(got, "operation") {
		t.Fatalf("get def = %s", got)
	}

	folder := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"create_folder","arguments":{"workspace_id":"`+wid+`","display_name":"dst"}}`))
	var fol struct {
		Body struct{ ID string }
	}
	_ = json.Unmarshal([]byte(folder), &fol)
	if fol.Body.ID == "" {
		t.Fatalf("folder = %s", folder)
	}
	if got := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"get_folder","arguments":{"workspace_id":"`+wid+`","folder_id":"`+fol.Body.ID+`"}}`)); !strings.Contains(got, "dst") {
		t.Fatalf("get folder = %s", got)
	}
	if list := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"list_folders","arguments":{"workspace_id":"`+wid+`"}}`)); !strings.Contains(list, "dst") {
		t.Fatalf("list folders = %s", list)
	}
	if upd := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"update_folder","arguments":{"workspace_id":"`+wid+`","folder_id":"`+fol.Body.ID+`","display_name":"dest"}}`)); !strings.Contains(upd, "dest") {
		t.Fatalf("rename folder = %s", upd)
	}
	moved := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"bulk_move_items","arguments":{"workspace_id":"`+wid+`","item_ids":["`+it.Body.ID+`"],"target_folder_id":"`+fol.Body.ID+`"}}`))
	if !strings.Contains(moved, `"status":200`) {
		t.Fatalf("bulk move = %s", moved)
	}
	if mv := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"move_folder","arguments":{"workspace_id":"`+wid+`","folder_id":"`+fol.Body.ID+`"}}`)); !strings.Contains(mv, `"status":200`) {
		t.Fatalf("move folder = %s", mv)
	}

	_ = mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"delete_item","arguments":{"workspace_id":"`+wid+`","item_id":"`+it.Body.ID+`"}}`))
	if del := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"delete_folder","arguments":{"workspace_id":"`+wid+`","folder_id":"`+fol.Body.ID+`"}}`)); !strings.Contains(del, `"status":200`) {
		t.Fatalf("delete folder = %s", del)
	}
	_ = mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"delete_workspace_role","arguments":{"workspace_id":"`+wid+`","role_assignment_id":"`+ra.Body.ID+`"}}`))

	missing := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"get_operation_state","arguments":{"operation_id":"nope"}}`))
	if !strings.Contains(missing, `"status":404`) {
		t.Fatalf("op state = %s", missing)
	}
	missing = mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"get_operation_result","arguments":{"operation_id":"nope"}}`))
	if !strings.Contains(missing, `"status":404`) {
		t.Fatalf("op result = %s", missing)
	}
	for _, kind := range []string{"Notebook", "Warehouse", "DataPipeline", ""} {
		k := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"get_knowledge","arguments":{"item_type":"`+kind+`"}}`))
		if k == "" {
			t.Fatalf("knowledge %q empty", kind)
		}
	}
	_ = mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"delete_workspace","arguments":{"workspace_id":"`+wid+`"}}`))
}

func TestMCPProtocolEdges(t *testing.T) {
	a, _ := newAPI(t)

	w := do(a.handleMCPPost, admin, "POST", `{"id":1,"method":"ping"}`, nil)
	var env map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil || env["result"] == nil {
		t.Fatalf("empty jsonrpc = %s", w.Body.Bytes())
	}

	if w := do(a.handleMCPPost, admin, "POST",
		`{"jsonrpc":"2.0","id":null,"method":"notifications/cancelled"}`, nil); w.Code != http.StatusAccepted {
		t.Fatalf("id null cancelled = %d", w.Code)
	}
	if w := do(a.handleMCPPost, admin, "POST",
		`{"jsonrpc":"2.0","method":"tools/list"}`, nil); w.Code != http.StatusAccepted {
		t.Fatalf("no-id notification = %d", w.Code)
	}

	w = do(a.handleMCPPost, admin, "POST",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1.0"}}`, nil)
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env["result"].(map[string]any)["protocolVersion"] != "2025-03-26" {
		t.Fatalf("unknown protocol = %v", env["result"])
	}
	for _, ver := range []string{"2024-11-05", "2025-06-18"} {
		w = do(a.handleMCPPost, admin, "POST",
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+ver+`"}}`, nil)
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		if env["result"].(map[string]any)["protocolVersion"] != ver {
			t.Fatalf("protocol %s = %v", ver, env["result"])
		}
	}

	w = do(a.handleMCPPost, admin, "POST",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"nope"}`, nil)
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env["result"].(map[string]any)["isError"] != true {
		t.Fatalf("bad params = %s", w.Body.Bytes())
	}

	text := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"list_workspaces"}`))
	if !strings.Contains(text, `"status":200`) {
		t.Fatalf("omitted arguments = %s", text)
	}
}

func TestMCPOptionalArgsAndAliases(t *testing.T) {
	a, _ := newAPI(t)
	caps := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"list_capacities","arguments":{}}`))
	var capWrap struct {
		Body struct {
			Value []struct{ ID string }
		}
	}
	_ = json.Unmarshal([]byte(caps), &capWrap)
	if len(capWrap.Body.Value) == 0 {
		t.Fatalf("no capacities: %s", caps)
	}

	text := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"create_workspace","arguments":{"displayName":"OptArgs","description":"d","capacity_id":"`+capWrap.Body.Value[0].ID+`"}}`))
	var created struct {
		Body struct{ ID string }
	}
	if err := json.Unmarshal([]byte(text), &created); err != nil || created.Body.ID == "" {
		t.Fatalf("create ws = %s", text)
	}
	wid := created.Body.ID

	parent := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"create_folder","arguments":{"workspaceId":"`+wid+`","displayName":"src"}}`))
	var fol struct {
		Body struct{ ID string }
	}
	_ = json.Unmarshal([]byte(parent), &fol)
	nested := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"create_folder","arguments":{"workspace_id":"`+wid+`","display_name":"nested","parent_folder_id":"`+fol.Body.ID+`"}}`))
	if !strings.Contains(nested, `"status":201`) {
		t.Fatalf("nested folder = %s", nested)
	}

	item := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"create_item","arguments":{"workspaceId":"`+wid+`","displayName":"lh","type":"Lakehouse","description":"desc","folderId":"`+fol.Body.ID+`"}}`))
	var it struct {
		Body struct{ ID string }
	}
	_ = json.Unmarshal([]byte(item), &it)
	if it.Body.ID == "" {
		t.Fatalf("create item = %s", item)
	}

	moved := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"bulk_move_items","arguments":{"workspaceId":"`+wid+`","items":["`+it.Body.ID+`"]}}`))
	if !strings.Contains(moved, `"status":200`) {
		t.Fatalf("bulk move items alias = %s", moved)
	}
	moved = mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"bulk_move_items","arguments":{"workspace_id":"`+wid+`","itemIds":["`+it.Body.ID+`"],"targetFolderId":"`+fol.Body.ID+`"}}`))
	if !strings.Contains(moved, `"status":200`) {
		t.Fatalf("bulk move itemIds alias = %s", moved)
	}

	page := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"search_catalog","arguments":{"search":"lh","filter":"Type eq 'Lakehouse'","page_size":1}}`))
	var cat struct {
		Body struct {
			Value             []any
			ContinuationToken string
		}
	}
	if err := json.Unmarshal([]byte(page), &cat); err != nil {
		t.Fatalf("search page = %s", page)
	}
	if cat.Body.ContinuationToken != "" {
		next := mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"search_catalog","arguments":{"pageSize":1,"continuationToken":"`+cat.Body.ContinuationToken+`"}}`))
		if !strings.Contains(next, `"status":200`) {
			t.Fatalf("search continue = %s", next)
		}
	}
	_ = mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"search_catalog","arguments":{"search":"lh","pageSize":1,"continuation_token":"`+encodePageToken(0)+`"}}`))

	_ = mcpText(t, mcpRPC(t, a, "tools/call", `{"name":"delete_workspace","arguments":{"workspace_id":"`+wid+`"}}`))
}

func TestMCPRouteRequiresBearer(t *testing.T) {
	mux, _, token := newRegisteredAPI(t)
	if w := serve(mux, "POST", "/v1/mcp/core", "", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("no bearer = %d", w.Code)
	}
	w := serve(mux, "POST", "/v1/mcp/core", token,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`)
	if w.Code != http.StatusOK || w.Header().Get("Mcp-Session-Id") == "" {
		t.Fatalf("init = %d %s", w.Code, w.Body.Bytes())
	}
	if w := serve(mux, "GET", "/v1/mcp/core", token, ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d", w.Code)
	}
}
