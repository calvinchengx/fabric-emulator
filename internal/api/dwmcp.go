package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// Fabric Data Warehouse MCP server: a remote MCP server that runs T-SQL on a
// Warehouse or a lakehouse's SQL analytics endpoint as the signed-in caller,
// on the same transport as Core MCP.
//
// Sources, as read on 2026-10-04:
//   - learn.microsoft.com/fabric/data-warehouse/data-warehouse-mcp-server — the
//     two endpoints (global, and item-scoped to one warehouse), "uses the
//     signed-in user's identity and respects Fabric permissions", and that the
//     server exposes one tool and no separate schema-discovery tools. It names
//     that tool `executeSQL`.
//   - github.com/microsoft/skills-for-fabric skills/sqldw-cli — the tool as
//     Microsoft's own skills call it and allow-list it in their .mcp.json:
//     execute_query(workspaceId, itemId, query), one T-SQL batch per call (no GO,
//     no sqlcmd meta-commands), only the last result set returned, as "CSV
//     results (RFC 4180) + metadata text"; itemId is a Warehouse id or a
//     lakehouse's SQL analytics endpoint id, "not the lakehouse item id". It
//     records 10,000 rows, 300 seconds and 20 requests a minute as "observed
//     defaults, not a documented contract".
//
//   - github.com/iemejia/fabio .agents/API-BEHAVIORS-DISCOVERED.md, "Fabric
//     Data Warehouse MCP Server" and "execute_query result shape" — a third
//     party's live capture: serverInfo microsoft.fabric.sqlEndpoint 0.1.0, one
//     tool titled "Execute T-SQL Query" requiring workspaceId, itemId and
//     query, its annotations, the CSV as an embedded text/csv resource then
//     "Query returned N rows.", and a SQL error as isError text
//     "Error -32002: <message>".
//
// The Learn page names the tool executeSQL; the live server and the skills say
// execute_query, so tools/list publishes that, and executeSQL is accepted as
// the same tool for a client following the Learn page. Not captured anywhere,
// and so this emulator's own: the resource URI between fabric:// and
// /query-results/, and the text for a batch that returns no result set.

// dwMaxRows is the observed result cap. The skills say exactly 10,000 rows
// means the result was truncated: the server does not say so itself.
const dwMaxRows = 10000

const (
	dwTool      = "execute_query"
	dwToolAlias = "executeSQL" // the Learn page's name for the same tool
)

// SQLBatchResult is the last result set of one T-SQL batch: column names and
// engine type names, the rows read, and whether more rows were left unread.
// No columns means the batch returned no result set.
type SQLBatchResult struct {
	Columns   []string
	Types     []string
	Rows      [][]any
	Truncated bool
}

// dwScope is the item an item-scoped endpoint is bound to; zero for the global one.
type dwScope struct{ workspaceID, itemID string }

func (a *API) registerDataWarehouseMCP(mux *http.ServeMux) {
	global := dataWarehouseMCP(dwScope{})
	mux.HandleFunc("POST /v1/mcp/dataPlane/sqlEndpoint", a.withAuth(a.mcpPost(global)))
	mux.HandleFunc("GET /v1/mcp/dataPlane/sqlEndpoint", a.withAuth(a.mcpGet(global)))
	mux.HandleFunc("DELETE /v1/mcp/dataPlane/sqlEndpoint", a.withAuth(a.mcpDelete(global)))
	scoped := func(serve func(*mcpServer) handler) handler {
		return func(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
			serve(dataWarehouseMCP(dwScope{r.PathValue("workspaceId"), r.PathValue("itemId")}))(w, r, p)
		}
	}
	const item = "/v1/mcp/dataPlane/workspaces/{workspaceId}/items/{itemId}/sqlEndpoint"
	mux.HandleFunc("POST "+item, a.withAuth(scoped(a.mcpPost)))
	mux.HandleFunc("GET "+item, a.withAuth(scoped(a.mcpGet)))
	mux.HandleFunc("DELETE "+item, a.withAuth(scoped(a.mcpDelete)))
}

