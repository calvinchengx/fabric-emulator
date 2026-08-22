package tds

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"

	_ "github.com/microsoft/go-mssqldb"
)

// The pieces that decide WHO a caller is, unit-tested without an engine. The
// end-to-end proof that SQL Server then restricts them belongs to
// ci:warehouse-tds; these are the parts that would be wrong in a way an
// integration test reports as "the policy did nothing".

func TestPrincipalPasswordIsDerivedAndStable(t *testing.T) {
	a := principalPassword("11111111-1111-1111-1111-111111111111")
	// Stable: a reconnect must find the login it made last time, so the same
	// id has to produce the same password every process, every restart.
	if a != principalPassword("11111111-1111-1111-1111-111111111111") {
		t.Fatal("the same object id produced two passwords")
	}
	if a == principalPassword("22222222-2222-2222-2222-222222222222") {
		t.Fatal("two object ids share a password")
	}
	// The object id must not be recoverable from, or contained in, the
	// credential — a password that embeds the caller is not a credential.
	if strings.Contains(a, "1111") {
		t.Fatalf("the password embeds the object id: %q", a)
	}
	// Valid even if a deployment turns CHECK_POLICY back on.
	if len(a) < 12 || !strings.ContainsAny(a, "!") || a[0] < 'A' {
		t.Fatalf("password %q would fail a complexity policy", a)
	}
}

// A `]` in an identifier would close the bracket quote and turn the rest into
// SQL. Unlikely in an Entra object id and catastrophic if it ever happened.
func TestPrincipalNameEscapesTheQuoteCharacter(t *testing.T) {
	if got := principalName("ab]cd"); got != "ab]]cd" {
		t.Fatalf("principalName = %q, want the bracket doubled", got)
	}
	if got := principalName("plain-id"); got != "plain-id" {
		t.Fatalf("principalName mangled an ordinary id: %q", got)
	}
}

// db_owner would carry CONTROL, which implies UNMASK — a writer would see
// through every masked column and the emulator would look like it enforced
// masking while enforcing nothing.
func TestWritersDoNotGetOwnership(t *testing.T) {
	rights := principalRights(RoleWriter)
	for _, r := range rights {
		if r == "db_owner" || r == "db_securityadmin" {
			t.Fatalf("a writer was granted %s, which bypasses masking and CLS", r)
		}
	}
	// They do still need to read, write and create tables: dbt builds a
	// warehouse by issuing DDL.
	for _, need := range []string{"db_datareader", "db_datawriter", "db_ddladmin"} {
		if !contains(rights, need) {
			t.Errorf("a writer lacks %s", need)
		}
	}
}

// An owner must be able to AUTHOR policy — CREATE SECURITY POLICY, ADD MASKED
// WITH and GRANT all need rights a writer does not have. The first e2e run
// failed on all three with "User does not have permission to perform this
// action", which is what this rung exists to prevent.
func TestOwnersCanAuthorPolicy(t *testing.T) {
	rights := principalRights(RoleOwner)
	if len(rights) != 1 || rights[0] != "db_owner" {
		t.Fatalf("owner rights = %v, want db_owner", rights)
	}
}

func TestReadersGetOnlyRead(t *testing.T) {
	rights := principalRights(RoleReader)
	if len(rights) != 1 || rights[0] != "db_datareader" {
		t.Fatalf("reader rights = %v, want db_datareader alone", rights)
	}
}

func TestEnsurePrincipalRefusesAnEmptyCaller(t *testing.T) {
	// No id means no principal to be, and connecting as "nobody" would fall
	// back to the relay's own sysadmin account — the exact bypass this exists
	// to remove.
	if err := EnsurePrincipal(t.Context(), nil, nil, "", RoleWriter); err == nil {
		t.Fatal("an empty object id was accepted")
	}
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// --- against a real engine ----------------------------------------------------
//
// The SQL above is only meaningful to SQL Server, so these are gated on a live
// one exactly as the rest of the warehouse suite is. What they prove is the part
// a unit test cannot: that the statements are accepted, that a reconnect finds
// the same principal, and — the point of the whole increment — that connecting
// as the caller produces a DIFFERENT identity from the relay's own account.

func liveDBs(t *testing.T) (master *sql.DB, target *sql.DB, dbName string) {
	t.Helper()
	dsn := os.Getenv("WAREHOUSE_MSSQL_DSN")
	if dsn == "" {
		t.Skip("set WAREHOUSE_MSSQL_DSN (a reachable SQL Server) to run the principal tests")
	}
	master, err := sql.Open("sqlserver", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = master.Close() })

	dbName = "princ_" + strings.ReplaceAll(uuidish(), "-", "")
	if _, err := master.Exec("CREATE DATABASE [" + dbName + "]"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = master.Exec("ALTER DATABASE [" + dbName + "] SET SINGLE_USER WITH ROLLBACK IMMEDIATE")
		_, _ = master.Exec("DROP DATABASE [" + dbName + "]")
	})
	target, err = sql.Open("sqlserver", dsn+";database="+dbName)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = target.Close() })
	return master, target, dbName
}

