// Package testsupport gives warehouse-facing tests a REAL SQL Server.
//
// These tests used to inject an in-memory SQLite handle wherever production
// uses SQL Server. That made them fast and portable and it made them prove very
// little: SQLite is not T-SQL, and the two backends had already diverged in
// ways the tests could not see. The clearest example is the one that prompted
// this — reflection built INSERT ... VALUES text because that is all SQLite can
// take, and the fixed 500-row batch that made a 100,000 x 100 table take over
// ten minutes went unnoticed for as long as the only backend under test was one
// that never had a bulk protocol to use. The double could not have caught it.
//
// So the double is gone. A test that exercises the warehouse now talks to SQL
// Server or does not run, and CI is what guarantees it runs: see the note on
// the coverage job in .github/workflows/ci.yml.
package testsupport

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"
	// Registers the "np" (named pipe) protocol, and only on Windows — the
	// package's init is a no-op elsewhere. Windows CI reaches SQL Server LocalDB
	// this way, which is already on the runner image; there is no Linux
	// container available there to run mssql/server in.
	_ "github.com/microsoft/go-mssqldb/namedpipe"
)

// DSNEnv is the one environment variable that decides whether warehouse tests
// run. It is the convention already used by the e2e-style tests in
// internal/server, so there is one switch, not two.
const DSNEnv = "WAREHOUSE_MSSQL_DSN"

// OpenMSSQL returns a handle to a FRESH, uniquely named database on the server
// named by WAREHOUSE_MSSQL_DSN, and drops it when the test finishes.
//
// A database per test rather than a shared one: these tests create, drop and
// reflect tables by name, and sharing a database would make them order- and
// parallelism-dependent in a way that only shows up intermittently.
//
// Skips — rather than fails — when the variable is unset, so `go test ./...` on
// a developer machine with no sidecar still runs everything else.
func OpenMSSQL(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv(DSNEnv)
	if dsn == "" {
		t.Skipf("set %s (a reachable SQL Server) to run the warehouse tests", DSNEnv)
	}

	master, err := sql.Open("sqlserver", dsn)
	if err != nil {
		t.Fatalf("opening %s: %v", DSNEnv, err)
	}
	defer master.Close()
	if err := master.Ping(); err != nil {
		t.Fatalf("%s is set but the server is unreachable: %v", DSNEnv, err)
	}

	name := dbName(t)
	if _, err := master.Exec("CREATE DATABASE " + quote(name)); err != nil {
		t.Fatalf("creating test database %s: %v", name, err)
	}

	db, err := sql.Open("sqlserver", withDatabase(dsn, name))
	if err != nil {
		t.Fatalf("opening test database %s: %v", name, err)
	}
	t.Cleanup(func() {
		db.Close()
		drop, err := sql.Open("sqlserver", dsn)
		if err != nil {
			return
		}
		defer drop.Close()
		// SINGLE_USER WITH ROLLBACK IMMEDIATE: a pooled connection may still be
		// closing, and DROP DATABASE fails while any session is attached.
		_, _ = drop.Exec("ALTER DATABASE " + quote(name) +
			" SET SINGLE_USER WITH ROLLBACK IMMEDIATE")
		_, _ = drop.Exec("DROP DATABASE " + quote(name))
	})
	return db
}

// DSN returns the raw DSN CI set, for the few tests that need to feed it to a
// production constructor rather than open it here. Skips like OpenMSSQL does,
// so a caller can use either without a second gate.
func DSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(DSNEnv)
	if dsn == "" {
		t.Skipf("set %s (a reachable SQL Server) to run the warehouse tests", DSNEnv)
	}
	return dsn
}

// dbName derives a legal, unique database name from the test's own name, so a
// leaked database says which test leaked it.
func dbName(t *testing.T) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '_'
		}
	}, t.Name())
	if len(safe) > 80 {
		safe = safe[:80]
	}
	return fmt.Sprintf("t_%s_%d", safe, os.Getpid())
}

func quote(name string) string {
	return "[" + strings.ReplaceAll(name, "]", "]]") + "]"
}

// WithDatabase rewrites a DSN to point at `name`, PRESERVING everything else
// about it. Exported because tests outside this package need it and the
// hand-rolled alternative is a trap: rebuilding a DSN from a parsed Config's
// Host and Port silently drops a named pipe, which lives in ProtocolParameters
// and leaves Host empty. That produced "sqlserver://sa:pw@?database=..." on the
// Windows leg — no host, no pipe — and a SQL Browser lookup for an empty
// instance. Rewrite the DSN you were given; never reconstruct one.
func WithDatabase(dsn, name string) string { return withDatabase(dsn, name) }

// withDatabase rewrites a DSN to point at `name`. Both DSN shapes the driver
// accepts are handled: the URL form and the ADO/keyword form the repo's CI job
// actually uses ("server=...;user id=...;...").
func withDatabase(dsn, name string) string {
	if strings.HasPrefix(dsn, "sqlserver://") {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		return dsn + sep + "database=" + name
	}
	out := dsn
	if !strings.HasSuffix(out, ";") {
		out += ";"
	}
	return out + "database=" + name
}

// Assert the driver is linked in. Without this the blank imports above could be
// dropped by a well-meaning tidy and the failure would be a confusing
// "unknown driver" at run time rather than a build error.
var _ = mssql.Driver{}
