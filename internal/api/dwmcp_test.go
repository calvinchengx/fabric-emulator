package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// Fabric Data Warehouse MCP, with the SQL engine stubbed: what the server owns
// is the endpoint, the tool, which item a call runs on and who may name it, and
// the CSV it answers with. Running the batch as the caller — the refusals,
// dialect, row-level security — is the TDS wire's, and is witnessed against a
// real SQL Server in internal/server/sqlexec_test.go.

type dwFixture struct {
	t        *testing.T
	a        *API
	st       *store.Store
	mux      *http.ServeMux
	token    string
	ws       *store.Workspace
	wh       *store.Item
	lh       *store.Item
	endpoint *store.Item
	// calls records what the engine was asked to run.
	calls []dwCall
	// answer is what the stub engine returns.
	answer *SQLBatchResult
	err    error
}

type dwCall struct{ item, principal, query string }

const dwCaller = "route-admin"

func newDW(t *testing.T) *dwFixture {
	t.Helper()
	mux, a, st, token := newRegisteredAPIWithHooks(t)
	f := &dwFixture{t: t, a: a, st: st, mux: mux, token: token,
		answer: &SQLBatchResult{Columns: []string{"n"}, Types: []string{"INT"}, Rows: [][]any{{int64(1)}}}}
	f.ws = &store.Workspace{DisplayName: "dw"}
	if err := st.CreateWorkspace(f.ws, store.Principal{ID: dwCaller, Type: "User"}); err != nil {
		t.Fatal(err)
	}
	f.wh = f.item("Warehouse", "Sales")
	f.lh = f.item("Lakehouse", "Bronze")
	f.endpoint = &store.Item{ID: a.ensureSQLEndpointItem(f.lh)}
	a.SQLExecAs = func(_ context.Context, item, principal, query string, maxRows int) (*SQLBatchResult, error) {
		if maxRows != dwMaxRows {
			t.Errorf("maxRows = %d, want the observed %d", maxRows, dwMaxRows)
		}
		f.calls = append(f.calls, dwCall{item, principal, query})
		return f.answer, f.err
	}
	return f
}

func (f *dwFixture) item(typ, name string) *store.Item {
	it := &store.Item{WorkspaceID: f.ws.ID, Type: typ, DisplayName: name}
	if err := f.st.CreateItem(it, nil); err != nil {
		f.t.Fatal(err)
	}
	return it
}

// rpc posts one JSON-RPC request to path through the registered mux.
func (f *dwFixture) rpc(path, method string, params any) map[string]any {
	f.t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	w := serve(f.mux, "POST", path, f.token, string(body))
	if w.Code != http.StatusOK {
		f.t.Fatalf("%s %s: HTTP %d %s", method, path, w.Code, w.Body)
	}
	var env map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		f.t.Fatal(err)
	}
	return env
}

// call runs a tool and returns its text blocks and whether it is a tool error.
func (f *dwFixture) call(path, tool string, args map[string]any) ([]string, bool) {
	f.t.Helper()
	env := f.rpc(path, "tools/call", map[string]any{"name": tool, "arguments": args})
	var res mcpToolResult
	b, _ := json.Marshal(env["result"])
	if err := json.Unmarshal(b, &res); err != nil {
		f.t.Fatal(err)
	}
	var texts []string
	for _, c := range res.Content {
		texts = append(texts, c.Text)
	}
	return texts, res.IsError
}

const dwGlobal = "/v1/mcp/dataPlane/sqlEndpoint"

func (f *dwFixture) scoped(it *store.Item) string {
	return "/v1/mcp/dataPlane/workspaces/" + f.ws.ID + "/items/" + it.ID + "/sqlEndpoint"
}

func TestDataWarehouseMCPServesOneToolAtBothEndpoints(t *testing.T) {
	f := newDW(t)
	for path, required := range map[string][]any{
		dwGlobal:           {"workspaceId", "itemId", "query"},
		f.scoped(f.wh):     {"query"},
		f.scoped(f.lake()): {"query"},
	} {
		init := f.rpc(path, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
		if dig(init, "result", "serverInfo", "name") != "fabric-data-warehouse" {
			t.Errorf("%s: serverInfo %v", path, dig(init, "result", "serverInfo"))
		}
		tools := dig(f.rpc(path, "tools/list", nil), "result", "tools").([]any)
		if len(tools) != 1 || dig(tools, 0, "name") != "execute_query" {
			t.Fatalf("%s: tools %v, want execute_query alone", path, tools)
		}
		got := dig(tools, 0, "inputSchema", "required").([]any)
		if len(got) != len(required) {
			t.Errorf("%s: required %v, want %v", path, got, required)
		}
		// Writes run where the caller may write, so the tool is not read-only.
		if dig(tools, 0, "annotations") != nil {
			t.Errorf("%s: annotations %v", path, dig(tools, 0, "annotations"))
		}
	}
	if !strings.Contains(dig(f.rpc(f.scoped(f.wh), "initialize", nil), "result", "instructions").(string), f.wh.ID) {
		t.Error("the item-scoped endpoint's instructions should name the item it is bound to")
	}
	for _, path := range []string{dwGlobal, f.scoped(f.wh)} {
		if w := serve(f.mux, "GET", path, f.token, ""); w.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d", path, w.Code)
		}
		if w := serve(f.mux, "DELETE", path, f.token, ""); w.Code != http.StatusNoContent {
			t.Errorf("DELETE %s = %d", path, w.Code)
		}
		if w := serve(f.mux, "POST", path, "", `{}`); w.Code != http.StatusUnauthorized {
			t.Errorf("POST %s without a token = %d", path, w.Code)
		}
	}
}