func uuidish() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func TestEnsurePrincipalIsIdempotentAndDistinguishing(t *testing.T) {
	master, target, dbName := liveDBs(t)
	ctx := t.Context()
	alice := "aaaaaaaa-0000-0000-0000-" + uuidish() + "0000"

	if err := EnsurePrincipal(ctx, master, target, alice, RoleReader); err != nil {
		t.Fatalf("first provision: %v", err)
	}
	// A reconnect is the normal case, so running it twice must not error.
	if err := EnsurePrincipal(ctx, master, target, alice, RoleReader); err != nil {
		t.Fatalf("second provision: %v", err)
	}

	var n int
	if err := target.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sys.database_principals WHERE name = @p1", alice).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("database principals named %s: %d, want exactly 1", alice, n)
	}

	// THE POINT: connecting as the caller is a different identity from the
	// relay's own account. Without this, every policy is written against a
	// sysadmin and enforces nothing.
	dsn := os.Getenv("WAREHOUSE_MSSQL_DSN")
	asAlice, err := sql.Open("sqlserver", fmt.Sprintf("%s;database=%s;user id=%s;password=%s",
		stripCreds(dsn), dbName, alice, principalPassword(alice)))
	if err != nil {
		t.Fatal(err)
	}
	defer asAlice.Close()

	var who string
	if err := asAlice.QueryRowContext(ctx, "SELECT USER_NAME()").Scan(&who); err != nil {
		t.Fatalf("connecting as the caller: %v", err)
	}
	if who != alice {
		t.Fatalf("USER_NAME() = %q, want %q", who, alice)
	}
	var isSysadmin int
	if err := asAlice.QueryRowContext(ctx, "SELECT IS_SRVROLEMEMBER('sysadmin')").Scan(&isSysadmin); err != nil {
		t.Fatal(err)
	}
	if isSysadmin != 0 {
		t.Fatal("the caller is a sysadmin, which bypasses RLS, CLS and masking")
	}
}

// stripCreds removes the DSN's own user id/password so the caller's can be used.
func stripCreds(dsn string) string {
	out := []string{}
	for _, part := range strings.Split(dsn, ";") {
		low := strings.ToLower(strings.TrimSpace(part))
		if strings.HasPrefix(low, "user id=") || strings.HasPrefix(low, "password=") ||
			strings.HasPrefix(low, "database=") {
			continue
		}
		if strings.TrimSpace(part) != "" {
			out = append(out, part)
		}
	}
	return strings.Join(out, ";")
}

