package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// Fabric's remote Eventhouse MCP server: KQL against one KQL database, as the
// signed-in caller, on the same transport as Core MCP.
//
// Sources, as read on 2026-10-04:
//   - learn.microsoft.com/fabric/real-time-intelligence/mcp-remote-eventhouse —
//     the item-scoped endpoint (…/workspaces/<ws>/items/<id>/kqlEndpoint) and the
//     global one (…/dataPlane/kqlEndpoint, where every call carries workspaceId
//     and itemId), "read or query permissions to the KQL database", and the
//     optional clusterUrl and databaseName every tool takes. It names no tools.
//   - github.com/iemejia/fabio .agents/API-BEHAVIORS-DISCOVERED.md and
//     src/commands/kql_database/mcp.rs — a third party's live initialize and
//     tools/list: serverInfo KustoMCP 1.0.0, exactly four tools (executeQuery,
//     getSchema, getGeneralKQLExamples, getSpecificKQLExamples), the three
//     grounding tools taking referenceText, the item id being the KQL
//     DATABASE's, and "Database is empty" as an isError when it has no tables.
//   - github.com/adindabudi/enterprise-data-analyst-agent
//     apps/api/{src,tests}/…/fabric_auth/eventhouse.py — a second third party's
//     live capture of executeQuery's input schema (kqlQuery and maxRecords
//     required; activityTitle, activityDescription, clusterUrl, databaseName
//     optional), that maxRecords is capped at 1,000 silently, the result as a
//     Kusto document {"Tables":[…]} whose rows are in the table named
//     PrimaryResult, and a failed query's text.
//
// Not captured anywhere, and so this emulator's own: every tool description,
// the schemas of the three grounding tools beyond referenceText, the documents
// getSchema and the example tools return, and the relevance ranking — Fabric's
// grounding is Copilot's, and the emulator has no model, so it ranks by the
// words of referenceText.

// ehMaxRecords is executeQuery's cap: "caps it at 1,000 and says nothing when
// a result held more".
const ehMaxRecords = 1000

// ehSchemaTables bounds how many tables getSchema samples, so a large database
// costs a bounded number of engine round trips.
const ehSchemaTables = 20

const ehDatabaseEmpty = "Database is empty"

type ehScope struct{ workspaceID, itemID string }

func (a *API) registerEventhouseMCP(mux *http.ServeMux) {
	serve := func(scoped bool, run func(*mcpServer) handler) handler {
		return func(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
			scope := ehScope{}
			if scoped {
				scope = ehScope{r.PathValue("workspaceId"), r.PathValue("itemId")}
			}
			run(eventhouseMCP(scope, requestBase(r)))(w, r, p)
		}
	}
	// Written out, not looped: scripts/check_undocumented_routes.py reads each
	// registration's path from its literal.
	mux.HandleFunc("POST /v1/mcp/dataPlane/kqlEndpoint", a.withAuth(serve(false, a.mcpPost)))
	mux.HandleFunc("GET /v1/mcp/dataPlane/kqlEndpoint", a.withAuth(serve(false, a.mcpGet)))
	mux.HandleFunc("DELETE /v1/mcp/dataPlane/kqlEndpoint", a.withAuth(serve(false, a.mcpDelete)))
	const item = "/v1/mcp/dataPlane/workspaces/{workspaceId}/items/{itemId}/kqlEndpoint"
	mux.HandleFunc("POST "+item, a.withAuth(serve(true, a.mcpPost)))
	mux.HandleFunc("GET "+item, a.withAuth(serve(true, a.mcpGet)))
	mux.HandleFunc("DELETE "+item, a.withAuth(serve(true, a.mcpDelete)))
}

// requestBase is the scheme and host the caller reached, from which an
// eventhouse's queryServiceUri is formed (kustoBaseURI).
func requestBase(r *http.Request) string {
	return strings.TrimSuffix(kustoBaseURI(r, "", ""), "/kusto//")
}

