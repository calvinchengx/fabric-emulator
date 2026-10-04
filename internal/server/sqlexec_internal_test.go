package server

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/tds"
	"github.com/calvinchengx/fabric-emulator/internal/tsql"
)

// The executor's own decisions, against SQLite and a stub route, so they hold
// in every CI job rather than only the ones with a SQL Server. What the engine
// enforces as the caller is witnessed in sqlexec_test.go.

// execBackend logs nobody in: it hands back one SQLite database, or an error.
type execBackend struct {
	db    *sql.DB
	asErr error
	as    []string
}

func (b *execBackend) EnsureDatabase(context.Context, string) error { return nil }
func (b *execBackend) DB(string) *sql.DB                            { return b.db }
func (b *execBackend) DBAs(_ context.Context, database, principal string, grants []tds.Grant) (*sql.DB, error) {
	b.as = append(b.as, database+"/"+principal+"/"+grants[0].Database)
	if b.asErr != nil {
		return nil, b.asErr
	}
	// A pool of its own, as the real backend's is: the executor closes it.
	return sql.Open("sqlite", "file:exec?mode=memory&cache=shared")
}

func newExecDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:exec?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE typed (id UNIQUEIDENTIFIER, bin VARBINARY, txt NVARCHAR, n INT);
		DELETE FROM typed;
		INSERT INTO typed VALUES (x'FF1996F6868B11D0B42D00C04FC964FF', x'CAFE', CAST('text' AS BLOB), 7);`); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestSQLExecAsForDecidesBeforeTheEngine(t *testing.T) {
	db := newExecDB(t)
	be := &execBackend{db: db}
	var observed []string
	wire := &tds.Server{Observe: func(database string, _ []tsql.Flow) { observed = append(observed, database) }}
	conn := tds.Connection{TargetDB: "wh"}
	route := func(_ context.Context, server, database, principal string) (tds.Connection, error) {
		if server != "" || database != "wh" {
			t.Errorf("routed (%q, %q): the executor addresses an item by id, with no server name", server, database)
		}
		if principal == "stranger" {
			return tds.Connection{}, errors.New("access denied")
		}
		return conn, nil
	}
	exec := sqlExecAsFor(be, route, wire)
	ctx := context.Background()

	if _, err := exec(ctx, "wh", "stranger", "SELECT 1", 10); err == nil || err.Error() != "access denied" {
		t.Errorf("route refusal: %v", err)
	}
	conn.ReadOnly = true
	if _, err := exec(ctx, "wh", "viewer", "INSERT INTO typed VALUES (NULL, NULL, NULL, 1)", 10); err == nil ||
		!strings.Contains(err.Error(), "read-only") {
		t.Errorf("the wire's refusal: %v", err)
	}
	if len(be.as) != 0 {
		t.Errorf("a refused batch logged in: %v", be.as)
	}
	conn.ReadOnly = false
	be.asErr = errors.New("cannot provision")
	if _, err := exec(ctx, "wh", "alice", "SELECT 1", 10); err == nil || err.Error() != "cannot provision" {
		t.Errorf("login failure: %v", err)
	}
	if be.as[0] != "wh/alice/wh" {
		t.Errorf("logged in as %v, want alice in wh with wh's grant first", be.as)
	}
	be.asErr = nil
	if _, err := exec(ctx, "wh", "alice", "SELECT * FROM missing", 10); err == nil {
		t.Error("an engine error is returned")
	}
	if len(observed) != 0 {
		t.Errorf("a failed batch was observed: %v", observed)
	}
	if _, err := exec(ctx, "wh", "alice", "INSERT INTO typed (n) SELECT n FROM typed WHERE 0 = 1", 10); err != nil {
		t.Fatal(err)
	}
	if len(observed) != 1 || observed[0] != "wh" {
		t.Errorf("an accepted write is observed once, in its database: %v", observed)
	}
}

func TestLastResultSetPrintsWhatCSVCanCarry(t *testing.T) {
	db := newExecDB(t)
	ctx := context.Background()
	res, err := lastResultSet(ctx, db, "SELECT id, bin, txt, n FROM typed", 10)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Types, ",") != "UNIQUEIDENTIFIER,VARBINARY,NVARCHAR,INT" {
		t.Fatalf("types %v", res.Types)
	}
	// SQL Server's mixed-endian GUID bytes, decoded; other binary as hex; text as text.
	row := res.Rows[0]
	if row[0] != "F69619FF-8B86-D011-B42D-00C04FC964FF" || row[1] != "0xCAFE" || row[2] != "text" || row[3] != int64(7) {
		t.Errorf("row %#v", row)
	}
	// A batch with no result set at all.
	if res, err := lastResultSet(ctx, db, "UPDATE typed SET n = n", 10); err != nil || len(res.Columns) != 0 {
		t.Errorf("no result set: %v %+v", err, res)
	}
	// Truncation keeps maxRows and says so.
	if res, err := lastResultSet(ctx, db, "SELECT n FROM typed UNION ALL SELECT n FROM typed", 1); err != nil ||
		len(res.Rows) != 1 || !res.Truncated {
		t.Errorf("truncation: %v %+v", err, res)
	}
	// A uniqueidentifier that is not sixteen bytes is an error, not a guess.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS bad (id UNIQUEIDENTIFIER); DELETE FROM bad; INSERT INTO bad VALUES (x'00')`); err != nil {
		t.Fatal(err)
	}
	if _, err := lastResultSet(ctx, db, "SELECT id FROM bad", 10); err == nil {
		t.Error("a malformed uniqueidentifier should fail")
	}
	if _, err := lastResultSet(ctx, db, "SELECT nope FROM typed", 10); err == nil {
		t.Error("a query the engine refuses fails")
	}
}