// dataWarehouseMCP is the server for one endpoint. The item-scoped endpoint
// takes its warehouse from the URL, so its tool does not ask for one.
func dataWarehouseMCP(scope dwScope) *mcpServer {
	props := map[string]any{
		"workspaceId": map[string]any{"type": "string", "description": "The workspace's id"},
		"itemId": map[string]any{"type": "string", "description": "A Warehouse's id, or a lakehouse's SQL analytics " +
			"endpoint id (properties.sqlEndpointProperties.id), not the lakehouse's own id"},
		"query": map[string]any{"type": "string", "description": "One T-SQL batch: no GO separators, no sqlcmd commands"},
	}
	// The captured schema requires all three. The item-scoped endpoint takes
	// its item from the URL, so it also accepts a call that omits them, and
	// refuses one that names another item.
	required := []any{"workspaceId", "itemId", "query"}
	about := "Microsoft Fabric Data Warehouse MCP. One tool, execute_query, runs a T-SQL batch on a Warehouse or a " +
		"SQL analytics endpoint as you, and returns its last result set as CSV."
	if scope.itemID != "" {
		about += " This endpoint is bound to item " + scope.itemID + " in workspace " + scope.workspaceID + "."
	}
	run := func(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
		return a.toolDWExecuteQuery(scope, p, args)
	}
	return &mcpServer{
		name:         "microsoft.fabric.sqlEndpoint",
		version:      "0.1.0",
		description:  "Fabric SQL Endpoint – executes T-SQL queries",
		instructions: about + " Use INFORMATION_SCHEMA and the catalog views to discover tables and columns.",
		tools: []mcpToolSpec{{
			Name:  dwTool,
			Title: "Execute T-SQL Query",
			Description: "Execute one T-SQL batch against a Fabric Warehouse or SQL analytics endpoint and return the " +
				"last result set as CSV (RFC 4180), then a row count. At most 10,000 rows are returned.",
			InputSchema: map[string]any{"type": "object", "properties": props, "required": required},
			// As captured: writes run where the caller may write, so the client
			// should ask before each call, as Microsoft's page says.
			Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": true,
				"idempotentHint": true, "openWorldHint": false},
		}},
		dispatch: map[string]func(*API, *auth.Principal, map[string]any) mcpToolResult{dwTool: run, dwToolAlias: run},
	}
}

func (a *API) toolDWExecuteQuery(scope dwScope, p *auth.Principal, args map[string]any) mcpToolResult {
	ws, itemID, query := arg(args, "workspaceId"), arg(args, "itemId"), arg(args, "query")
	if scope.itemID != "" {
		if (ws != "" && !strings.EqualFold(ws, scope.workspaceID)) || (itemID != "" && !strings.EqualFold(itemID, scope.itemID)) {
			return mcpErr(fmt.Sprintf("this endpoint is bound to item %s in workspace %s; use the global endpoint "+
				"for another item", scope.itemID, scope.workspaceID))
		}
		ws, itemID = scope.workspaceID, scope.itemID
	}
	switch {
	case ws == "" || itemID == "":
		return mcpErr("workspaceId and itemId are required: the Warehouse's id, or a lakehouse's SQL analytics endpoint id")
	case strings.TrimSpace(query) == "":
		return mcpErr("query is required: one T-SQL batch")
	}
	target, msg := a.dwTarget(p.ID, ws, itemID)
	if msg != "" {
		return mcpErr(msg)
	}
	if a.SQLExecAs == nil {
		return mcpErr("this emulator serves no SQL engine: set WAREHOUSE_MSSQL_DSN to run T-SQL (docs/04-configuration.md)")
	}
	res, err := a.SQLExecAs(context.Background(), target, p.ID, query, dwMaxRows)
	if err != nil {
		// The captured shape of a failed batch.
		return mcpErr("Error -32002: " + err.Error())
	}
	return dwResult(res, "fabric://workspaces/"+ws+"/items/"+itemID+"/query-results/"+store.NewID()+".csv")
}