func eventhouseMCP(scope ehScope, base string) *mcpServer {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	schema := func(required []any, props map[string]any) map[string]any {
		props["clusterUrl"] = str("Optional: an eventhouse's query URI, to run against it instead")
		props["databaseName"] = str("Optional, with clusterUrl: the KQL database there")
		if scope.itemID == "" {
			// The global endpoint: "provide both workspaceId and itemId in each tool call".
			props["workspaceId"] = str("The workspace's id")
			props["itemId"] = str("The KQL database's item id")
			required = append(required, "workspaceId", "itemId")
		}
		return map[string]any{"type": "object", "properties": props, "required": required}
	}
	grounding := func() map[string]any {
		return schema([]any{"referenceText"}, map[string]any{
			"referenceText": str("The question or KQL the grounding is for"),
		})
	}
	srv := &mcpServer{
		name:    "KustoMCP",
		version: "1.0.0",
		instructions: "Microsoft Fabric Eventhouse MCP over one KQL database: getSchema and the example tools ground " +
			"a KQL query, and executeQuery runs it as you.",
		tools: []mcpToolSpec{
			{Name: "executeQuery", Description: "Run a KQL query against the KQL database and return the result tables.",
				InputSchema: schema([]any{"kqlQuery", "maxRecords"}, map[string]any{
					"kqlQuery":            str("kql query"),
					"maxRecords":          map[string]any{"type": "number"},
					"activityTitle":       map[string]any{"type": "string"},
					"activityDescription": map[string]any{"type": "string"},
				})},
			{Name: "getSchema", Description: "The KQL database's tables, materialized views and functions, most " +
				"relevant to referenceText first, with sample rows and row counts.", InputSchema: grounding()},
			{Name: "getGeneralKQLExamples", Description: "General natural-language-to-KQL example pairs relevant " +
				"to referenceText.", InputSchema: grounding()},
			{Name: "getSpecificKQLExamples", Description: "Example queries curated for this KQL database, " +
				"relevant to referenceText.", InputSchema: grounding()},
		},
	}
	tool := func(fn func(*API, *auth.Principal, *ehTarget, map[string]any) mcpToolResult) func(*API, *auth.Principal, map[string]any) mcpToolResult {
		return func(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
			t, msg := a.ehResolve(scope, base, p, args)
			if msg != "" {
				return mcpErr(msg)
			}
			return fn(a, p, t, args)
		}
	}
	srv.dispatch = map[string]func(*API, *auth.Principal, map[string]any) mcpToolResult{
		"executeQuery":           tool((*API).ehExecuteQuery),
		"getSchema":              tool((*API).ehGetSchema),
		"getGeneralKQLExamples":  tool((*API).ehGeneralExamples),
		"getSpecificKQLExamples": tool((*API).ehSpecificExamples),
	}
	return srv
}

// ehTarget is the KQL database a call runs against, with the names its
// errors show.
type ehTarget struct {
	db         *store.Item
	engineDB   string
	clusterURI string
}

// ehResolve finds the KQL database a call names and checks the caller may
// read it. The Fabric item (itemId) must always be readable — clusterUrl and
// databaseName only redirect where the query runs, as they do in Fabric, where
// the item is then "used only to meter" — and the redirected database must be
// readable too. An item the caller cannot read is "not found".
func (a *API) ehResolve(scope ehScope, base string, p *auth.Principal, args map[string]any) (*ehTarget, string) {
	ws, itemID := arg(args, "workspaceId"), arg(args, "itemId")
	if scope.itemID != "" {
		if (ws != "" && !strings.EqualFold(ws, scope.workspaceID)) || (itemID != "" && !strings.EqualFold(itemID, scope.itemID)) {
			return nil, fmt.Sprintf("this endpoint is bound to KQL database %s in workspace %s; use the global "+
				"endpoint for another", scope.itemID, scope.workspaceID)
		}
		ws, itemID = scope.workspaceID, scope.itemID
	}
	if ws == "" || itemID == "" {
		return nil, "workspaceId and itemId are required: the KQL database's workspace and item id"
	}
	db, msg := a.ehReadableDatabase(p, ws, itemID)
	if msg != "" {
		return nil, msg
	}
	if a.KQLURL == nil {
		return nil, "this emulator has no Kusto engine attached: set FABRIC_KQL_URL (docs/25-rti-kusto.md)"
	}
	cluster, dbName := arg(args, "clusterUrl"), arg(args, "databaseName")
	if (cluster == "") != (dbName == "") {
		return nil, "clusterUrl and databaseName go together: name both, or neither"
	}
	if cluster != "" {
		if db, msg = a.ehRedirect(p, cluster, dbName); msg != "" {
			return nil, msg
		}
	}
	props, _ := a.Store.ItemProperties(db.ID)
	return &ehTarget{db: db, engineDB: engineDatabaseName(db.ID),
		clusterURI: base + "/kusto/" + db.WorkspaceID + "/" + props[propParentEventhouse]}, ""
}

