package server_test

// Contracts 5, 6 and 7 on the warehouse surface (docs/38-framework-conformance.md).
//
// The lakehouse column proves these with notebooks; the warehouse has no
// notebook, so each one is re-expressed as the property the TDS relay actually
// risks getting wrong:
//
//	5  concurrent isolation  — N sessions live AT ONCE, each dialed at its own
//	                           warehouse and logged in as its own principal.
//	                           The relay keeps a per-connection backend session;
//	                           anything it kept process-global leaks here.
//	6  rewrite fall-through  — internal/tds/dialect.go states the same three-way
//	                           contract the delta-rs interception does (rewrite /
//	                           reject / forward), so it is asserted the same way.
//	7  credential lifetime   — a session whose login token ages out mid-run must
//	                           keep working, and an already-expired one must not
//	                           be let in.
//
// Gated on WAREHOUSE_MSSQL_DSN like every other warehouse e2e.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	entra "github.com/calvinchengx/entra-emulator/emulator"
	"github.com/calvinchengx/fabric-emulator/internal/config"
	"github.com/calvinchengx/fabric-emulator/internal/server"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	mssql "github.com/microsoft/go-mssqldb"
)

// confFixture is the shared warehouse stand: one entra, one emulator, one TDS
// listener. Warehouses are created per test, because contract 5 needs several
// and the others need one that no other test is writing to.
type confFixture struct {
	srv  *server.Server
	emu  *entra.Emulator
	ws   *store.Workspace
	addr *net.TCPAddr
}

