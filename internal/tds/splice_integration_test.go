package tds

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

// fakeSpliceBackend is a SpliceBackend whose "engine" is an in-memory pipe the
// test drives directly, so the whole splice path (handle → Dial → forwarded
// login response → spliceSession) is exercised without a real SQL Server.
type fakeSpliceBackend struct {
	engine    net.Conn
	loginResp []byte
	dialErr   error
}

func (f *fakeSpliceBackend) Query(context.Context, string) (*Result, error) {
	return nil, fmt.Errorf("unused")
}

func (f *fakeSpliceBackend) Dial(context.Context, string) (net.Conn, []byte, error) {
	if f.dialErr != nil {
		return nil, nil, f.dialErr
	}
	return f.engine, f.loginResp, nil
}

// TestSpliceEndToEnd drives a real go-mssqldb client through the splice path: it
// logs in over FedAuth, the server forwards the (fake) engine's login response,
// and a query is byte-forwarded to the engine, which answers over the pipe.
func TestSpliceEndToEnd(t *testing.T) {
	engineServer, engineTest := net.Pipe()
	be := &fakeSpliceBackend{
		engine:    engineServer,
		loginResp: concat(loginAck(), done(doneFinal, 0)),
	}
	srv := &Server{
		Auth:    func(string) error { return nil },
		Backend: be,
		OnConnect: func(context.Context, string, string, string) (string, bool, error) {
			return "item-db", false, nil
		},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go srv.Serve(ln)

	// The engine end: answer the first forwarded batch with a single int = 7.
	go func() {
		defer engineTest.Close()
		if _, _, err := ReadMessage(engineTest); err != nil {
			return
		}
		_ = WriteMessage(engineTest, PktTabular, intResult(7))
	}()

	addr := ln.Addr().(*net.TCPAddr)
	dsn := fmt.Sprintf("server=127.0.0.1;port=%d;database=lh;encrypt=disable;dial timeout=5", addr.Port)
	c, err := mssql.NewAccessTokenConnector(dsn, func() (string, error) { return "a.b.c", nil })
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(c)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var got int
	if err := db.QueryRowContext(ctx, "select 7").Scan(&got); err != nil {
		t.Fatalf("query through splice: %v", err)
	}
	if got != 7 {
		t.Fatalf("splice returned %d, want 7", got)
	}
}

// TestServerHandshakeErrors covers the handshake type guards: a wrong first
// packet (not PRELOGIN), and a wrong second packet (not LOGIN7).
func TestServerHandshakeErrors(t *testing.T) {
	srv := &Server{Auth: func(string) error { return nil }}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go srv.Serve(ln)
	addr := ln.Addr().String()
	buf := make([]byte, 8)

	// First packet is not PRELOGIN → the handler errors and closes.
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	_ = WriteMessage(c, PktSQLBatch, []byte{0, 0, 0, 0})
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	_, _ = c.Read(buf)
	c.Close()

	// PRELOGIN accepted, then a non-LOGIN7 second packet → errors and closes.
	c2, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	_ = WriteMessage(c2, PktPreLogin, []byte{0xFF})
	if _, _, err := ReadMessage(c2); err != nil {
		t.Fatal(err)
	}
	_ = WriteMessage(c2, PktSQLBatch, []byte{0, 0, 0, 0})
	_ = c2.SetReadDeadline(time.Now().Add(time.Second))
	_, _ = c2.Read(buf)
	c2.Close()

	// A connection that closes before sending anything → ReadMessage error.
	if c3, err := net.Dial("tcp", addr); err == nil {
		c3.Close()
	}

	// PRELOGIN accepted, then a malformed (too short) LOGIN7 → ParseLogin7 error.
	c4, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	_ = WriteMessage(c4, PktPreLogin, []byte{0xFF})
	if _, _, err := ReadMessage(c4); err != nil {
		t.Fatal(err)
	}
	_ = WriteMessage(c4, PktLogin7, []byte{0, 1, 2})
	_ = c4.SetReadDeadline(time.Now().Add(time.Second))
	_, _ = c4.Read(buf)
	c4.Close()
}

// roBackend is a plain (non-splice) Backend that answers every read with a
// single int = 1 — enough to exercise the re-encode fallback path.
type roBackend struct{}

func (roBackend) Query(context.Context, string) (*Result, error) {
	return &Result{Columns: []Column{{Name: "x", Type: ColInt}}, Rows: [][]any{{int64(1)}}}, nil
}

// TestReEncodeReadOnly covers the re-encode relay's read-only surface: reads are
// answered, a write is rejected with a read-only error (no splice backend).
func TestReEncodeReadOnly(t *testing.T) {
	srv := &Server{
		Auth:    func(string) error { return nil },
		Backend: roBackend{},
		OnConnect: func(context.Context, string, string, string) (string, bool, error) {
			return "db", true, nil // read-only surface
		},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go srv.Serve(ln)
	addr := ln.Addr().(*net.TCPAddr)
	dsn := fmt.Sprintf("server=127.0.0.1;port=%d;database=lh;encrypt=disable;dial timeout=5", addr.Port)
	c, _ := mssql.NewAccessTokenConnector(dsn, func() (string, error) { return "a.b.c", nil })
	db := sql.OpenDB(c)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var got int
	if err := db.QueryRowContext(ctx, "select 1").Scan(&got); err != nil || got != 1 {
		t.Fatalf("read on read-only surface: got=%d err=%v", got, err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO t VALUES (1)"); err == nil {
		t.Fatal("write on a read-only surface should be rejected")
	} else if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("write error = %q; want a read-only rejection", err)
	}
}

// TestOnConnectRejects: an OnConnect error rejects the login.
func TestOnConnectRejects(t *testing.T) {
	srv := &Server{
		Auth:    func(string) error { return nil },
		Backend: roBackend{},
		OnConnect: func(context.Context, string, string, string) (string, bool, error) {
			return "", false, fmt.Errorf("access denied: no role")
		},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go srv.Serve(ln)
	addr := ln.Addr().(*net.TCPAddr)
	dsn := fmt.Sprintf("server=127.0.0.1;port=%d;database=x;encrypt=disable;dial timeout=5", addr.Port)
	c, _ := mssql.NewAccessTokenConnector(dsn, func() (string, error) { return "a.b.c", nil })
	db := sql.OpenDB(c)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err == nil {
		t.Fatal("login should be rejected when OnConnect denies access")
	}
}

// countingBackend records every query it is asked to run, so a test can assert
// that an unauthorized session reached the engine ZERO times — "the login
// failed" alone would not prove the batch never executed.
// Guarded by a mutex: the server writes these from its connection goroutine
// while the test reads them, which the race detector rightly refuses.
type countingBackend struct {
	mu      sync.Mutex
	queries []string
}

func (c *countingBackend) Query(_ context.Context, q string) (*Result, error) {
	c.mu.Lock()
	c.queries = append(c.queries, q)
	c.mu.Unlock()
	return &Result{Columns: []Column{{Name: "x", Type: ColInt}}, Rows: [][]any{{int64(1)}}}, nil
}

func (c *countingBackend) ran() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.queries...)
}

// TestLoginWithoutDatabaseIsRejected pins the RBAC wall on the TDS surface.
//
// OnConnect is the ONLY place a TDS connection's workspace role and read-only
// flag are decided. It used to be skipped when the client sent no database name,
// which left readOnly=false and targetDB="" — and an empty targetDB also fails
// the splice gate, so the session fell through to the re-encode relay, where
// DB("") is the backend's DEFAULT pool under the emulator's privileged
// credential. A token carrying no role on any workspace could therefore run
// arbitrary read/write T-SQL against the warehouse, which real Fabric refuses.
//
// The assertion is deliberately two-sided: the login must fail AND the backend
// must never be asked to run anything. Checking only the first would still pass
// if the batch executed and the error arrived afterwards.
func TestLoginWithoutDatabaseIsRejected(t *testing.T) {
	be := &countingBackend{}
	authorized := false
	srv := &Server{
		Auth:    func(string) error { return nil }, // a valid token, no workspace role
		Backend: be,
		OnConnect: func(context.Context, string, string, string) (string, bool, error) {
			authorized = true
			return "db", false, nil
		},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go srv.Serve(ln)

	// No `database=` in the DSN: LOGIN7 carries an empty database name.
	addr := ln.Addr().(*net.TCPAddr)
	dsn := fmt.Sprintf("server=127.0.0.1;port=%d;encrypt=disable;dial timeout=5", addr.Port)
	c, _ := mssql.NewAccessTokenConnector(dsn, func() (string, error) { return "a.b.c", nil })
	db := sql.OpenDB(c)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var got int
	err = db.QueryRowContext(ctx, "SELECT 1").Scan(&got)
	if err == nil {
		t.Fatal("a login with no database ran a query: the RBAC wall was skipped")
	}
	if ran := be.ran(); len(ran) != 0 {
		t.Fatalf("backend ran %d query/queries (%q) for an unauthorized session; want 0",
			len(ran), ran)
	}
	if authorized {
		t.Fatal("OnConnect ran for an empty database; it cannot authorize one")
	}
}

// countingSpliceBackend is both a Backend and a SpliceBackend, so a test can
// tell WHICH relay a session took: Dial means the splice path, Query means the
// re-encode relay.
type countingSpliceBackend struct {
	engine    net.Conn
	loginResp []byte

	mu      sync.Mutex
	dialed  int
	queries []string
}

func (c *countingSpliceBackend) Query(_ context.Context, q string) (*Result, error) {
	c.mu.Lock()
	c.queries = append(c.queries, q)
	c.mu.Unlock()
	return &Result{Columns: []Column{{Name: "x", Type: ColInt}}, Rows: [][]any{{int64(1)}}}, nil
}

func (c *countingSpliceBackend) Dial(context.Context, string) (net.Conn, []byte, error) {
	c.mu.Lock()
	c.dialed++
	c.mu.Unlock()
	return c.engine, c.loginResp, nil
}

func (c *countingSpliceBackend) counts() (int, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dialed, append([]string(nil), c.queries...)
}

// TestConfiguredAuthorizerNeverReachesTheReEncodeRelay pins the STRUCTURAL
// property behind the empty-database fix, not just the symptom.
//
// Production sets OnConnect only where the backend is a SpliceBackend
// (server.go wires both inside the same `WarehouseSQLURL != ""` block), and
// warehouseRouter returns an item GUID, never an empty string. So once an empty
// database is rejected, every accepted session has a non-empty targetDB and
// satisfies the splice gate — the re-encode relay, whose DB("") is the
// backend's DEFAULT pool, becomes unreachable.
//
// That is the invariant worth locking: if a future change reintroduces a path
// where an authorized-but-unrouted session falls through to the relay, Query
// gets called and this fails.
func TestConfiguredAuthorizerNeverReachesTheReEncodeRelay(t *testing.T) {
	engineServer, engineTest := net.Pipe()
	defer engineTest.Close()
	be := &countingSpliceBackend{
		engine:    engineServer,
		loginResp: concat(loginAck(), done(doneFinal, 0)),
	}
	srv := &Server{
		Auth:    func(string) error { return nil },
		Backend: be,
		OnConnect: func(context.Context, string, string, string) (string, bool, error) {
			return "item-db", false, nil
		},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go srv.Serve(ln)
	addr := ln.Addr().(*net.TCPAddr)

	// The rejected session: no database, so the login must fail outright.
	noDB := fmt.Sprintf("server=127.0.0.1;port=%d;encrypt=disable;dial timeout=5", addr.Port)
	c1, _ := mssql.NewAccessTokenConnector(noDB, func() (string, error) { return "a.b.c", nil })
	db1 := sql.OpenDB(c1)
	ctx1, cancel1 := context.WithTimeout(context.Background(), 5*time.Second)
	if err := db1.PingContext(ctx1); err == nil {
		t.Error("a login with no database was accepted")
	}
	cancel1()
	db1.Close()

	// The accepted session: it must take the splice path. Ping runs in the
	// background because the fake engine never answers the post-login traffic;
	// the test waits on `dialed` — the thing it actually asserts — rather than
	// on a fixed sleep.
	withDB := fmt.Sprintf("server=127.0.0.1;port=%d;database=wh;encrypt=disable;dial timeout=5", addr.Port)
	c2, _ := mssql.NewAccessTokenConnector(withDB, func() (string, error) { return "a.b.c", nil })
	db2 := sql.OpenDB(c2)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	go func() { _ = db2.PingContext(ctx2) }()
	defer db2.Close()
	var dialed int
	var ran []string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if dialed, ran = be.counts(); dialed > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	dialed, ran = be.counts()
	if len(ran) != 0 {
		t.Fatalf("the re-encode relay ran %d query/queries (%q); with an authorizer "+
			"configured no session may reach it", len(ran), ran)
	}
	if dialed == 0 {
		t.Fatal("the authorized session never reached the splice path")
	}
}

// TestReEncodeRelayRejectsStrictStatement covers the relay's dialect guard,
// which was never exercised: a construct real Fabric refuses must be refused
// here too, rather than forwarded to the sidecar that would happily run it.
// The splice path has this guard tested; the re-encode path did not.
func TestReEncodeRelayRejectsStrictStatement(t *testing.T) {
	be := &countingBackend{}
	srv := &Server{
		Auth:    func(string) error { return nil },
		Backend: be,
		Strict:  true,
		OnConnect: func(context.Context, string, string, string) (string, bool, error) {
			return "db", false, nil
		},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go srv.Serve(ln)
	addr := ln.Addr().(*net.TCPAddr)
	dsn := fmt.Sprintf("server=127.0.0.1;port=%d;database=db;encrypt=disable;dial timeout=5", addr.Port)
	c, _ := mssql.NewAccessTokenConnector(dsn, func() (string, error) { return "a.b.c", nil })
	db := sql.OpenDB(c)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = db.ExecContext(ctx, "CREATE TRIGGER trg ON t AFTER INSERT AS SELECT 1")
	if err == nil {
		t.Fatal("a construct Fabric rejects was accepted by the re-encode relay")
	}
	if !strings.Contains(err.Error(), "trigger") {
		t.Fatalf("rejection = %q; want it to name the unsupported construct", err)
	}
	if ran := be.ran(); len(ran) != 0 {
		t.Fatalf("backend ran %q; a rejected statement must never reach the engine", ran)
	}
}

// captureFedAuthLogin7 returns the LOGIN7 payload a real go-mssqldb client
// sends, so a test can replay it on a raw connection and drive the post-login
// protocol directly. Hand-rolling a FedAuth LOGIN7 would test our idea of the
// driver's bytes rather than the driver's actual bytes.
func captureFedAuthLogin7(t *testing.T, database string) []byte {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	out := make(chan []byte, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := ReadMessage(conn); err != nil { // PRELOGIN
			return
		}
		if err := WriteMessage(conn, PktTabular, ServerPreLogin(true)); err != nil {
			return
		}
		if _, data, err := ReadMessage(conn); err == nil { // LOGIN7
			out <- data
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	dsn := fmt.Sprintf("server=127.0.0.1;port=%d;database=%s;encrypt=disable;dial timeout=5",
		addr.Port, database)
	c, _ := mssql.NewAccessTokenConnector(dsn, func() (string, error) { return "a.b.c", nil })
	db := sql.OpenDB(c)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = db.PingContext(ctx) }()
	select {
	case data := <-out:
		return data
	case <-ctx.Done():
		t.Fatal("never captured a LOGIN7 from the driver")
		return nil
	}
}

// batchPayload builds a SQLBatch body: a 4-byte ALL_HEADERS length of 4 (an
// empty header block) followed by the UCS2 statement, which is what
// sqlBatchQuery parses.
func batchPayload(sql string) []byte {
	hdr := make([]byte, 4)
	binary.LittleEndian.PutUint32(hdr, 4)
	return append(hdr, str2ucs2(sql)...)
}

// TestReEncodeRelaySkipsNonBatchMessages: the relay answers PktSQLBatch and
// skips every other message type. The guarantee worth pinning is that skipping
// one does not derail the loop — the NEXT batch is still answered.
//
// Driven over a raw connection rather than through the driver: a client whose
// RPC goes unanswered just waits for its deadline, which is slow and asserts a
// hang rather than a recovery.
func TestReEncodeRelaySkipsNonBatchMessages(t *testing.T) {
	login7 := captureFedAuthLogin7(t, "db")
	be := &countingBackend{}
	srv := &Server{
		Auth:    func(string) error { return nil },
		Backend: be,
		OnConnect: func(context.Context, string, string, string) (string, bool, error) {
			return "db", false, nil
		},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go srv.Serve(ln)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := WriteMessage(conn, PktPreLogin, []byte{0xFF}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadMessage(conn); err != nil { // PRELOGIN response
		t.Fatal(err)
	}
	if err := WriteMessage(conn, PktLogin7, login7); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadMessage(conn); err != nil { // login response
		t.Fatal(err)
	}

	// An ATTENTION is not a batch: skipped, with no reply and no disconnect.
	if err := WriteMessage(conn, PktAttention, nil); err != nil {
		t.Fatal(err)
	}
	// The loop must still be running: this batch is answered.
	if err := WriteMessage(conn, PktSQLBatch, batchPayload("select 1")); err != nil {
		t.Fatal(err)
	}
	typ, _, err := ReadMessage(conn)
	if err != nil {
		t.Fatalf("a non-batch message derailed the relay loop: %v", err)
	}
	if typ != PktTabular {
		t.Fatalf("reply type = %#x; want PktTabular", typ)
	}
	if ran := be.ran(); len(ran) != 1 || ran[0] != "select 1" {
		t.Fatalf("backend ran %q; want exactly the one batch", ran)
	}
}

// TestSpliceDialRejectsLogin: when the backend can't be dialed, the login is
// rejected (not silently accepted).
func TestSpliceDialRejectsLogin(t *testing.T) {
	srv := &Server{
		Auth:    func(string) error { return nil },
		Backend: &fakeSpliceBackend{dialErr: fmt.Errorf("engine down")},
		OnConnect: func(context.Context, string, string, string) (string, bool, error) {
			return "item-db", false, nil
		},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go srv.Serve(ln)

	addr := ln.Addr().(*net.TCPAddr)
	dsn := fmt.Sprintf("server=127.0.0.1;port=%d;database=lh;encrypt=disable;dial timeout=5", addr.Port)
	c, _ := mssql.NewAccessTokenConnector(dsn, func() (string, error) { return "a.b.c", nil })
	db := sql.OpenDB(c)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err == nil {
		t.Fatal("login succeeded despite the backend being undialable")
	}
}

// TestLoginWithNoValidatorIsRejected: a server built without a token validator
// must refuse every login, not accept every token.
//
// The guard used to be `if s.Auth != nil`, which failed OPEN: a construction
// mistake turned the surface — read/write T-SQL against the warehouse — into an
// unauthenticated one. Production always wires Auth, so this only fires on
// misconfiguration, which is precisely when a security check should be loudest.
// api.withAuth answers "not configured" for the same reason.
func TestLoginWithNoValidatorIsRejected(t *testing.T) {
	be := &countingBackend{}
	srv := &Server{ // deliberately no Auth
		Backend: be,
		OnConnect: func(context.Context, string, string, string) (string, bool, error) {
			return "db", false, nil
		},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go srv.Serve(ln)
	addr := ln.Addr().(*net.TCPAddr)
	dsn := fmt.Sprintf("server=127.0.0.1;port=%d;database=db;encrypt=disable;dial timeout=5", addr.Port)
	c, _ := mssql.NewAccessTokenConnector(dsn, func() (string, error) { return "any.token.at.all", nil })
	db := sql.OpenDB(c)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var got int
	if err := db.QueryRowContext(ctx, "select 1").Scan(&got); err == nil {
		t.Fatal("a server with no validator accepted a token and ran a query")
	}
	if ran := be.ran(); len(ran) != 0 {
		t.Fatalf("backend ran %q with no validator configured; want nothing", ran)
	}
}