func (a *API) ehReadableDatabase(p *auth.Principal, ws, itemID string) (*store.Item, string) {
	notFound := fmt.Sprintf("KQL database %s was not found in workspace %s, or you cannot read it", itemID, ws)
	it, err := a.Store.GetItemByID(itemID)
	if err != nil || !strings.EqualFold(it.WorkspaceID, ws) {
		return nil, notFound
	}
	if acc, err := a.Store.EffectiveItemAccess(it, p.ID); err != nil || !acc.Has(store.PermRead) {
		return nil, notFound
	}
	switch it.Type {
	case "KQLDatabase":
		return it, ""
	case "Eventhouse":
		return nil, fmt.Sprintf("%s is an Eventhouse; pass one of its KQL databases (properties.databasesItemIds) "+
			"as itemId", it.ID)
	}
	return nil, fmt.Sprintf("%s is a %s; this server runs KQL against a KQL database", it.ID, it.Type)
}

// ehRedirect resolves clusterUrl and databaseName. The emulator hosts no Azure
// Data Explorer cluster, so the cluster must be one of its own eventhouses'
// query URIs; the host is not compared, since a caller may reach the emulator
// by any name.
func (a *API) ehRedirect(p *auth.Principal, cluster, dbName string) (*store.Item, string) {
	u, err := url.Parse(cluster)
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if err != nil || len(parts) != 3 || parts[0] != "kusto" {
		return nil, fmt.Sprintf("clusterUrl %s is not an eventhouse this emulator serves: it hosts no Azure Data "+
			"Explorer cluster, and an eventhouse's query URI ends /kusto/<workspace>/<eventhouse>", cluster)
	}
	db, err := a.resolveKQLDatabase(parts[1], parts[2], dbName)
	if err != nil {
		return nil, fmt.Sprintf("database %s was not found at %s, or you cannot read it", dbName, cluster)
	}
	return a.ehReadableDatabase(p, db.WorkspaceID, db.ID)
}

// ehRun runs csl against the target's engine database: a query (v2) or a
// management command (v1). The engine's database name is mapped back to the
// Fabric display name, as the relay does, so no caller sees the internal name.
func (a *API) ehRun(t *ehTarget, kind, csl string) ([]byte, string) {
	ctx := context.Background()
	if err := a.ensureKustoDatabase(ctx, t.engineDB); err != nil {
		return nil, err.Error()
	}
	ver := "v2"
	if kind == "mgmt" {
		ver = "v1"
	}
	status, payload, err := a.callKusto(ctx, ver, kind, kustoRequest{DB: t.engineDB, CSL: csl})
	if err != nil {
		return nil, err.Error()
	}
	payload = bytes.ReplaceAll(payload, []byte(t.engineDB), []byte(t.db.DisplayName))
	if status >= 300 {
		return nil, a.ehFailure(t, payload)
	}
	if ver == "v2" {
		if msg := v2Failure(payload); msg != "" {
			return nil, a.ehFailure(t, []byte(msg))
		}
	}
	return payload, ""
}

// ehFailure is a failed query in the captured shape: Fabric's prefix naming
// the cluster and database, then the engine's own category and error body.
func (a *API) ehFailure(t *ehTarget, body []byte) string {
	var env struct {
		Error struct {
			Message string `json:"@message"`
		} `json:"error"`
	}
	category := "Error"
	if json.Unmarshal(body, &env) == nil && env.Error.Message != "" {
		// "Semantic error: …" gives "Semantic error", as captured; a message
		// with no category of its own is its own.
		category, _, _ = strings.Cut(env.Error.Message, ":")
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, body, "", "    ") != nil {
		pretty.Reset()
		pretty.Write(body)
	}
	return fmt.Sprintf("Error in executing KQL query. cluster='%s', database='%s', Exception='%s: %s'",
		t.clusterURI, t.db.DisplayName, category, pretty.String())
}

