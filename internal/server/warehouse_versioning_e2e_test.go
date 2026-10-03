package server_test

// Warehouse time travel, Phase 4 and 5 (docs/35-warehouse-time-travel.md), end
// to end over the TDS wire: statements a client sends to a Warehouse become
// Delta versions, and `OPTION (FOR TIMESTAMP AS OF …)` reads them back --
// through the same front a dbt build uses. Gated on WAREHOUSE_MSSQL_DSN.

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	entra "github.com/calvinchengx/entra-emulator/emulator"
	"github.com/calvinchengx/fabric-emulator/internal/config"
	"github.com/calvinchengx/fabric-emulator/internal/server"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/testsupport"
	mssql "github.com/microsoft/go-mssqldb"
)

type versionedStack struct {
	srv  *server.Server
	conn *sql.Conn
	wh   *store.Item
}

// startVersionedWarehouse brings up the emulator with a frozen clock and a
// Warehouse, and returns ONE pinned connection to it. Pinned because the
// observer runs on the session goroutine after the response: the next
// statement on the same session cannot be read until it has finished, which is
// what makes a barrier of `SELECT 1` an honest "that write has been versioned".
func startVersionedWarehouse(t *testing.T, mutate func(*config.Config)) *versionedStack {
	t.Helper()
	dsn := os.Getenv("WAREHOUSE_MSSQL_DSN")
	if dsn == "" {
		t.Skip("set WAREHOUSE_MSSQL_DSN (a reachable SQL Server) to run the versioning e2e")
	}
	testsupport.SkipIfSpliceUnsupported(t, dsn)

	emu := entra.StartT(t)
	cfg := &config.Config{
		EntraIssuer:         emu.Origin + "/" + emu.TenantID + "/v2.0",
		SQLTDSAddr:          "127.0.0.1:0",
		WarehouseSQLURL:     dsn,
		WarehouseVersioning: true,
	}
	if mutate != nil {
		mutate(cfg)
	}
	if err := cfg.Finish(); err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(cfg, emu.HTTPClient())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ws := &store.Workspace{DisplayName: "ver-ws"}
	if err := srv.Store.CreateWorkspace(ws, store.Principal{ID: entra.DaemonClientID, Type: "ServicePrincipal"}); err != nil {
		t.Fatal(err)
	}
	wh := &store.Item{WorkspaceID: ws.ID, Type: "Warehouse", DisplayName: "dw"}
	if err := srv.Store.CreateItem(wh, nil); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = srv.TDS.Serve(ln) }()

	token := forgeAppToken(t, emu, "https://database.windows.net")
	connDSN := fmt.Sprintf("server=127.0.0.1;port=%d;database=%s;encrypt=disable;dial timeout=5",
		ln.Addr().(*net.TCPAddr).Port, wh.ID)
	c, err := mssql.NewAccessTokenConnector(connDSN, func() (string, error) { return token, nil })
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(c)
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	var conn *sql.Conn
	// SQL Server may still be starting.
	testsupport.WaitFor(t, 120*time.Second, "connecting to the warehouse", func() bool {
		c, err := db.Conn(ctx)
		if err != nil {
			return false
		}
		if err := c.PingContext(ctx); err != nil {
			_ = c.Close()
			return false
		}
		conn = c
		return true
	})
	t.Cleanup(func() { _ = conn.Close() })
	// Only now: the login token is validated against this same clock, so moving
	// it before the session exists makes a fresh token look expired. The pinned
	// session stays authenticated across everything the tests do to time.
	srv.Clock.Freeze()
	srv.Clock.Advance(400 * 24 * 3600) // clear of the real "now"
	return &versionedStack{srv: srv, conn: conn, wh: wh}
}

// do runs a write and then the barrier.
func (v *versionedStack) do(t *testing.T, q string) {
	t.Helper()
	ctx := context.Background()
	if _, err := v.conn.ExecContext(ctx, q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	var one int
	if err := v.conn.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("barrier: %v", err)
	}
}

func (v *versionedStack) advance(sec int64) { v.srv.Clock.Advance(sec) }

func (v *versionedStack) asOf() string {
	return time.Unix(v.srv.Clock.Now(), 0).UTC().Format("2006-01-02T15:04:05")
}

