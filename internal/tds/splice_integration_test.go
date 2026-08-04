package tds

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strings"
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
type countingBackend struct{ queries []string }

func (c *countingBackend) Query(_ context.Context, q string) (*Result, error) {
	c.queries = append(c.queries, q)
	return &Result{Columns: []Column{{Name: "x", Type: ColInt}}, Rows: [][]any{{int64(1)}}}, nil
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
	if len(be.queries) != 0 {
		t.Fatalf("backend ran %d query/queries (%q) for an unauthorized session; want 0",
			len(be.queries), be.queries)
	}
	if authorized {
		t.Fatal("OnConnect ran for an empty database; it cannot authorize one")
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