// v2Frame is one frame of a Kusto v2 response.
type v2Frame struct {
	FrameType string `json:"FrameType"`
	TableKind string `json:"TableKind"`
	TableName string `json:"TableName"`
	Columns   []struct {
		ColumnName string `json:"ColumnName"`
		ColumnType string `json:"ColumnType"`
	} `json:"Columns"`
	Rows      []json.RawMessage `json:"Rows"`
	HasErrors bool              `json:"HasErrors"`
	Errors    []json.RawMessage `json:"OneApiErrors"`
}

// v2Failure is a query that failed after the engine answered 200: a
// DataSetCompletion with HasErrors, as the error body it carries.
func v2Failure(payload []byte) string {
	var frames []v2Frame
	if json.Unmarshal(payload, &frames) != nil {
		return ""
	}
	for _, f := range frames {
		if f.FrameType == "DataSetCompletion" && f.HasErrors && len(f.Errors) > 0 {
			return string(f.Errors[0])
		}
	}
	return ""
}

// v2Tables is a v2 response as the Kusto document the tool answers with: each
// data table named by its kind (QueryProperties, PrimaryResult,
// QueryCompletionInformation), the primary result cut to max rows.
func v2Tables(payload []byte, max int) ([]map[string]any, error) {
	var frames []v2Frame
	if err := json.Unmarshal(payload, &frames); err != nil {
		return nil, fmt.Errorf("the engine's response could not be read: %w", err)
	}
	var tables []map[string]any
	for _, f := range frames {
		if f.FrameType != "DataTable" {
			continue
		}
		cols := make([]map[string]string, len(f.Columns))
		for i, c := range f.Columns {
			cols[i] = map[string]string{"ColumnName": c.ColumnName, "ColumnType": c.ColumnType}
		}
		rows := f.Rows
		if f.TableKind == "PrimaryResult" && len(rows) > max {
			rows = rows[:max] // silently, as captured
		}
		if rows == nil {
			rows = []json.RawMessage{}
		}
		tables = append(tables, map[string]any{"TableName": f.TableKind, "Columns": cols, "Rows": rows})
	}
	return tables, nil
}

func (a *API) ehExecuteQuery(_ *auth.Principal, t *ehTarget, args map[string]any) mcpToolResult {
	query := arg(args, "kqlQuery")
	if strings.TrimSpace(query) == "" {
		return mcpErr("kqlQuery is required")
	}
	n, ok := argInt(args, "maxRecords", 0)
	if !ok || n < 1 {
		return mcpErr("maxRecords is required: a whole number of records, at least 1 (at most 1,000 are returned)")
	}
	payload, msg := a.ehRun(t, "query", query)
	if msg != "" {
		return mcpErr(msg)
	}
	tables, err := v2Tables(payload, min(n, ehMaxRecords))
	if err != nil {
		return mcpErr(err.Error())
	}
	return mcpJSON(map[string]any{"Tables": tables})
}

// ehSchema is the database schema the engine reports, as `.show database
// schema as json` returns it.
type ehSchema struct {
	Tables            map[string]ehTable `json:"Tables"`
	MaterializedViews map[string]ehTable `json:"MaterializedViews"`
	Functions         map[string]struct {
		Name            string `json:"Name"`
		InputParameters []struct {
			Name    string `json:"Name"`
			CslType string `json:"CslType"`
		} `json:"InputParameters"`
		Body      string `json:"Body"`
		DocString string `json:"DocString"`
		Folder    string `json:"Folder"`
	} `json:"Functions"`
}

type ehTable struct {
	Name           string `json:"Name"`
	Folder         string `json:"Folder"`
	DocString      string `json:"DocString"`
	OrderedColumns []struct {
		Name    string `json:"Name"`
		CslType string `json:"CslType"`
	} `json:"OrderedColumns"`
}