// ids reads the id column of a query, sorted.
func (v *versionedStack) ids(t *testing.T, q string) ([]int, error) {
	t.Helper()
	rows, err := v.conn.QueryContext(context.Background(), q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int{}
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A Warehouse table has a past: every statement is a version, and the hint
// answers from the one that was current then.
func TestWarehouseTimeTravelOverTheWire(t *testing.T) {
	v := startVersionedWarehouse(t, nil)

	v.do(t, `CREATE TABLE dbo.orders (id int, status varchar(10))`)
	v.advance(3600)
	v.do(t, `INSERT INTO dbo.orders VALUES (1, 'new'), (2, 'new')`)
	afterInsert := v.asOf()
	v.advance(3600)
	v.do(t, `UPDATE dbo.orders SET status = 'done' WHERE id = 1`)
	v.advance(3600)
	v.do(t, `DELETE FROM dbo.orders WHERE id = 2`)
	afterDelete := v.asOf()
	v.advance(3600)
	v.do(t, `INSERT INTO dbo.orders VALUES (3, 'new')`)
	v.advance(3600)
	v.do(t, `TRUNCATE TABLE dbo.orders`)

	q := func(ts string) string {
		return "SELECT id FROM dbo.orders ORDER BY id OPTION (FOR TIMESTAMP AS OF '" + ts + "')"
	}
	for _, c := range []struct {
		name string
		ts   string
		want []int
	}{
		{"after the insert", afterInsert, []int{1, 2}},
		{"after the delete", afterDelete, []int{1}},
	} {
		got, err := v.ids(t, q(c.ts))
		if err != nil || !sameInts(got, c.want) {
			t.Errorf("%s: ids = %v, %v; want %v", c.name, got, err, c.want)
		}
	}
	// The UPDATE is visible as a changed value, not merely as a row count.
	var status string
	if err := v.conn.QueryRowContext(context.Background(),
		"SELECT status FROM dbo.orders WHERE id = 1 OPTION (FOR TIMESTAMP AS OF '"+afterInsert+"')").Scan(&status); err != nil || status != "new" {
		t.Errorf("status of id 1 after the insert = %q, %v; want the pre-update value", status, err)
	}
	// And the present is the present.
	if got, err := v.ids(t, "SELECT id FROM dbo.orders"); err != nil || len(got) != 0 {
		t.Errorf("now = %v, %v; want the truncated table", got, err)
	}
	// Before the table existed is an error, not an empty answer.
	if _, err := v.ids(t, q("2001-01-01T00:00:00")); err == nil {
		t.Error("an instant before the table existed answered instead of failing")
	}
}

// dbt builds a model into a temp table and swaps it in. The swapped-in table's
// history starts at the build; the table it replaced does not leak into it.
func TestWarehouseTimeTravelAcrossADbtSwap(t *testing.T) {
	v := startVersionedWarehouse(t, nil)

	v.do(t, `CREATE TABLE dbo.dim_x__dbt_temp (id int)`)
	v.do(t, `INSERT INTO dbo.dim_x__dbt_temp VALUES (1)`)
	v.advance(60)
	v.do(t, `EXEC sp_rename 'dbo.dim_x__dbt_temp', 'dim_x'`)
	built := v.asOf()
	v.advance(3600)
	v.do(t, `INSERT INTO dbo.dim_x VALUES (2)`)

	got, err := v.ids(t, "SELECT id FROM dbo.dim_x ORDER BY id OPTION (FOR TIMESTAMP AS OF '"+built+"')")
	if err != nil || !sameInts(got, []int{1}) {
		t.Fatalf("as of the build: %v, %v; want [1] under the swapped-in name", got, err)
	}
	// Dropping the table ends its history: nothing is left to travel in.
	v.do(t, `DROP TABLE dbo.dim_x`)
	v.do(t, `CREATE TABLE dbo.dim_x (id int)`)
	if got, err := v.ids(t, "SELECT id FROM dbo.dim_x ORDER BY id OPTION (FOR TIMESTAMP AS OF '"+built+"')"); err == nil {
		t.Errorf("a recreated table answered for its predecessor's past: %v", got)
	}
}

// Past the retention window the instant is refused, naming the window; inside
// it the answer is unchanged.
func TestWarehouseRetentionWindowOverTheWire(t *testing.T) {
	v := startVersionedWarehouse(t, func(c *config.Config) { c.WarehouseRetentionDays = 7 })

	v.do(t, `CREATE TABLE dbo.t (id int)`)
	v.do(t, `INSERT INTO dbo.t VALUES (1)`)
	old := v.asOf()
	v.advance(5 * 24 * 3600)
	v.do(t, `INSERT INTO dbo.t VALUES (2)`)
	recent := v.asOf()
	v.advance(5 * 24 * 3600) // `old` is now 10 days back, `recent` 5

	if got, err := v.ids(t, "SELECT id FROM dbo.t ORDER BY id OPTION (FOR TIMESTAMP AS OF '"+recent+"')"); err != nil || !sameInts(got, []int{1, 2}) {
		t.Errorf("inside the window: %v, %v", got, err)
	}
	_, err := v.ids(t, "SELECT id FROM dbo.t OPTION (FOR TIMESTAMP AS OF '"+old+"')")
	if err == nil || !strings.Contains(err.Error(), "7-day data retention window") {
		t.Errorf("outside the window: err = %v; want the retention refusal", err)
	}
}

// Versioning off is the escape: the statement still works, there is no past.
func TestWarehouseVersioningCanBeTurnedOff(t *testing.T) {
	v := startVersionedWarehouse(t, func(c *config.Config) { c.WarehouseVersioning = false })
	v.do(t, `CREATE TABLE dbo.t (id int)`)
	v.do(t, `INSERT INTO dbo.t VALUES (1)`)
	if _, err := v.srv.Store.GetOneLakePath(v.wh.ID, "Tables/t/_delta_log/00000000000000000000.json"); err == nil {
		t.Error("a Delta log was written with versioning off")
	}
	if got, err := v.ids(t, "SELECT id FROM dbo.t"); err != nil || len(got) != 1 {
		t.Errorf("the table itself: %v, %v", got, err)
	}
}
