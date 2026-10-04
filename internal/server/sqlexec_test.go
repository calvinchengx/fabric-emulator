package server_test

// Fabric's Data Warehouse MCP server runs T-SQL as its caller through the same
// route, refusals, dialect and observers as the TDS wire (sqlexec.go). This is
// that claim against a real SQL Server: each assertion is paired with another
// caller, or the other surface, getting the other answer in the same run, so a
// policy that refused everyone or an executor that ran as the service account
// would fail it.

import (
	"context"
	"strings"
	"sync"
	"testing"

	entra "github.com/calvinchengx/entra-emulator/emulator"

	"github.com/calvinchengx/fabric-emulator/internal/api"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/tsql"
)

func (f *secFixture) execAs(t *testing.T, item, principal, query string, maxRows int) (*api.SQLBatchResult, error) {
	t.Helper()
	if f.srv.API.SQLExecAs == nil {
		t.Fatal("a SQL backend is configured but SQLExecAs is not wired")
	}
	return f.srv.API.SQLExecAs(context.Background(), item, principal, query, maxRows)
}

func (f *secFixture) assignRole(t *testing.T, principal, role string) {
	t.Helper()
	if err := f.srv.Store.CreateRoleAssignment(&store.RoleAssignment{
		WorkspaceID: f.ws.ID, Principal: store.Principal{ID: principal, Type: "User"}, Role: role,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSQLExecAsRunsAsTheCallerWithTheWiresRules(t *testing.T) {
	f := newSecFixture(t)
	owner := entra.DaemonClientID
	alice := "aaaa1111-0000-0000-0000-0000000d3c01"
	bob := "bbbb2222-0000-0000-0000-0000000d3c02"
	viewer := "cccc3333-0000-0000-0000-0000000d3c03"
	f.assignRole(t, alice, store.RoleContributor)
	f.assignRole(t, bob, store.RoleContributor)
	f.assignRole(t, viewer, store.RoleViewer)

	// The owner authors the table and a row-level security policy keyed on the
	// connected user, all through the MCP executor.
	for _, stmt := range []string{
		`CREATE TABLE dbo.mcp_sales (owner_name sysname, amount int)`,
		`INSERT INTO dbo.mcp_sales VALUES (N'` + alice + `', 10), (N'` + bob + `', 20)`,
		`CREATE SCHEMA mcpsec`,
		`CREATE FUNCTION mcpsec.fn_owner(@owner sysname) RETURNS TABLE WITH SCHEMABINDING
		   AS RETURN SELECT 1 AS ok WHERE @owner = USER_NAME()`,
		`CREATE SECURITY POLICY mcpsec.p ADD FILTER PREDICATE mcpsec.fn_owner(owner_name)
		   ON dbo.mcp_sales WITH (STATE = ON)`,
	} {
		if res, err := f.execAs(t, f.wh.ID, owner, stmt, 10); err != nil || len(res.Columns) != 0 {
			t.Fatalf("owner %q: %v %+v", strings.Fields(stmt)[0], err, res)
		}
	}

	// Two callers, one query, each sees only their own row: the batch ran as
	// them, not as the relay's account.
	for who, want := range map[string]int64{alice: 10, bob: 20} {
		res, err := f.execAs(t, f.wh.ID, who, `SELECT owner_name, amount FROM dbo.mcp_sales`, 10)
		if err != nil {
			t.Fatalf("%s: %v", who[:8], err)
		}
		if len(res.Rows) != 1 || res.Rows[0][0] != who || res.Rows[0][1] != want {
			t.Errorf("row-level security: %s saw %v", who[:8], res.Rows)
		}
	}

	// A Viewer's session is read-only, by the wire's own refusal; the same
	// INSERT from a Contributor succeeds.
	if _, err := f.execAs(t, f.wh.ID, viewer, `INSERT INTO dbo.mcp_sales VALUES (N'x', 1)`, 10); err == nil ||
		!strings.Contains(err.Error(), "this session is read-only") {
		t.Errorf("viewer write: %v, want the read-only session's refusal", err)
	}
	if _, err := f.execAs(t, f.wh.ID, alice, `INSERT INTO dbo.mcp_sales VALUES (N'`+alice+`', 1)`, 10); err != nil {
		t.Errorf("contributor write: %v", err)
	}

	// No role and no grant: the route refuses before the engine is reached.
	if _, err := f.execAs(t, f.wh.ID, "dddd4444-0000-0000-0000-0000000d3c04", `SELECT 1`, 10); err == nil ||
		!strings.Contains(err.Error(), "access denied") {
		t.Errorf("stranger: %v", err)
	}
	// The engine's own error comes back as it said it.
	if _, err := f.execAs(t, f.wh.ID, owner, `SELECT * FROM dbo.no_such_table`, 10); err == nil ||
		!strings.Contains(err.Error(), "no_such_table") {
		t.Errorf("engine error: %v", err)
	}
}

// Only the last result set comes back, at most maxRows of it, and a set left
// unread does not stop the batch reaching the ones after it.
func TestSQLExecAsReturnsTheLastResultSet(t *testing.T) {
	f := newSecFixture(t)
	owner := entra.DaemonClientID
	res, err := f.execAs(t, f.wh.ID, owner, `SELECT 1 AS a; SELECT 2 AS b, N'x' AS c`, 10)
	if err != nil || strings.Join(res.Columns, ",") != "b,c" || len(res.Rows) != 1 || res.Rows[0][1] != "x" {
		t.Fatalf("last set: %v %+v", err, res)
	}
	three := `SELECT n FROM (VALUES (1),(2),(3)) v(n)`
	res, err = f.execAs(t, f.wh.ID, owner, three, 2)
	if err != nil || len(res.Rows) != 2 || !res.Truncated {
		t.Fatalf("truncation: %v %+v", err, res)
	}
	res, err = f.execAs(t, f.wh.ID, owner, three+`; SELECT 9 AS z`, 2)
	if err != nil || strings.Join(res.Columns, ",") != "z" || res.Truncated {
		t.Fatalf("a truncated set is drained: %v %+v", err, res)
	}
	// A statement that fails after an earlier one returned rows fails the
	// batch: its error is not lost behind the result set already read.
	if res, err := f.execAs(t, f.wh.ID, owner, `SELECT 1 AS a; SELECT * FROM dbo.no_such_table`, 10); err == nil {
		t.Fatalf("a failing second statement returned %+v", res)
	}
	// A value of each type that needs converting for CSV.
	res, err = f.execAs(t, f.wh.ID, owner, `SELECT
		CAST('6F9619FF-8B86-D011-B42D-00C04FC964FF' AS uniqueidentifier) AS id,
		CAST(0xCAFE AS varbinary(2)) AS bin, CAST(12.50 AS decimal(5,2)) AS dec,
		CAST(NULL AS int) AS nothing`, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Rows[0]; got[0] != "6F9619FF-8B86-D011-B42D-00C04FC964FF" || got[1] != "0xCAFE" || got[2] != "12.50" || got[3] != nil {
		t.Errorf("values: %#v", got)
	}
}

// A lakehouse's SQL analytics endpoint is read-only for data on this path as on
// the wire, while its own SQL objects are still authored there.
func TestSQLExecAsKeepsTheAnalyticsEndpointReadOnly(t *testing.T) {
	f := newSecFixture(t)
	lh := &store.Item{WorkspaceID: f.ws.ID, Type: "Lakehouse", DisplayName: "lh"}
	if err := f.srv.Store.CreateItem(lh, nil); err != nil {
		t.Fatal(err)
	}
	owner := entra.DaemonClientID
	if _, err := f.execAs(t, lh.ID, owner, `CREATE TABLE dbo.t (a int)`, 10); err == nil ||
		!strings.Contains(err.Error(), "SQL analytics endpoint is read-only") {
		t.Errorf("endpoint table DDL: %v, want the endpoint's refusal", err)
	}
	if _, err := f.execAs(t, lh.ID, owner, `CREATE VIEW dbo.v AS SELECT 1 AS x`, 10); err != nil {
		t.Errorf("a view is the endpoint's own object: %v", err)
	}
	if res, err := f.execAs(t, lh.ID, owner, `SELECT x FROM dbo.v`, 10); err != nil || len(res.Rows) != 1 {
		t.Errorf("read the view: %v %+v", err, res)
	}
}

// A write the engine accepted is observed for lineage, as the wire's is; one it
// refused is not.
func TestSQLExecAsObservesTheWritesItRan(t *testing.T) {
	f := newSecFixture(t)
	var mu sync.Mutex
	var seen []tsql.Flow
	f.srv.TDS.Observe = func(database string, flows []tsql.Flow) {
		mu.Lock()
		defer mu.Unlock()
		if database == f.wh.ID {
			seen = append(seen, flows...)
		}
	}
	owner := entra.DaemonClientID
	for _, stmt := range []string{`CREATE TABLE dbo.src (a int)`, `CREATE TABLE dbo.dst (a int)`,
		`INSERT INTO dbo.dst SELECT a FROM dbo.src`} {
		if _, err := f.execAs(t, f.wh.ID, owner, stmt, 10); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	n := len(seen)
	mu.Unlock()
	if n < 3 {
		t.Fatalf("observed %v, want the two creates and the insert", seen)
	}
	if _, err := f.execAs(t, f.wh.ID, owner, `INSERT INTO dbo.dst SELECT a FROM dbo.missing`, 10); err == nil {
		t.Fatal("a statement on a missing table should fail")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != n {
		t.Errorf("a refused write was observed: %v", seen[n:])
	}
}