// ehLoadSchema reads the database's schema, or refuses with the captured
// "Database is empty" when it has no tables: the grounding tools ground on
// tables and their data.
func (a *API) ehLoadSchema(t *ehTarget) (*ehSchema, string) {
	payload, msg := a.ehRun(t, "mgmt", ".show database schema as json")
	if msg != "" {
		return nil, msg
	}
	var v1 struct {
		Tables []struct {
			Rows [][]any `json:"Rows"`
		} `json:"Tables"`
	}
	if json.Unmarshal(payload, &v1) != nil || len(v1.Tables) == 0 || len(v1.Tables[0].Rows) == 0 || len(v1.Tables[0].Rows[0]) == 0 {
		return nil, "the engine's schema could not be read"
	}
	text, _ := v1.Tables[0].Rows[0][0].(string)
	var doc struct {
		Databases map[string]ehSchema `json:"Databases"`
	}
	if json.Unmarshal([]byte(text), &doc) != nil {
		return nil, "the engine's schema could not be read"
	}
	for _, s := range doc.Databases {
		if len(s.Tables) == 0 {
			return nil, ehDatabaseEmpty
		}
		return &s, ""
	}
	return nil, ehDatabaseEmpty
}

// relevance scores a name and its columns against the words of referenceText:
// the emulator has no model, so it ranks by shared words.
func relevance(reference string, names ...string) int {
	words := strings.FieldsFunc(strings.ToLower(reference), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_'
	})
	score := 0
	for _, n := range names {
		n = strings.ToLower(n)
		for _, w := range words {
			if len(w) > 2 && strings.Contains(n, w) {
				score++
			}
		}
	}
	return score
}

func (a *API) ehGetSchema(_ *auth.Principal, t *ehTarget, args map[string]any) mcpToolResult {
	schema, msg := a.ehLoadSchema(t)
	if msg != "" {
		return mcpErr(msg)
	}
	ref := arg(args, "referenceText")
	type ranked struct {
		table ehTable
		score int
	}
	var tables []ranked
	for _, tb := range schema.Tables {
		names := []string{tb.Name, tb.DocString}
		for _, c := range tb.OrderedColumns {
			names = append(names, c.Name)
		}
		tables = append(tables, ranked{tb, relevance(ref, names...)})
	}
	sort.Slice(tables, func(i, j int) bool {
		if tables[i].score != tables[j].score {
			return tables[i].score > tables[j].score
		}
		return tables[i].table.Name < tables[j].table.Name
	})
	var out []map[string]any
	for i, r := range tables {
		cols := make([]map[string]string, len(r.table.OrderedColumns))
		for k, c := range r.table.OrderedColumns {
			cols[k] = map[string]string{"Name": c.Name, "Type": c.CslType}
		}
		entry := map[string]any{"Name": r.table.Name, "Folder": r.table.Folder, "DocString": r.table.DocString,
			"Columns": cols}
		// Samples and counts come from the data, as Fabric's do; past the
		// first tables only the schema is listed.
		if i < ehSchemaTables {
			if samples, count, msg := a.ehSample(t, r.table.Name); msg == "" {
				entry["RowCount"], entry["SampleRows"] = count, samples
			}
		}
		out = append(out, entry)
	}
	views := []string{}
	for name := range schema.MaterializedViews {
		views = append(views, name)
	}
	sort.Strings(views)
	var funcs []map[string]any
	for _, f := range schema.Functions {
		params := make([]string, len(f.InputParameters))
		for i, p := range f.InputParameters {
			params[i] = p.Name + ":" + p.CslType
		}
		funcs = append(funcs, map[string]any{"Name": f.Name, "Parameters": params, "Body": f.Body, "DocString": f.DocString})
	}
	sort.Slice(funcs, func(i, j int) bool { return funcs[i]["Name"].(string) < funcs[j]["Name"].(string) })
	return mcpJSON(map[string]any{
		"Database": t.db.DisplayName, "Tables": out, "MaterializedViews": views, "Functions": funcs,
		"Guidance": "Quote a table or column whose name is a KQL keyword as ['name']. Filter with where before " +
			"summarize, and bound a result with take or top.",
	})
}

