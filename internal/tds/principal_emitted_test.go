package tds

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"
	"sync"
	"testing"
)

// The SQL these functions emit is SQL SYNTAX built from names that are data: an
// Entra object id, a OneLake role, a shortcut table, a column. Escaping is the
// only thing between a hostile name and the statement, and the ledger entries
// for them in docs/security-footguns.json say the guard is "the entry PLUS the
// Go tests that pin the emitted text". These are those tests. They need no
// engine: a recording driver captures the batch each function sends.

type recordingDriver struct {
	mu    sync.Mutex
	execs []string
}

func (d *recordingDriver) Open(string) (driver.Conn, error) { return &recordingConn{d: d}, nil }

type recordingConn struct{ d *recordingDriver }

func (c *recordingConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *recordingConn) Close() error                        { return nil }
func (c *recordingConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }
func (c *recordingConn) ExecContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	c.d.mu.Lock()
	defer c.d.mu.Unlock()
	c.d.execs = append(c.d.execs, q)
	return driver.RowsAffected(0), nil
}

type recordingConnector struct{ d *recordingDriver }

func (c recordingConnector) Connect(context.Context) (driver.Conn, error) {
	return &recordingConn{d: c.d}, nil
}
func (c recordingConnector) Driver() driver.Driver { return c.d }

// recorded returns a DB whose only effect is to remember what it was asked to run.
func recorded(t *testing.T) (*sql.DB, func() string) {
	t.Helper()
	d := &recordingDriver{}
	db := sql.OpenDB(recordingConnector{d})
	t.Cleanup(func() { _ = db.Close() })
	return db, func() string {
		d.mu.Lock()
		defer d.mu.Unlock()
		if len(d.execs) != 1 {
			t.Fatalf("expected exactly one batch, got %d", len(d.execs))
		}
		return d.execs[0]
	}
}

func mustContain(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("emitted SQL is missing %q\n%s", w, got)
		}
	}
}

func mustNotContain(t *testing.T, got string, bad ...string) {
	t.Helper()
	for _, b := range bad {
		if strings.Contains(got, b) {
			t.Errorf("emitted SQL contains the unescaped %q\n%s", b, got)
		}
	}
}

// An object id that closes both quoting contexts: `]` ends a bracketed
// identifier, `'` ends an N'...' literal.
const hostileID = "ab]c'd"

func TestRevokePrincipalEscapesTheObjectIDInBothQuotingContexts(t *testing.T) {
	db, got := recorded(t)
	if err := EnsurePrincipal(t.Context(), nil, db, hostileID, RoleNone); err != nil {
		t.Fatal(err)
	}
	sqlText := got()
	// Literal: ' -> ''. Identifier: ] -> ]]. The two are NOT interchangeable.
	mustContain(t, sqlText,
		"WHERE name = N'ab]c''d'",
		"IS_ROLEMEMBER(N'db_datareader', N'ab]c''d') = 1 ALTER ROLE [db_datareader] DROP MEMBER [ab]]c'd];",
		"REVOKE CONNECT FROM [ab]]c'd];")
	mustNotContain(t, sqlText, "[ab]c", "N'ab]c'd'")
}

func TestSyncOneLakeMembershipsEscapesPrincipalAndRoleNames(t *testing.T) {
	db, got := recorded(t)
	if err := SyncOneLakeMemberships(t.Context(), db, hostileID, []string{"OLS_a]'b"}); err != nil {
		t.Fatal(err)
	}
	sqlText := got()
	mustContain(t, sqlText,
		"DATABASE_PRINCIPAL_ID(N'OLS_a]''b')",
		"IS_ROLEMEMBER(N'OLS_a]''b', N'ab]c''d') = 0",
		"ALTER ROLE [OLS_a]]'b] ADD MEMBER [ab]]c'd];",
		"NOT IN (N'', N'OLS_a]''b')",
		"WHERE m.name = N'ab]c''d'")
	mustNotContain(t, sqlText, "[OLS_a]'b]", "[ab]c'd]")
}

func TestSyncShortcutAccessEscapesTableColumnAndPrincipalNames(t *testing.T) {
	db, got := recorded(t)
	const table = "t]x'y"
	err := SyncShortcutAccess(t.Context(), db, hostileID,
		[]string{table, "plain"}, []string{"plain"},
		map[string][]string{table: {"C'Z"}})
	if err != nil {
		t.Fatal(err)
	}
	sqlText := got()
	mustContain(t, sqlText,
		// The table, as a literal handed to OBJECT_ID: ident first (] doubled),
		// then the whole identifier as a literal (' doubled).
		"OBJECT_ID(N'[dbo].[t]]x''y]', N'U')",
		// The principal in a REVOKE/DENY, bracket-quoted with ] doubled.
		"REVOKE SELECT ON [dbo].[t]]x'y] FROM [ab]]c'd];",
		"DENY SELECT ON [dbo].[plain] TO [ab]]c'd];",
		// The same table spliced into dynamic SQL inside a literal.
		"N'REVOKE SELECT ON [dbo].[t]]x''y] ('",
		"N'DENY SELECT ON [dbo].[t]]x''y] ('",
		// The allow-list: lower-cased, ' doubled.
		"NOT IN (N'', N'c''z')")
	mustNotContain(t, sqlText, "[dbo].[t]x", "N'c'z'", "[ab]c'd]")
}

func TestSyncShortcutAccessRefusesWhatItCannotSync(t *testing.T) {
	db, _ := recorded(t)
	if err := SyncShortcutAccess(t.Context(), db, "", []string{"t"}, nil, nil); err == nil {
		t.Error("synced shortcut access for no principal")
	}
	if err := SyncShortcutAccess(t.Context(), db, "u", nil, nil, nil); err != nil {
		t.Errorf("no tables is nothing to do, got %v", err)
	}
}
