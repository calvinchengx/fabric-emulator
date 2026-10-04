package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// Fabric's remote Eventhouse MCP server against a scripted Kusto engine: what
// the server owns is the endpoints, the four tools, which database a call runs
// on and who may name it, and the documents it answers with. That an
// unmodified MCP client reaches a real engine (kustainer) is witnessed by
// e2e/mcp-eventhouse in CI.

// v2Engine answers like Kusto: v1 management commands, v2 query frames.
type v2Engine struct {
	mu        sync.Mutex
	databases map[string]bool
	calls     []string            // "kind db csl"
	tables    map[string][]string // table -> columns; empty means "Database is empty"
	rows      int                 // rows a plain query returns
	// schemaRaw, when set, is the whole answer to .show database schema.
	schemaRaw string
}

func (e *v2Engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body kustoRequest
	_ = json.Unmarshal(raw, &body)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, r.URL.Path+" "+body.DB+" "+body.CSL)
	w.Header().Set("Content-Type", "application/json")
	v1 := func(cols []string, rows [][]any) {
		c := make([]map[string]string, len(cols))
		for i, n := range cols {
			c[i] = map[string]string{"ColumnName": n, "DataType": "String"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Tables": []any{map[string]any{"TableName": "Table_0", "Columns": c, "Rows": rows}}})
	}
	switch {
	case body.CSL == ".show databases":
		var rows [][]any
		for n := range e.databases {
			rows = append(rows, []any{n})
		}
		v1([]string{"DatabaseName"}, rows)
	case strings.HasPrefix(body.CSL, ".create database "):
		e.databases[strings.Fields(body.CSL)[2]] = true
		v1([]string{"DatabaseName"}, nil)
	case body.CSL == ".show database schema as json" && e.schemaRaw != "":
		_, _ = w.Write([]byte(e.schemaRaw))
	case body.CSL == ".show database schema as json":
		tables := map[string]any{}
		for name, cols := range e.tables {
			oc := []map[string]string{}
			for _, c := range cols {
				oc = append(oc, map[string]string{"Name": c, "CslType": "string"})
			}
			tables[name] = map[string]any{"Name": name, "Folder": "", "DocString": "about " + name, "OrderedColumns": oc}
		}
		doc, _ := json.Marshal(map[string]any{"Databases": map[string]any{body.DB: map[string]any{
			"Tables":            tables,
			"MaterializedViews": map[string]any{"DailyCounts": map[string]any{"Name": "DailyCounts"}},
			"Functions": map[string]any{"Recent": map[string]any{"Name": "Recent", "Body": "{ T | take 1 }",
				"InputParameters": []map[string]string{{"Name": "n", "CslType": "long"}}},
				"Archive": map[string]any{"Name": "Archive", "Body": "{ T }"}},
		}}})
		v1([]string{"DatabaseSchema"}, [][]any{{string(doc)}})
	case body.CSL == "Broken | take 1":
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"error":{"code":"General_BadRequest","@message":"Semantic error: 'take' operator: Failed to resolve table or column expression named 'Broken' in %s"}}`, body.DB)
	case body.CSL == "Partial":
		_, _ = w.Write([]byte(`[{"FrameType":"DataSetHeader"},{"FrameType":"DataSetCompletion","HasErrors":true,` +
			`"OneApiErrors":[{"error":{"code":"LimitsExceeded","@message":"Query execution has exceeded the allowed limits"}}]}]`))
	case body.CSL == "Hangup":
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
	case body.CSL == "NotJSON" || strings.HasPrefix(body.CSL, "['FailSample'] | take") || strings.HasPrefix(body.CSL, "['FailCount'] | count"):
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("the engine said no"))
	case strings.HasPrefix(body.CSL, "['BadRow'] | take"):
		_, _ = w.Write([]byte(`[{"FrameType":"DataTable","TableKind":"PrimaryResult","Columns":[{"ColumnName":"c"}],"Rows":["not a row",["ok"]]}]`))
	case body.CSL == "NullRows":
		_, _ = w.Write([]byte(`[{"FrameType":"DataTable","TableKind":"PrimaryResult","Columns":[{"ColumnName":"c"}],"Rows":null}]`))
	case body.CSL == "Garbled":
		_, _ = w.Write([]byte(`{"not":"frames"}`))
	default:
		rows := [][]any{}
		switch {
		case strings.HasSuffix(body.CSL, "| count"):
			rows = append(rows, []any{int64(42)})
		case strings.HasSuffix(body.CSL, "| take 5"):
			rows = append(rows, []any{"a", "b"}, []any{"c"})
		default:
			for i := 0; i < e.rows; i++ {
				rows = append(rows, []any{body.DB, i})
			}
		}
		frames := []any{
			map[string]any{"FrameType": "DataSetHeader", "IsProgressive": false},
			map[string]any{"FrameType": "DataTable", "TableKind": "QueryProperties", "TableName": "@ExtendedProperties",
				"Columns": []map[string]string{{"ColumnName": "Key", "ColumnType": "string"}}, "Rows": [][]any{{"Visualization"}}},
			map[string]any{"FrameType": "DataTable", "TableKind": "PrimaryResult", "TableName": "PrimaryResult",
				"Columns": []map[string]string{{"ColumnName": "Db", "ColumnType": "string"}, {"ColumnName": "N", "ColumnType": "long"}}, "Rows": rows},
			map[string]any{"FrameType": "DataTable", "TableKind": "QueryCompletionInformation", "TableName": "QueryCompletionInformation",
				"Columns": []map[string]string{{"ColumnName": "Level", "ColumnType": "int"}}, "Rows": [][]any{}},
			map[string]any{"FrameType": "DataSetCompletion", "HasErrors": false, "Cancelled": false},
		}
		_ = json.NewEncoder(w).Encode(frames)
	}
}

type ehFixture struct {
	*dwFixture
	engine *v2Engine
	eh     *store.Item
	db     *store.Item
	other  *store.Item // a second database in the same eventhouse
}

func newEH(t *testing.T) *ehFixture {
	t.Helper()
	f := &ehFixture{dwFixture: newDW(t), engine: &v2Engine{databases: map[string]bool{},
		tables: map[string][]string{"StormEvents": {"State", "EventType"}, "Users": {"UserId"}}, rows: 3}}
	srv := httptest.NewServer(f.engine)
	t.Cleanup(srv.Close)
	if err := f.a.SetKQLBackend(srv.URL); err != nil {
		t.Fatal(err)
	}
	f.eh = f.item("Eventhouse", "Telemetry")
	f.db = f.kqlDatabase("Telemetry")
	f.other = f.kqlDatabase("Archive")
	return f
}

func (f *ehFixture) kqlDatabase(name string) *store.Item {
	db := f.item("KQLDatabase", name)
	if err := f.st.SetItemProperties(db.ID, map[string]string{propParentEventhouse: f.eh.ID}); err != nil {
		f.t.Fatal(err)
	}
	return db
}

const ehGlobal = "/v1/mcp/dataPlane/kqlEndpoint"

func (f *ehFixture) scopedKQL(it *store.Item) string {
	return "/v1/mcp/dataPlane/workspaces/" + it.WorkspaceID + "/items/" + it.ID + "/kqlEndpoint"
}

// doc runs a tool that must succeed and decodes its JSON text.
func (f *ehFixture) doc(path, tool string, args map[string]any) map[string]any {
	f.t.Helper()
	texts, isErr := f.call(path, tool, args)
	if isErr {
		f.t.Fatalf("%s: %q", tool, texts)
	}
	var d map[string]any
	if err := json.Unmarshal([]byte(texts[0]), &d); err != nil {
		f.t.Fatalf("%s: %q", tool, texts)
	}
	return d
}

func TestEventhouseMCPServesTheFourCapturedToolsAtBothEndpoints(t *testing.T) {
	f := newEH(t)
	for path, extra := range map[string][]string{ehGlobal: {"workspaceId", "itemId"}, f.scopedKQL(f.db): nil} {
		init := f.rpc(path, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
		if dig(init, "result", "serverInfo", "name") != "KustoMCP" || dig(init, "result", "serverInfo", "version") != "1.0.0" {
			t.Errorf("%s: serverInfo %v", path, dig(init, "result", "serverInfo"))
		}
		tools := dig(f.rpc(path, "tools/list", nil), "result", "tools").([]any)
		var names []string
		for _, tl := range tools {
			names = append(names, dig(tl, "name").(string))
		}
		if strings.Join(names, ",") != "executeQuery,getSchema,getGeneralKQLExamples,getSpecificKQLExamples" {
			t.Fatalf("%s: tools %v", path, names)
		}
		// executeQuery's captured schema, plus the ids on the global endpoint.
		exec := tools[0]
		want := append([]string{"kqlQuery", "maxRecords"}, extra...)
		if got := dig(exec, "inputSchema", "required").([]any); fmt.Sprint(got) != fmt.Sprint(toAny(want)) {
			t.Errorf("%s: executeQuery required %v, want %v", path, got, want)
		}
		for _, p := range []string{"kqlQuery", "maxRecords", "activityTitle", "activityDescription", "clusterUrl", "databaseName"} {
			if dig(exec, "inputSchema", "properties", p) == nil {
				t.Errorf("%s: executeQuery has no %s", path, p)
			}
		}
		if dig(exec, "inputSchema", "properties", "kqlQuery", "description") != "kql query" ||
			dig(exec, "inputSchema", "properties", "maxRecords", "type") != "number" {
			t.Errorf("%s: executeQuery properties %v", path, dig(exec, "inputSchema", "properties"))
		}
		for _, g := range tools[1:] {
			if req := dig(g, "inputSchema", "required").([]any); req[0] != "referenceText" || len(req) != 1+len(extra) {
				t.Errorf("%s: %v required %v", path, dig(g, "name"), req)
			}
		}
	}
	for _, path := range []string{ehGlobal, f.scopedKQL(f.db)} {
		if w := serve(f.mux, "GET", path, f.token, ""); w.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d", path, w.Code)
		}
		if w := serve(f.mux, "DELETE", path, f.token, ""); w.Code != http.StatusNoContent {
			t.Errorf("DELETE %s = %d", path, w.Code)
		}
	}
}

func toAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func TestExecuteQueryAnswersAKustoDocumentNamedByTableKind(t *testing.T) {
	f := newEH(t)
	d := f.doc(f.scopedKQL(f.db), "executeQuery", map[string]any{"kqlQuery": "StormEvents | take 3", "maxRecords": 1000,
		"activityTitle": "storms", "activityDescription": "a look"})
	tables := d["Tables"].([]any)
	var kinds []string
	for _, tb := range tables {
		kinds = append(kinds, dig(tb, "TableName").(string))
	}
	if strings.Join(kinds, ",") != "QueryProperties,PrimaryResult,QueryCompletionInformation" {
		t.Fatalf("tables %v", kinds)
	}
	primary := tables[1]
	if dig(primary, "Columns", 0, "ColumnName") != "Db" || dig(primary, "Columns", 1, "ColumnType") != "long" {
		t.Errorf("columns %v", dig(primary, "Columns"))
	}
	// The engine's isolated database name never reaches the caller.
	if rows := dig(primary, "Rows").([]any); len(rows) != 3 || dig(rows, 0, 0) != "Telemetry" {
		t.Errorf("rows %v, want three, naming the display name", rows)
	}
	if last := f.engine.calls[len(f.engine.calls)-1]; !strings.HasPrefix(last, "/v2/rest/query "+engineDatabaseName(f.db.ID)+" ") {
		t.Errorf("ran %q, want a v2 query in the database's own engine database", last)
	}
	// maxRecords cuts the primary result; above 1,000 it is 1,000, silently.
	f.engine.rows = 1200
	if rows := dig(f.doc(f.scopedKQL(f.db), "executeQuery", map[string]any{"kqlQuery": "T", "maxRecords": 2}), "Tables", 1, "Rows").([]any); len(rows) != 2 {
		t.Errorf("maxRecords 2: %d rows", len(rows))
	}
	if rows := dig(f.doc(f.scopedKQL(f.db), "executeQuery", map[string]any{"kqlQuery": "T", "maxRecords": 5000}), "Tables", 1, "Rows").([]any); len(rows) != 1000 {
		t.Errorf("maxRecords 5000: %d rows, want the 1,000 cap", len(rows))
	}
	f.engine.rows = 0
	if rows := dig(f.doc(f.scopedKQL(f.db), "executeQuery", map[string]any{"kqlQuery": "T", "maxRecords": 5}), "Tables", 1, "Rows").([]any); len(rows) != 0 {
		t.Errorf("no rows is an empty list: %v", rows)
	}
}

func TestEventhouseMCPRefusesWhatItCannotRunByName(t *testing.T) {
	f := newEH(t)
	notebook := f.item("Notebook", "nb")
	other := &store.Workspace{DisplayName: "elsewhere"}
	if err := f.st.CreateWorkspace(other, store.Principal{ID: "someone-else", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	hidden := &store.Item{WorkspaceID: other.ID, Type: "KQLDatabase", DisplayName: "hidden"}
	if err := f.st.CreateItem(hidden, nil); err != nil {
		t.Fatal(err)
	}
	q := func(extra map[string]any) map[string]any {
		args := map[string]any{"workspaceId": f.ws.ID, "itemId": f.db.ID, "kqlQuery": "T", "maxRecords": 5}
		for k, v := range extra {
			args[k] = v
		}
		return args
	}
	for want, args := range map[string]map[string]any{
		"workspaceId and itemId are required":             {"kqlQuery": "T", "maxRecords": 5},
		"kqlQuery is required":                            q(map[string]any{"kqlQuery": " "}),
		"maxRecords is required":                          q(map[string]any{"maxRecords": nil}),
		"at least 1":                                      q(map[string]any{"maxRecords": 0}),
		"a whole number":                                  q(map[string]any{"maxRecords": 2.5}),
		"is an Eventhouse; pass one of its KQL databases": q(map[string]any{"itemId": f.eh.ID}),
		"is a Notebook":                                   q(map[string]any{"itemId": notebook.ID}),
		"was not found in workspace " + f.ws.ID:           q(map[string]any{"itemId": "nope"}),
		"was not found in workspace " + other.ID + ", or you cannot read it": q(map[string]any{
			"workspaceId": other.ID, "itemId": hidden.ID}),
		"go together":                          q(map[string]any{"clusterUrl": "https://x/kusto/a/b"}),
		"hosts no Azure Data Explorer cluster": q(map[string]any{"clusterUrl": "https://help.kusto.windows.net", "databaseName": "Samples"}),
		"database Nope was not found":          q(map[string]any{"clusterUrl": "http://h/kusto/" + f.ws.ID + "/" + f.eh.ID, "databaseName": "Nope"}),
	} {
		texts, isErr := f.call(ehGlobal, "executeQuery", args)
		if !isErr || !strings.Contains(texts[0], want) {
			t.Errorf("%v: %v %q, want an error containing %q", args, isErr, texts, want)
		}
	}
	// The scoped endpoint refuses another database, and a call naming its own is fine.
	if texts, isErr := f.call(f.scopedKQL(f.db), "executeQuery", map[string]any{"itemId": f.other.ID, "kqlQuery": "T", "maxRecords": 1}); !isErr ||
		!strings.Contains(texts[0], "bound to KQL database "+f.db.ID) {
		t.Errorf("scoped: %v %q", isErr, texts)
	}
	if _, isErr := f.call(f.scopedKQL(f.db), "executeQuery", map[string]any{"workspaceId": f.ws.ID, "itemId": f.db.ID, "kqlQuery": "T", "maxRecords": 1}); isErr {
		t.Error("the bound database, named again")
	}
	// No engine attached.
	f.a.KQLURL = nil
	if texts, isErr := f.call(ehGlobal, "executeQuery", q(nil)); !isErr || !strings.Contains(texts[0], "FABRIC_KQL_URL") {
		t.Errorf("no engine: %v %q", isErr, texts)
	}
}

// clusterUrl and databaseName run the query in another database of an
// eventhouse this emulator serves, still as the caller.
func TestClusterUrlRedirectsToAnotherDatabaseOfAnEventhouse(t *testing.T) {
	f := newEH(t)
	cluster := "https://any-host/kusto/" + f.ws.ID + "/" + f.eh.ID
	d := f.doc(f.scopedKQL(f.db), "executeQuery", map[string]any{"kqlQuery": "T", "maxRecords": 1,
		"clusterUrl": cluster, "databaseName": "Archive"})
	if dig(d, "Tables", 1, "Rows", 0, 0) != "Archive" {
		t.Errorf("ran in %v, want Archive", dig(d, "Tables", 1, "Rows"))
	}
}

func TestAFailedQueryNamesTheClusterDatabaseAndTheEnginesError(t *testing.T) {
	f := newEH(t)
	texts, isErr := f.call(f.scopedKQL(f.db), "executeQuery", map[string]any{"kqlQuery": "Broken | take 1", "maxRecords": 1})
	want := "Error in executing KQL query. cluster='http://example.com/kusto/" + f.ws.ID + "/" + f.eh.ID +
		"', database='Telemetry', Exception='Semantic error: {"
	if !isErr || !strings.HasPrefix(texts[0], want) || !strings.Contains(texts[0], `"@message": "Semantic error: 'take' operator`) ||
		strings.Contains(texts[0], engineDatabaseName(f.db.ID)) {
		t.Errorf("got %q\nwant it to start %q, carry the engine's body, and not the engine's database name", texts, want)
	}
	texts, isErr = f.call(f.scopedKQL(f.db), "executeQuery", map[string]any{"kqlQuery": "Partial", "maxRecords": 1})
	if !isErr || !strings.Contains(texts[0], "Exception='Query execution has exceeded the allowed limits: {") {
		t.Errorf("a failure reported in the completion frame: %v %q", isErr, texts)
	}
	texts, isErr = f.call(f.scopedKQL(f.db), "executeQuery", map[string]any{"kqlQuery": "Garbled", "maxRecords": 1})
	if !isErr || !strings.Contains(texts[0], "could not be read") {
		t.Errorf("an unreadable answer: %v %q", isErr, texts)
	}
}

func TestGetSchemaRanksByTheReferenceAndSamplesTheData(t *testing.T) {
	f := newEH(t)
	d := f.doc(f.scopedKQL(f.db), "getSchema", map[string]any{"referenceText": "which users signed in?"})
	if d["Database"] != "Telemetry" {
		t.Errorf("database %v", d["Database"])
	}
	tables := d["Tables"].([]any)
	if len(tables) != 2 || dig(tables, 0, "Name") != "Users" || dig(tables, 1, "Name") != "StormEvents" {
		t.Fatalf("Users is the relevant table and comes first: %v", tables)
	}
	first := tables[0]
	if dig(first, "RowCount") != float64(42) || len(dig(first, "SampleRows").([]any)) != 2 ||
		dig(first, "SampleRows", 0, "Db") != "a" || dig(first, "Columns", 0, "Type") != "string" {
		t.Errorf("samples, count and columns: %v", first)
	}
	if fmt.Sprint(d["MaterializedViews"]) != "[DailyCounts]" || dig(d, "Functions", 0, "Name") != "Archive" ||
		dig(d, "Functions", 1, "Parameters", 0) != "n:long" {
		t.Errorf("views and functions: %v %v", d["MaterializedViews"], d["Functions"])
	}
	// With no reference words in common, tables are listed by name.
	d = f.doc(f.scopedKQL(f.db), "getSchema", map[string]any{"referenceText": "xyz"})
	if dig(d, "Tables", 0, "Name") != "StormEvents" {
		t.Errorf("ties by name: %v", dig(d, "Tables"))
	}
	if !strings.Contains(fmt.Sprint(f.engine.calls), "['Users'] | take 5") {
		t.Error("a table name is quoted when sampled")
	}
}

func TestGetSchemaSamplesOnlyTheFirstTables(t *testing.T) {
	f := newEH(t)
	f.engine.tables = map[string][]string{}
	for i := 0; i < ehSchemaTables+2; i++ {
		f.engine.tables[fmt.Sprintf("T%02d", i)] = []string{"c"}
	}
	tables := f.doc(f.scopedKQL(f.db), "getSchema", map[string]any{"referenceText": "x"})["Tables"].([]any)
	if len(tables) != ehSchemaTables+2 || dig(tables, ehSchemaTables-1, "RowCount") == nil || dig(tables, ehSchemaTables, "RowCount") != nil {
		t.Errorf("the first %d tables are sampled, the rest listed", ehSchemaTables)
	}
}

func TestTheGroundingToolsRefuseAnEmptyDatabase(t *testing.T) {
	f := newEH(t)
	f.engine.tables = nil
	for _, tool := range []string{"getSchema", "getGeneralKQLExamples", "getSpecificKQLExamples"} {
		if texts, isErr := f.call(f.scopedKQL(f.db), tool, map[string]any{"referenceText": "x"}); !isErr || texts[0] != "Database is empty" {
			t.Errorf("%s: %v %q", tool, isErr, texts)
		}
	}
	// executeQuery does not ground on tables, so it runs.
	if _, isErr := f.call(f.scopedKQL(f.db), "executeQuery", map[string]any{"kqlQuery": "print 1", "maxRecords": 1}); isErr {
		t.Error("executeQuery on an empty database")
	}
}

func TestTheExampleTools(t *testing.T) {
	f := newEH(t)
	texts, isErr := f.call(f.scopedKQL(f.db), "getGeneralKQLExamples", map[string]any{"referenceText": "how many distinct users"})
	if isErr || !strings.HasPrefix(texts[0], "# General KQL examples") || strings.Count(texts[0], "```kql") != 5 ||
		!strings.Contains(texts[0][:strings.Index(texts[0], "```kql")+80], "dcount(UserId)") {
		t.Errorf("general: %v %q", isErr, texts)
	}
	texts, isErr = f.call(f.scopedKQL(f.db), "getSpecificKQLExamples", map[string]any{"referenceText": "x"})
	if isErr || texts[0] != "No examples have been curated for database Telemetry yet." {
		t.Errorf("specific: %v %q", isErr, texts)
	}
}

func TestTheEngineFailingIsReportedNotHidden(t *testing.T) {
	f := newEH(t)
	// An engine that refuses everything, including the database bootstrap.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(dead.Close)
	if err := f.a.SetKQLBackend(dead.URL); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"executeQuery", "getSchema"} {
		if _, isErr := f.call(f.scopedKQL(f.db), tool, map[string]any{"kqlQuery": "T", "maxRecords": 1, "referenceText": "x"}); !isErr {
			t.Errorf("%s against a failing engine", tool)
		}
	}
	// An engine that cannot be reached at all.
	dead.Close()
	if _, isErr := f.call(f.scopedKQL(f.db), "executeQuery", map[string]any{"kqlQuery": "T", "maxRecords": 1}); !isErr {
		t.Error("unreachable engine")
	}
}