func TestVersionedRouteGivesAWarehouseItsHistory(t *testing.T) {
	st, err := store.Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ws := &store.Workspace{DisplayName: "w"}
	if err := st.CreateWorkspace(ws, store.Principal{ID: "p", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	wh := &store.Item{WorkspaceID: ws.ID, Type: "Warehouse", DisplayName: "wh"}
	db := &store.Item{WorkspaceID: ws.ID, Type: "SQLDatabase", DisplayName: "db"}
	for _, it := range []*store.Item{wh, db} {
		if err := st.CreateItem(it, nil); err != nil {
			t.Fatal(err)
		}
	}
	route := versionedRoute(func(_ context.Context, _, database, _ string) (tds.Connection, error) {
		if database == "refused" {
			return tds.Connection{}, errors.New("no")
		}
		return tds.Connection{TargetDB: database}, nil
	}, st, 30)
	ctx := context.Background()
	if c, err := route(ctx, "", wh.ID, "p"); err != nil || c.TimeTravel == nil {
		t.Errorf("warehouse: %v, resolver set %v", err, c.TimeTravel != nil)
	}
	if c, err := route(ctx, "", db.ID, "p"); err != nil || c.TimeTravel != nil {
		t.Errorf("a SQL Database never travels: %v", err)
	}
	if _, err := route(ctx, "", "refused", "p"); err == nil {
		t.Error("the route's refusal is kept")
	}
}

// failingDriver returns one row and then the error a later statement in the
// batch raised, the way SQL Server reports it: after the rows already read.
type failingDriver struct{}
type failingConn struct{ driver.Conn }
type failingRows struct{ sent bool }

func (failingDriver) Open(string) (driver.Conn, error) { return failingConn{}, nil }
func (failingConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("not used")
}
func (failingConn) Close() error { return nil }
func (failingConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &failingRows{}, nil
}
func (*failingRows) Columns() []string { return []string{"a"} }
func (*failingRows) Close() error      { return nil }
func (r *failingRows) Next(dest []driver.Value) error {
	if r.sent {
		return mssql.Error{Number: 208, Message: "Invalid object name 'dbo.no_such_table'."}
	}
	r.sent, dest[0] = true, int64(1)
	return nil
}

func init() { sql.Register("sqlexec-failing", failingDriver{}) }

func TestLastResultSetReportsAnErrorRaisedAfterRows(t *testing.T) {
	// SQL Server's words reach the caller without the driver's decoration.
	if got := engineMessage(mssql.Error{Number: 208, Message: "Invalid object name 'x'."}); got.Error() != "Invalid object name 'x'." {
		t.Errorf("engineMessage = %q", got)
	}
	if plain := errors.New("access denied"); engineMessage(plain) != plain {
		t.Error("an error that is not the engine's is kept as it is")
	}
	db, err := sql.Open("sqlexec-failing", "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if res, err := lastResultSet(context.Background(), db, "SELECT 1 AS a; SELECT * FROM dbo.no_such_table", 10); err == nil ||
		!strings.Contains(err.Error(), "no_such_table") {
		t.Fatalf("got %+v, %v: the later statement's error must fail the batch", res, err)
	}
}

// A FedAuth login whose token names nobody is refused before any routing, and
// says why; one that does is routed as its principal.
func TestTokenRouteResolvesThePrincipalFirst(t *testing.T) {
	var routed string
	route := tokenRoute(func(token string) (string, error) {
		if token == "bad" {
			return "", errors.New("signature invalid")
		}
		return "p-" + token, nil
	}, func(_ context.Context, _, _, principal string) (tds.Connection, error) {
		routed = principal
		return tds.Connection{TargetDB: "wh"}, nil
	})
	if _, err := route(context.Background(), "", "wh", "bad"); err == nil ||
		!strings.Contains(err.Error(), "resolving principal: signature invalid") || routed != "" {
		t.Errorf("bad token: %v, routed %q", err, routed)
	}
	if c, err := route(context.Background(), "", "wh", "ok"); err != nil || c.TargetDB != "wh" || routed != "p-ok" {
		t.Errorf("good token: %v %+v routed %q", err, c, routed)
	}
}