func newConfFixture(t *testing.T, wsName string) *confFixture {
	t.Helper()
	dsn := os.Getenv("WAREHOUSE_MSSQL_DSN")
	if dsn == "" {
		t.Skip("set WAREHOUSE_MSSQL_DSN (a reachable SQL Server) to run the warehouse conformance e2e")
	}
	if strings.Contains(strings.ToLower(dsn), "np:") || strings.Contains(dsn, `\pipe\`) {
		t.Skip("named-pipe backend: the TDS splice cannot handshake over one")
	}
	emu := entra.StartT(t)
	cfg := &config.Config{
		EntraIssuer:     emu.Origin + "/" + emu.TenantID + "/v2.0",
		SQLTDSAddr:      "127.0.0.1:0",
		WarehouseSQLURL: dsn,
	}
	if err := cfg.Finish(); err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(cfg, emu.HTTPClient())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	if srv.TDS == nil || srv.TDS.Backend == nil {
		t.Fatal("expected a TDS server with a SQL Server backend")
	}
	ws := &store.Workspace{DisplayName: wsName}
	if err := srv.Store.CreateWorkspace(ws, store.Principal{
		ID: entra.DaemonClientID, Type: "ServicePrincipal"}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = srv.TDS.Serve(ln) }()
	return &confFixture{srv: srv, emu: emu, ws: ws, addr: ln.Addr().(*net.TCPAddr)}
}

func (f *confFixture) warehouse(t *testing.T, name string) *store.Item {
	t.Helper()
	wh := &store.Item{WorkspaceID: f.ws.ID, Type: "Warehouse", DisplayName: name}
	if err := f.srv.Store.CreateItem(wh, nil); err != nil {
		t.Fatal(err)
	}
	return wh
}

// open returns a handle pinned to ONE backend connection. MaxOpenConns(1) is
// not decoration: contracts 5 and 7 are both about what one session carries, and
// a pool that silently opened a second connection would answer a different
// question than the one being asked.
func (f *confFixture) open(t *testing.T, whID, token string) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("server=127.0.0.1;port=%d;database=%s;encrypt=disable;dial timeout=5",
		f.addr.Port, whID)
	c, err := mssql.NewAccessTokenConnector(dsn, func() (string, error) { return token, nil })
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(c)
	db.SetMaxOpenConns(1)
	return db
}

// grant gives `oid` a workspace role, so a caller that is not the seeded daemon
// can reach the warehouse at all.
func (f *confFixture) grant(t *testing.T, oid, role string) {
	t.Helper()
	if oid == entra.DaemonClientID {
		return
	}
	if err := f.srv.Store.CreateRoleAssignment(&store.RoleAssignment{
		WorkspaceID: f.ws.ID, Principal: store.Principal{ID: oid, Type: "User"}, Role: role,
	}); err != nil {
		t.Fatal(err)
	}
}

// retryExec absorbs the backend's cold start. The first statement of a run pays
// for database creation and principal provisioning; nothing after it should.
func retryExec(ctx context.Context, db *sql.DB, q string) error {
	var lastErr error
	for i := 0; i < 60; i++ {
		if _, err := db.ExecContext(ctx, q); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return lastErr
}

// ---------------------------------------------------------------- contract 5

// TestConformanceConcurrentIsolation: three sessions live at the same time,
// each dialed at its own warehouse and logged in as its own Entra principal.
//
// ALL THREE CONNECT BEFORE ANY EXECUTES, and that ordering is the test. The
// relay's per-caller backend login exists precisely because a shared or pooled
// one would let identity drift between callers (internal/tds/principal.go says
// so in its own header) — and drift is only possible while two sessions are
// live at once. Connecting, running and closing them one at a time would
// exercise the same code and prove nothing about it.
//
// Each session reports the identity the ENGINE attributes to it, and that is
// compared against what the control plane issued: the warehouse item id it was
// dialed at, and the object id its token carried. A session that answered with
// another's would agree with that other session and disagree with this.
func TestConformanceConcurrentIsolation(t *testing.T) {
	f := newConfFixture(t, "conformance-iso-ws")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	const n = 3
	type child struct {
		marker string
		oid    string
		wh     *store.Item
		db     *sql.DB
	}
	children := make([]*child, n)
	expected := make(map[string]string, n)
	for i := range children {
		c := &child{
			marker: fmt.Sprintf("child%d", i),
			oid:    fmt.Sprintf("00000000-0000-4000-8000-00000000000%d", i),
		}
		c.wh = f.warehouse(t, fmt.Sprintf("wh%d", i))
		f.grant(t, c.oid, "Admin")
		children[i] = c
		expected[c.marker] = c.wh.ID + "/" + c.oid
	}

	// Phase one: every session connected, none of them having run anything.
	for _, c := range children {
		c.db = f.open(t, c.wh.ID, forgeTokenAs(t, f.emu, "https://database.windows.net", c.oid))
		defer c.db.Close()
		// Ping through the retry: connect is where database creation and
		// principal provisioning happen, and on a cold server that is slow.
		if err := retryExec(ctx, c.db, "SELECT 1"); err != nil {
			t.Fatalf("%s could not connect: %v", c.marker, err)
		}
	}

	// Phase two: released together.
	type seen struct {
		marker   string
		identity string
		err      error
	}
	out := make([]seen, n)
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i, c := range children {
		wg.Add(1)
		go func(i int, c *child) {
			defer wg.Done()
			<-release
			table := "dbo.iso_" + c.marker
			if _, err := c.db.ExecContext(ctx,
				"CREATE TABLE "+table+" (marker NVARCHAR(32))"); err != nil {
				out[i] = seen{err: fmt.Errorf("create: %w", err)}
				return
			}
			if _, err := c.db.ExecContext(ctx,
				"INSERT INTO "+table+" VALUES (@p1)", c.marker); err != nil {
				out[i] = seen{err: fmt.Errorf("insert: %w", err)}
				return
			}
			var db, login string
			if err := c.db.QueryRowContext(ctx,
				"SELECT DB_NAME(), SUSER_SNAME()").Scan(&db, &login); err != nil {
				out[i] = seen{err: fmt.Errorf("identity: %w", err)}
				return
			}
			var got string
			if err := c.db.QueryRowContext(ctx,
				"SELECT marker FROM "+table).Scan(&got); err != nil {
				out[i] = seen{err: fmt.Errorf("read back: %w", err)}
				return
			}
			out[i] = seen{marker: got, identity: db + "/" + login}
		}(i, c)
	}
	close(release)
	wg.Wait()

	for i, c := range children {
		got := out[i]
		if got.err != nil {
			t.Fatalf("%s: %v", c.marker, got.err)
		}
		if got.marker != c.marker {
			t.Errorf("the table %s wrote carries marker %q — a session wrote another session's row",
				c.marker, got.marker)
		}
		if got.identity != expected[c.marker] {
			t.Errorf("%s believes it is %q; the control plane issued it %q",
				c.marker, got.identity, expected[c.marker])
		}
	}

	// Distinct identities, stated separately. Three sessions all reporting the
	// SAME one is already caught above as three mismatches; naming it as a
	// collision is what says "they leaked" rather than "they are each wrong".
	ids := map[string]string{}
	for i, c := range children {
		if prev, dup := ids[out[i].identity]; dup {
			t.Errorf("%s and %s share an identity: %q", prev, c.marker, out[i].identity)
		}
		ids[out[i].identity] = c.marker
	}

	// Out of band: a FRESH connection per warehouse, and it must see its own
	// child's table and none of the others. The sessions that wrote are closed
	// out of the question entirely.
	for _, c := range children {
		reader := f.open(t, c.wh.ID, forgeTokenAs(t, f.emu, "https://database.windows.net", c.oid))
		var count int
		if err := reader.QueryRowContext(ctx,
			// `iso[_]%`, not `iso_%`: underscore is a single-character
			// wildcard in T-SQL LIKE, so the unescaped form also matches
			// names that merely start with "iso" — a looser count than the
			// one this assertion claims to be making.
			"SELECT COUNT(*) FROM sys.tables WHERE name LIKE 'iso[_]%'").Scan(&count); err != nil {
			reader.Close()
			t.Fatalf("out-of-band read for %s: %v", c.marker, err)
		}
		reader.Close()
		if count != 1 {
			t.Errorf("%s's warehouse holds %d iso_ tables; each session writes exactly one, "+
				"so anything else is a write that landed in the wrong warehouse", c.marker, count)
		}
	}
}

// ---------------------------------------------------------------- contract 6

// TestConformanceFallThrough: the dialect layer rewrites what it models and
// forwards what it does not, byte for byte.
//
// TWO LEGS, AND BOTH ARE LOAD-BEARING.
//
// The RECOGNISED leg is CTAS. SQL Server has no `CREATE TABLE … AS SELECT`; it
// spells that `SELECT … INTO`. So a CTAS that succeeds here can only have
// succeeded through internal/tsql's rewrite — if it fails, nothing is
// intercepting and the second leg is vacuous, because everything falls through
// when nothing is in the way.
//
// The UNRECOGNISED leg is an ECHO, which is how a single-engine surface proves
// this at all. The lakehouse column contrasts two engines: the statement Sail
// cannot plan must succeed on the JVM. The warehouse has one engine and no
// contrast available, so the witness is the engine returning the bytes it was
// given. The payload is a string literal containing the EXACT nested-CTE
// construct the rewriter does recognise — the most tempting possible input for
// a tokenizer that does not respect quoting. If anything flattened it, the
// echoed text differs. The emulator cannot forge this without reproducing its
// own input verbatim, which is the same thing as not having touched it.
func TestConformanceFallThrough(t *testing.T) {
	f := newConfFixture(t, "conformance-ft-ws")
	wh := f.warehouse(t, "wh")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	db := f.open(t, wh.ID, forgeAppToken(t, f.emu, "https://database.windows.net"))
	defer db.Close()
	if err := retryExec(ctx, db, "SELECT 1"); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Leg one: the construct the grammar DOES model.
	_, _ = db.ExecContext(ctx, "IF OBJECT_ID('dbo.ft_ctas') IS NOT NULL DROP TABLE dbo.ft_ctas")
	if _, err := db.ExecContext(ctx,
		"CREATE TABLE dbo.ft_ctas AS SELECT 1 AS a"); err != nil {
		t.Fatalf("the statement the grammar DOES recognise failed (%v) — with nothing "+
			"intercepting, a fall-through result would prove nothing", err)
	}
	var a int
	if err := db.QueryRowContext(ctx, "SELECT a FROM dbo.ft_ctas").Scan(&a); err != nil {
		t.Fatalf("CTAS reported success; the table it names does not answer: %v", err)
	}
	if a != 1 {
		t.Fatalf("CTAS produced a=%d, want 1", a)
	}
	defer func() { _, _ = db.ExecContext(ctx, "DROP TABLE dbo.ft_ctas") }()

	// Leg two: the same construct, quoted, so it is data and not SQL.
	payload := "WITH a AS (WITH b AS (SELECT 1 x) SELECT x FROM b) SELECT x FROM a"
	var echoed string
	if err := db.QueryRowContext(ctx,
		"SELECT '"+payload+"' AS payload").Scan(&echoed); err != nil {
		t.Fatalf("the echo statement failed: %v", err)
	}
	if echoed != payload {
		t.Fatalf("the engine echoed back text this session did not send:\n sent %q\n  got %q\n"+
			"something rewrote a statement it does not model", payload, echoed)
	}
}

// ---------------------------------------------------------------- contract 7

// TestConformanceCredentialLifetime: a session outlives its login token.
//
// THE WAIT MUST EXCEED THE LIFETIME OR THE TEST IS ORNAMENTAL — a probe that
// naps for less than the token lived passes on a runtime that never re-mints,
// which is the defect itself. So the token is minted with an explicit 60s
// lifetime and the wait is 75s, and both numbers are asserted here rather than
// assumed.
//
// AND SURVIVING ONLY MEANS SOMETHING IF EXPIRY IS ENFORCED. "The session kept
// working past its lifetime" and "nothing ever checks a token" are the same
// green, and the second is a hole wearing the first's result. So the last leg
// presents an ALREADY-EXPIRED token on a fresh connection and requires it to be
// refused. Without that leg this test would pass just as happily against an
// endpoint that validated nothing at all.
func TestConformanceCredentialLifetime(t *testing.T) {
	if testing.Short() {
		t.Skip("this contract is a wait; -short skips it")
	}
	f := newConfFixture(t, "conformance-cred-ws")
	wh := f.warehouse(t, "wh")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const lifetime = 60
	const wait = 75 * time.Second

	db := f.open(t, wh.ID, forgeTokenExpiring(t, f.emu, "https://database.windows.net", lifetime))
	defer db.Close()
	if err := retryExec(ctx, db, "SELECT 1"); err != nil {
		t.Fatalf("connect: %v", err)
	}
	// PIN THE PHYSICAL CONNECTION. The question is whether ONE session survives
	// its token, so the pool must not be free to answer it by opening a new one.
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("pinning a connection: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// The baseline is the control: if this fails, nothing the second operation
	// does says anything either way.
	var one int
	if err := conn.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("the operation BEFORE the wait failed (%v) — with no working "+
			"baseline the second one proves nothing", err)
	}

	start := time.Now()
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(wait):
	}
	slept := time.Since(start)
	if slept <= lifetime*time.Second {
		t.Fatalf("slept %.0fs against a %ds token lifetime — the gap was never opened",
			slept.Seconds(), lifetime)
	}

	if _, err := conn.ExecContext(ctx,
		"CREATE TABLE dbo.cred_after (id INT)"); err != nil {
		t.Fatalf("after %.0fs, past a %ds token lifetime, the session failed: %v",
			slept.Seconds(), lifetime, err)
	}
	defer func() { _, _ = conn.ExecContext(ctx, "DROP TABLE dbo.cred_after") }()

	// The enforcement control. EXPIRED BY TEN MINUTES, not by one second past
	// the lifetime: internal/auth allows a 60s clock skew, as every real JWT
	// validator does, so a token expired by exactly that much sits ON the
	// boundary and is legitimately accepted. Asking whether expiry is enforced
	// has to mean asking outside the allowance — a first draft of this leg used
	// -60 and read the skew as "nothing is checking expiry at all".
	expired := f.open(t, wh.ID, forgeTokenExpiring(t, f.emu, "https://database.windows.net", -600))
	defer expired.Close()
	if err := expired.PingContext(ctx); err == nil {
		t.Fatal("an already-expired token was ACCEPTED, so outliving a lifetime here " +
			"says nothing about refreshing one — nothing is checking expiry at all")
	}
}

// forgeTokenExpiring mints a token with an explicit lifetime, in seconds. A
// NEGATIVE value mints one that is already expired, which is what contract 7's
// enforcement leg presents.
//
// The lifetime is a forge parameter rather than an emulator setting because the
// setting is process-wide: shortening it would shorten every token this test
// holds, including the ones the other legs depend on staying valid.
func forgeTokenExpiring(t *testing.T, emu *entra.Emulator, audience string, seconds int) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"clientId":         entra.DaemonClientID,
		"audience":         audience,
		"expiresInSeconds": seconds,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := emu.HTTPClient().Post(emu.Origin+"/admin/api/tokens", "application/json",
		bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	var tok struct{ AccessToken, Token string }
	if err := json.Unmarshal(out, &tok); err != nil {
		t.Fatalf("forge: %v %s", err, out)
	}
	if tok.AccessToken != "" {
		return tok.AccessToken
	}
	if tok.Token == "" {
		t.Fatalf("forge returned no token: %s", out)
	}
	return tok.Token
}