func TestTheEnginesOddAnswersAreHandled(t *testing.T) {
	f := newEH(t)
	run := func(q string) ([]string, bool) {
		return f.call(f.scopedKQL(f.db), "executeQuery", map[string]any{"kqlQuery": q, "maxRecords": 5})
	}
	// The connection drops after the database exists.
	if _, isErr := run("print 1"); isErr {
		t.Fatal("warm-up")
	}
	if _, isErr := run("Hangup"); !isErr {
		t.Error("a dropped connection is an error")
	}
	// An error body that is not JSON is passed on as it came.
	if texts, isErr := run("NotJSON"); !isErr || !strings.HasSuffix(texts[0], "Exception='Error: the engine said no'") {
		t.Errorf("not JSON: %q", texts)
	}
	// A primary result with null rows is an empty list, not null.
	d := f.doc(f.scopedKQL(f.db), "executeQuery", map[string]any{"kqlQuery": "NullRows", "maxRecords": 5})
	if rows, ok := dig(d, "Tables", 0, "Rows").([]any); !ok || len(rows) != 0 {
		t.Errorf("null rows: %v", dig(d, "Tables", 0))
	}
	// A table whose sample or count fails is still listed, unsampled; a row
	// that is not a list is skipped.
	f.engine.tables = map[string][]string{"FailSample": {"c"}, "FailCount": {"c"}, "BadRow": {"c"}}
	tables := f.doc(f.scopedKQL(f.db), "getSchema", map[string]any{"referenceText": "x"})["Tables"].([]any)
	for _, tb := range tables {
		switch dig(tb, "Name") {
		case "FailSample", "FailCount":
			if dig(tb, "RowCount") != nil {
				t.Errorf("%v: a failed sample is not reported as data", dig(tb, "Name"))
			}
		case "BadRow":
			if s := dig(tb, "SampleRows").([]any); len(s) != 1 || dig(s, 0, "c") != "ok" {
				t.Errorf("bad row: %v", s)
			}
		}
	}
	// A schema the engine answers unreadably, or with no database at all.
	for raw, want := range map[string]string{
		`not json`:                                       "could not be read",
		`{"Tables":[{"Rows":[["not json"]]}]}`:           "could not be read",
		`{"Tables":[]}`:                                  "could not be read",
		`{"Tables":[{"Rows":[[]]}]}`:                     "could not be read",
		`{"Tables":[{"Rows":[["{\"Databases\":{}}"]]}]}`: "Database is empty",
	} {
		f.engine.schemaRaw = raw
		if texts, isErr := f.call(f.scopedKQL(f.db), "getSchema", map[string]any{"referenceText": "x"}); !isErr || !strings.Contains(texts[0], want) {
			t.Errorf("%s: %q, want %q", raw, texts, want)
		}
	}
}