// lake is the endpoint item as the store holds it.
func (f *dwFixture) lake() *store.Item {
	it, err := f.st.GetItemByID(f.endpoint.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return it
}

func TestExecuteQueryRunsTheBatchAsTheCallerOnTheNamedItem(t *testing.T) {
	f := newDW(t)
	texts, isErr := f.call(dwGlobal, "execute_query", map[string]any{
		"workspaceId": f.ws.ID, "itemId": f.wh.ID, "query": "SELECT 1 AS n"})
	if isErr || len(texts) != 2 || texts[0] != "n\r\n1\r\n" || texts[1] != "1 row(s), 1 column(s)." {
		t.Fatalf("got %v %q", isErr, texts)
	}
	if len(f.calls) != 1 || f.calls[0] != (dwCall{f.wh.ID, dwCaller, "SELECT 1 AS n"}) {
		t.Fatalf("engine asked %v", f.calls)
	}
	// The Learn page's name for the same tool.
	if _, isErr := f.call(dwGlobal, "executeSQL", map[string]any{
		"workspaceId": f.ws.ID, "itemId": f.wh.ID, "query": "SELECT 1 AS n"}); isErr {
		t.Error("executeSQL is the same tool")
	}
	// A SQL analytics endpoint runs on its lakehouse's database.
	f.calls = nil
	if _, isErr := f.call(dwGlobal, "execute_query", map[string]any{
		"workspaceId": f.ws.ID, "itemId": f.endpoint.ID, "query": "SELECT 1 AS n"}); isErr || f.calls[0].item != f.lh.ID {
		t.Errorf("endpoint: %v %v", isErr, f.calls)
	}
}

func TestTheItemScopedEndpointTakesItsItemFromTheURL(t *testing.T) {
	f := newDW(t)
	if _, isErr := f.call(f.scoped(f.wh), "execute_query", map[string]any{"query": "SELECT 1"}); isErr || f.calls[0].item != f.wh.ID {
		t.Fatalf("scoped: %v %v", isErr, f.calls)
	}
	// Naming the bound item again is fine, in any case; naming another is not.
	if _, isErr := f.call(f.scoped(f.wh), "execute_query", map[string]any{
		"workspaceId": strings.ToUpper(f.ws.ID), "itemId": f.wh.ID, "query": "SELECT 1"}); isErr {
		t.Error("the bound item, named again")
	}
	for _, args := range []map[string]any{
		{"itemId": f.endpoint.ID, "query": "SELECT 1"},
		{"workspaceId": "00000000-0000-0000-0000-000000000000", "query": "SELECT 1"},
	} {
		texts, isErr := f.call(f.scoped(f.wh), "execute_query", args)
		if !isErr || !strings.Contains(texts[0], "bound to item "+f.wh.ID) {
			t.Errorf("%v: %v %q", args, isErr, texts)
		}
	}
}

func TestExecuteQueryRefusesWhatItCannotRunByName(t *testing.T) {
	f := newDW(t)
	notebook := f.item("Notebook", "nb")
	other := &store.Workspace{DisplayName: "elsewhere"}
	if err := f.st.CreateWorkspace(other, store.Principal{ID: "someone-else", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	hidden := &store.Item{WorkspaceID: other.ID, Type: "Warehouse", DisplayName: "hidden"}
	if err := f.st.CreateItem(hidden, nil); err != nil {
		t.Fatal(err)
	}
	orphan := f.item("SQLEndpoint", "orphan")
	for want, args := range map[string]map[string]any{
		"workspaceId and itemId are required": {"query": "SELECT 1"},
		"query is required":                   {"workspaceId": f.ws.ID, "itemId": f.wh.ID, "query": "  "},
		"is a Lakehouse; pass its SQL analytics endpoint id": {
			"workspaceId": f.ws.ID, "itemId": f.lh.ID, "query": "SELECT 1"},
		"is a Notebook": {"workspaceId": f.ws.ID, "itemId": notebook.ID, "query": "SELECT 1"},
		"was not found in workspace " + f.ws.ID: {
			"workspaceId": f.ws.ID, "itemId": "no-such-item", "query": "SELECT 1"},
		// The right item in the wrong workspace is not found either.
		"was not found in workspace " + other.ID: {
			"workspaceId": other.ID, "itemId": f.wh.ID, "query": "SELECT 1"},
		// An item the caller cannot read is not found, and its type is not told.
		"was not found in workspace " + other.ID + ", or you cannot read it": {
			"workspaceId": other.ID, "itemId": hidden.ID, "query": "SELECT 1"},
		"has no lakehouse": {"workspaceId": f.ws.ID, "itemId": orphan.ID, "query": "SELECT 1"},
	} {
		texts, isErr := f.call(dwGlobal, "execute_query", args)
		if !isErr || !strings.Contains(texts[0], want) {
			t.Errorf("%v: %v %q, want an error containing %q", args, isErr, texts, want)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("a refused call reached the engine: %v", f.calls)
	}
}

func TestExecuteQueryReportsTheEnginesRefusalAndItsAbsence(t *testing.T) {
	f := newDW(t)
	args := map[string]any{"workspaceId": f.ws.ID, "itemId": f.wh.ID, "query": "INSERT INTO t VALUES (1)"}
	f.err = errors.New("the lakehouse SQL analytics endpoint is read-only; writes require a Warehouse")
	if texts, isErr := f.call(dwGlobal, "execute_query", args); !isErr || texts[0] != f.err.Error() {
		t.Errorf("engine refusal: %v %q", isErr, texts)
	}
	f.a.SQLExecAs = nil
	if texts, isErr := f.call(dwGlobal, "execute_query", args); !isErr || !strings.Contains(texts[0], "WAREHOUSE_MSSQL_DSN") {
		t.Errorf("no engine: %v %q", isErr, texts)
	}
}

func TestTheResultIsRFC4180CSVThenMetadata(t *testing.T) {
	when := time.Date(2026, 10, 4, 9, 30, 15, 123400000, time.FixedZone("", 8*3600))
	for _, tc := range []struct {
		name string
		res  *SQLBatchResult
		csv  string
		meta string
	}{
		{"no result set", &SQLBatchResult{}, "", "The batch completed and returned no result set."},
		{"an empty result set keeps its header", &SQLBatchResult{Columns: []string{"a", "b"}, Types: []string{"INT", "INT"}},
			"a,b\r\n", "0 row(s), 2 column(s)."},
		{"quoting", &SQLBatchResult{Columns: []string{"say, \"hi\""}, Types: []string{"NVARCHAR"},
			Rows: [][]any{{"one, two"}, {"line\nbreak"}, {`a "quote"`}}},
			"\"say, \"\"hi\"\"\"\r\n\"one, two\"\r\n\"line\nbreak\"\r\n\"a \"\"quote\"\"\"\r\n", "3 row(s), 1 column(s)."},
		{"truncated says so", &SQLBatchResult{Columns: []string{"n"}, Types: []string{"INT"}, Rows: [][]any{{int64(1)}}, Truncated: true},
			"n\r\n1\r\n", "1 row(s), 1 column(s); truncated at the 10000-row limit: narrow the query with TOP, WHERE or an aggregate."},
		{"every kind of value", &SQLBatchResult{
			Columns: []string{"null", "bit1", "bit0", "int", "float", "date", "time", "dto", "dt2", "other"},
			Types:   []string{"INT", "BIT", "BIT", "BIGINT", "FLOAT", "DATE", "TIME", "DATETIMEOFFSET", "DATETIME2", "X"},
			Rows:    [][]any{{nil, true, false, int64(-42), 2.5, when, when, when, when, []int{7}}}},
			"null,bit1,bit0,int,float,date,time,dto,dt2,other\r\n" +
				",1,0,-42,2.5,2026-10-04,09:30:15.1234,2026-10-04 09:30:15.1234 +08:00,2026-10-04 09:30:15.1234,[7]\r\n",
			"1 row(s), 10 column(s)."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := dwResult(tc.res)
			var texts []string
			for _, c := range out.Content {
				texts = append(texts, c.Text)
			}
			want := []string{tc.meta}
			if tc.csv != "" {
				want = []string{tc.csv, tc.meta}
			}
			if out.IsError || strings.Join(texts, "|") != strings.Join(want, "|") {
				t.Errorf("got %q\nwant %q", texts, want)
			}
		})
	}
}