// ehSample is up to five rows of a table and its row count.
func (a *API) ehSample(t *ehTarget, table string) ([]map[string]any, int64, string) {
	quoted := kustoQuoteIdent(table)
	payload, msg := a.ehRun(t, "query", quoted+" | take 5")
	if msg != "" {
		return nil, 0, msg
	}
	tables, _ := v2Tables(payload, 5)
	var samples []map[string]any
	for _, tb := range tables {
		if tb["TableName"] != "PrimaryResult" {
			continue
		}
		cols := tb["Columns"].([]map[string]string)
		for _, raw := range tb["Rows"].([]json.RawMessage) {
			var row []any
			if json.Unmarshal(raw, &row) != nil {
				continue
			}
			rec := map[string]any{}
			for i, v := range row {
				if i < len(cols) {
					rec[cols[i]["ColumnName"]] = v
				}
			}
			samples = append(samples, rec)
		}
	}
	payload, msg = a.ehRun(t, "query", quoted+" | count")
	if msg != "" {
		return nil, 0, msg
	}
	tables, _ = v2Tables(payload, 1)
	var count int64
	for _, tb := range tables {
		if tb["TableName"] != "PrimaryResult" || len(tb["Rows"].([]json.RawMessage)) == 0 {
			continue
		}
		var row []int64
		if json.Unmarshal(tb["Rows"].([]json.RawMessage)[0], &row) == nil && len(row) == 1 {
			count = row[0]
		}
	}
	return samples, count, ""
}

// kqlExample is one natural-language question and the KQL that answers it.
type kqlExample struct{ Question, Query string }

// generalKQLExamples are the emulator's own general examples. Fabric's are a
// curated public set this emulator does not have; these show the same kinds
// of query, over a table named T, and are chosen by the words they share with
// referenceText.
var generalKQLExamples = []kqlExample{
	{"How many rows does the table have?", "T | count"},
	{"Show a few rows to see what the data looks like", "T | take 10"},
	{"Count rows per category, largest first", "T | summarize Count = count() by Category | order by Count desc"},
	{"What are the top 5 values by amount?", "T | top 5 by Amount desc"},
	{"How many events happened per hour over the last day?",
		"T | where Timestamp > ago(1d) | summarize Events = count() by bin(Timestamp, 1h)"},
	{"Filter rows where a text column contains a word", "T | where Message has 'error'"},
	{"What is the average, minimum and maximum of a value per group?",
		"T | summarize avg(Value), min(Value), max(Value) by Group"},
	{"How many distinct users are there?", "T | summarize dcount(UserId)"},
	{"Join two tables on a shared key", "T | join kind=inner (U) on Id"},
	{"Which rows are newest?", "T | order by Timestamp desc | take 20"},
}

func (a *API) ehGeneralExamples(_ *auth.Principal, t *ehTarget, args map[string]any) mcpToolResult {
	if _, msg := a.ehLoadSchema(t); msg != "" {
		return mcpErr(msg)
	}
	ref := arg(args, "referenceText")
	picked := append([]kqlExample(nil), generalKQLExamples...)
	sort.SliceStable(picked, func(i, j int) bool {
		return relevance(ref, picked[i].Question) > relevance(ref, picked[j].Question)
	})
	var b strings.Builder
	b.WriteString("# General KQL examples\n")
	for _, e := range picked[:5] {
		fmt.Fprintf(&b, "\n**%s**\n```kql\n%s\n```\n", e.Question, e.Query)
	}
	return mcpToolResult{Content: []mcpContent{{Type: "text", Text: b.String()}}}
}

// ehSpecificExamples: Fabric's are curated or learned from this database's own
// queries, and are empty on a fresh one. The emulator learns nothing, so a
// database with tables always has none.
func (a *API) ehSpecificExamples(_ *auth.Principal, t *ehTarget, _ map[string]any) mcpToolResult {
	if _, msg := a.ehLoadSchema(t); msg != "" {
		return mcpErr(msg)
	}
	return mcpToolResult{Content: []mcpContent{{Type: "text",
		Text: "No examples have been curated for database " + t.db.DisplayName + " yet."}}}
}
