package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// Fabric Core MCP Server (preview): Streamable HTTP at POST /v1/mcp/core.
// Tools wrap the existing REST handlers so RBAC, audit and LRO stay one path.
// Reference: learn.microsoft.com rest/api/fabric/articles/mcp-servers/core-remote.

func (a *API) registerMCP(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/mcp/core", a.withAuth(a.handleMCPPost))
	mux.HandleFunc("GET /v1/mcp/core", a.withAuth(a.handleMCPGet))
	mux.HandleFunc("DELETE /v1/mcp/core", a.withAuth(a.handleMCPDelete))
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (a *API) handleMCPGet(w http.ResponseWriter, _ *http.Request, _ *auth.Principal) {
	// No server-initiated messages. A host that opens GET SSE is told to
	// stick to request/response POSTs, which is enough for the published tools.
	w.Header().Set("Allow", "POST, DELETE")
	w.WriteHeader(http.StatusMethodNotAllowed)
}

func (a *API) handleMCPDelete(w http.ResponseWriter, _ *http.Request, _ *auth.Principal) {
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) handleMCPPost(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
		return
	}
	if req.JSONRPC == "" {
		req.JSONRPC = "2.0"
	}
	// Notifications have no id — acknowledge by empty 202, no body.
	if len(req.ID) == 0 || string(req.ID) == "null" {
		if req.Method == "notifications/initialized" || req.Method == "notifications/cancelled" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = a.mcpInitialize(req.Params)
		w.Header().Set("Mcp-Session-Id", store.NewID())
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": mcpTools}
	case "tools/call":
		resp.Result = a.mcpCall(req.Params, p)
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	writeRPC(w, resp)
}

func (a *API) mcpInitialize(params json.RawMessage) map[string]any {
	version := "2025-03-26"
	var in struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &in)
	switch in.ProtocolVersion {
	case "2024-11-05", "2025-03-26", "2025-06-18":
		version = in.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "fabric-core", "version": "preview"},
		"instructions":    "Microsoft Fabric Core MCP Server. Tools map to Fabric REST. This endpoint does not execute notebooks or write lakehouse tables.",
	}
}

type mcpCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type mcpToolResult struct {
	Content []mcpContent `json:"content"`
	IsError bool         `json:"isError,omitempty"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (a *API) mcpCall(params json.RawMessage, p *auth.Principal) mcpToolResult {
	var call mcpCallParams
	if err := json.Unmarshal(params, &call); err != nil {
		return mcpErr("invalid tools/call params")
	}
	if call.Arguments == nil {
		call.Arguments = map[string]any{}
	}
	fn, ok := mcpDispatch[call.Name]
	if !ok {
		return mcpErr("unknown tool: " + call.Name)
	}
	return fn(a, p, call.Arguments)
}

func mcpErr(msg string) mcpToolResult {
	return mcpToolResult{
		Content: []mcpContent{{Type: "text", Text: msg}},
		IsError: true,
	}
}

func mcpJSON(v any) mcpToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		return mcpErr(err.Error())
	}
	return mcpToolResult{Content: []mcpContent{{Type: "text", Text: string(b)}}}
}

func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// invoke runs an existing REST handler against a synthetic request so MCP
// tools cannot bypass RBAC, audit, or LRO.
func (a *API) invoke(h handler, p *auth.Principal, method, body string, pathVals map[string]string, query url.Values) *httptest.ResponseRecorder {
	path := "/x"
	if query != nil {
		path += "?" + query.Encode()
	}
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
		r.ContentLength = int64(len(body))
	}
	for k, v := range pathVals {
		r.SetPathValue(k, v)
	}
	w := httptest.NewRecorder()
	h(w, r, p)
	return w
}

func mcpFromHTTP(rec *httptest.ResponseRecorder) mcpToolResult {
	payload := map[string]any{
		"status": rec.Code,
		"body":   json.RawMessage(bytesOrEmpty(rec.Body.Bytes())),
	}
	if id := rec.Header().Get("x-ms-operation-id"); id != "" {
		payload["operationId"] = id
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		payload["location"] = loc
	}
	out := mcpJSON(payload)
	if rec.Code >= 400 {
		out.IsError = true
	}
	return out
}

func bytesOrEmpty(b []byte) []byte {
	if len(b) == 0 {
		return []byte("null")
	}
	return b
}

func arg(args map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := args[k]; ok && v != nil {
			switch t := v.(type) {
			case string:
				if t != "" {
					return t
				}
			}
		}
	}
	return ""
}

func argJSON(args map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := args[k]; ok && v != nil {
			b, err := json.Marshal(v)
			if err == nil {
				return string(b)
			}
		}
	}
	return ""
}