// THE PAYOFF, and the reason this increment adds no policy engine: with callers
// to restrict, SQL Server's own RLS, CLS and masking all apply at once. If this
// fails, the design in docs/55 is wrong and no amount of emulator code fixes it.
//
// TWO CALLERS, ONE QUERY, DIFFERENT ANSWERS. The relay's own account is a poor
// control for RLS — a filter predicate is not bypassed by sysadmin, so it sees
// neither row and proves nothing. A second provisioned caller is the honest
// comparison, and it is also the shape the product has.
func TestSQLServerRestrictsTheProvisionedCaller(t *testing.T) {
	master, target, dbName := liveDBs(t)
	ctx := t.Context()
	alice := "aaaaaaaa-1111-0000-0000-" + uuidish() + "0000"
	bob := "bbbbbbbb-2222-0000-0000-" + uuidish() + "0000"
	for _, who := range []string{alice, bob} {
		if err := EnsurePrincipal(ctx, master, target, who, RoleReader); err != nil {
			t.Fatal(err)
		}
	}

	for _, stmt := range []string{
		`CREATE TABLE dbo.sales (owner_name sysname, amount int, secret varchar(20))`,
		`ALTER TABLE dbo.sales ALTER COLUMN secret ADD MASKED WITH (FUNCTION = 'default()')`,
		`INSERT INTO dbo.sales VALUES (N'` + alice + `', 10, 'topsecret'), (N'` + bob + `', 20, 'topsecret')`,
		`CREATE SCHEMA sec`,
		`CREATE FUNCTION sec.fn_owner(@owner sysname) RETURNS TABLE WITH SCHEMABINDING
		   AS RETURN SELECT 1 AS ok WHERE @owner = USER_NAME()`,
		`CREATE SECURITY POLICY sec.sales_policy
		   ADD FILTER PREDICATE sec.fn_owner(owner_name) ON dbo.sales WITH (STATE = ON)`,
		`GRANT SELECT ON dbo.sales TO [` + alice + `]`,
		`GRANT SELECT ON dbo.sales TO [` + bob + `]`,
		// CLS on alice only, so bob is the control for the column too.
		`DENY SELECT ON dbo.sales(amount) TO [` + alice + `]`,
		// UNMASK on bob only, so he is the control for masking.
		`GRANT UNMASK TO [` + bob + `]`,
	} {
		if _, err := target.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", strings.Split(stmt, "\n")[0], err)
		}
	}

	dsn := os.Getenv("WAREHOUSE_MSSQL_DSN")
	open := func(who string) *sql.DB {
		t.Helper()
		db, err := sql.Open("sqlserver", fmt.Sprintf("%s;database=%s;user id=%s;password=%s",
			stripCreds(dsn), dbName, who, principalPassword(who)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	asAlice, asBob := open(alice), open(bob)

	// RLS: each caller sees only their own row, from the same query.
	for who, db := range map[string]*sql.DB{alice: asAlice, bob: asBob} {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(owner_name) FROM dbo.sales").Scan(&n); err != nil {
			t.Fatalf("%s select: %v", who[:8], err)
		}
		if n != 1 {
			t.Errorf("RLS: %s sees %d rows, want 1 of 2", who[:8], n)
		}
		var owner string
		if err := db.QueryRowContext(ctx, "SELECT owner_name FROM dbo.sales").Scan(&owner); err != nil {
			t.Fatalf("%s owner: %v", who[:8], err)
		}
		if owner != who {
			t.Errorf("RLS: %s sees the row of %s", who[:8], owner[:8])
		}
	}

	// CLS: denied for alice, readable for bob — the same column, same query.
	if _, err := asAlice.QueryContext(ctx, "SELECT amount FROM dbo.sales"); err == nil {
		t.Error("CLS: alice read a column denied to her")
	}
	var amount int
	if err := asBob.QueryRowContext(ctx, "SELECT amount FROM dbo.sales").Scan(&amount); err != nil {
		t.Errorf("CLS: bob was denied a column nobody denied him: %v", err)
	}

	// DDM: masked for alice, plain for bob, who holds UNMASK.
	var masked, plain string
	if err := asAlice.QueryRowContext(ctx, "SELECT secret FROM dbo.sales").Scan(&masked); err != nil {
		t.Fatalf("alice masked select: %v", err)
	}
	if err := asBob.QueryRowContext(ctx, "SELECT secret FROM dbo.sales").Scan(&plain); err != nil {
		t.Fatalf("bob unmasked select: %v", err)
	}
	if masked == plain {
		t.Errorf("DDM: both callers read %q — masking did not apply", masked)
	}
	if plain != "topsecret" {
		t.Errorf("the UNMASK holder read %q, want the real value", plain)
	}
	t.Logf("RLS: 1 row each. CLS: alice denied, bob %d. DDM: %q vs %q", amount, masked, plain)
}

// A provisioning failure must surface, not be swallowed: a caller silently
// left unprovisioned would fall back to the relay's own account, which is the
// bypass this whole file exists to close.
func TestEnsurePrincipalReportsEngineFailures(t *testing.T) {
	master, target, _ := liveDBs(t)
	ctx := t.Context()
	// A name SQL Server will refuse: the login statement cannot be built from it.
	bad := strings.Repeat("x", 200)
	if err := EnsurePrincipal(ctx, master, target, bad, RoleWriter); err == nil {
		t.Fatal("an unusable principal name was reported as provisioned")
	}
}
