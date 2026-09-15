package tds

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

// CloseSessions ends the live sessions routed to the databases it names, and
// only those: a switch in one workspace must not cut another's.
func TestCloseSessionsEndsOnlyTheNamedDatabases(t *testing.T) {
	be := &fakeBackend{res: &Result{Columns: []Column{{Name: "n"}}, Rows: [][]any{{1}}}}
	srv := &Server{Auth: func(string) error { return nil }, Backend: be,
		OnConnect: func(_ context.Context, _, database, _ string) (Connection, error) {
			return Connection{TargetDB: database}, nil
		}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { _ = srv.Serve(ln) }()
	port := ln.Addr().(*net.TCPAddr).Port
	open := func(database string) (*sql.DB, *sql.Conn) {
		t.Helper()
		c, err := mssql.NewAccessTokenConnector(fmt.Sprintf("server=127.0.0.1;port=%d;database=%s;encrypt=disable;dial timeout=5", port, database),
			func() (string, error) { return "a.b.c", nil })
		if err != nil {
			t.Fatal(err)
		}
		db := sql.OpenDB(c)
		t.Cleanup(func() { _ = db.Close() })
		conn, err := db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.PingContext(context.Background()); err != nil {
			t.Fatal(err)
		}
		return db, conn
	}
	_, lake := open("lake")
	otherDB, other := open("other")

	// Registration happens on the server's goroutine after the login ack.
	deadline := time.Now().Add(2 * time.Second)
	for {
		srv.sessionsMu.Lock()
		n := len(srv.sessions["lake"]) + len(srv.sessions["other"])
		srv.sessionsMu.Unlock()
		if n == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := srv.CloseSessions("lake", "nowhere"); n != 1 {
		t.Fatalf("closed %d sessions, want 1", n)
	}
	var v int
	if err := lake.QueryRowContext(context.Background(), "SELECT 1").Scan(&v); err == nil {
		t.Error("the closed session still answers")
	}
	if err := other.QueryRowContext(context.Background(), "SELECT 1").Scan(&v); err != nil {
		t.Errorf("a session to another database was cut: %v", err)
	}
	// A session that ends on its own leaves the registry.
	_ = other.Close()
	_ = otherDB.Close()
	deadline = time.Now().Add(2 * time.Second)
	for {
		srv.sessionsMu.Lock()
		_, still := srv.sessions["other"]
		srv.sessionsMu.Unlock()
		if !still {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("an ended session stayed registered")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