// dwTarget is the SQL item a call runs on, or why it cannot run. A SQL
// analytics endpoint is reached through its lakehouse, whose database it is;
// the lakehouse's own id is refused, as the skills warn it is in Fabric. An
// item the caller cannot read is "not found", so its type is not disclosed.
func (a *API) dwTarget(principal, ws, itemID string) (string, string) {
	notFound := fmt.Sprintf("item %s was not found in workspace %s, or you cannot read it", itemID, ws)
	it, err := a.Store.GetItemByID(itemID)
	if err != nil || !strings.EqualFold(it.WorkspaceID, ws) {
		return "", notFound
	}
	// Access to an endpoint is access to its lakehouse: "sharing a lakehouse
	// also grants access to the SQL analytics endpoint".
	target := it
	if it.Type == "SQLEndpoint" {
		props, _ := a.Store.ItemProperties(it.ID)
		if target, err = a.Store.GetItemByID(props[store.PropParentLakehouse]); err != nil {
			return "", fmt.Sprintf("SQL analytics endpoint %s has no lakehouse", it.ID)
		}
	}
	if acc, err := a.Store.EffectiveItemAccess(target, principal); err != nil || !acc.Has(store.PermRead) {
		return "", notFound
	}
	switch it.Type {
	case "Warehouse", "SQLEndpoint":
		return target.ID, ""
	case "Lakehouse":
		return "", fmt.Sprintf("%s is a Lakehouse; pass its SQL analytics endpoint id "+
			"(properties.sqlEndpointProperties.id) as itemId", it.ID)
	}
	return "", fmt.Sprintf("%s is a %s; execute_query runs on a Warehouse or a SQL analytics endpoint", it.ID, it.Type)
}

// dwResult is the tool's answer, as captured: the result set as an embedded
// text/csv resource (RFC 4180, header first), then "Query returned N rows.".
func dwResult(res *SQLBatchResult, uri string) mcpToolResult {
	if len(res.Columns) == 0 {
		return mcpToolResult{Content: []mcpContent{{Type: "text",
			Text: "Query executed successfully. It returned no result set."}}}
	}
	var buf strings.Builder
	csvRecord(&buf, res.Columns)
	for _, row := range res.Rows {
		rec := make([]string, len(row))
		for i, v := range row {
			rec[i] = dwCell(v, res.Types[i])
		}
		csvRecord(&buf, rec)
	}
	return mcpToolResult{Content: []mcpContent{
		{Type: "resource", Resource: &mcpResource{URI: uri, MimeType: "text/csv", Text: buf.String()}},
		{Type: "text", Text: fmt.Sprintf("Query returned %d rows.", len(res.Rows))},
	}}
}

// csvRecord appends one RFC 4180 record, ended by CRLF. A field holding a
// comma, a quote or a line break is quoted, with quotes doubled; its contents
// are otherwise written byte for byte. encoding/csv's UseCRLF is not used
// because it also rewrites a line break INSIDE a quoted field, so a stored
// "a\nb" would come back as "a\r\nb": the data, changed.
func csvRecord(b *strings.Builder, fields []string) {
	for i, f := range fields {
		if i > 0 {
			b.WriteByte(',')
		}
		if strings.ContainsAny(f, ",\"\r\n") {
			b.WriteByte('"')
			b.WriteString(strings.ReplaceAll(f, `"`, `""`))
			b.WriteByte('"')
			continue
		}
		b.WriteString(f)
	}
	b.WriteString("\r\n")
}

// dwCell prints one value. NULL is an empty field; a bit is 1 or 0; a date or
// time keeps the precision of its type, in SQL Server's literal forms.
func dwCell(v any, typ string) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "1"
		}
		return "0"
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case time.Time:
		switch strings.ToUpper(typ) {
		case "DATE":
			return x.Format("2006-01-02")
		case "TIME":
			return x.Format("15:04:05.9999999")
		case "DATETIMEOFFSET":
			return x.Format("2006-01-02 15:04:05.9999999 -07:00")
		}
		return x.Format("2006-01-02 15:04:05.9999999")
	}
	return fmt.Sprint(v)
}
