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

// TestServerFedAuthLogin drives the real go-mssqldb client end to end: a
// PRELOGIN → FedAuth LOGIN7 → LOGINACK login. A token the Authenticator accepts
// lets the connection complete (Ping succeeds); a rejected token fails the
// login. This is the FedAuth-termination milestone — the emulator authenticates
// a real SQL client with an Entra-style token over TDS.
func TestServerFedAuthLogin(t *testing.T) {
	const good = "good.jwt.token"
	srv := &Server{Auth: func(tok string) error {
		if tok != good {
			return fmt.Errorf("token not trusted")
		}
		return nil
	}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go srv.Serve(ln)

	addr := ln.Addr().(*net.TCPAddr)
	dsn := fmt.Sprintf("server=127.0.0.1;port=%d;encrypt=disable;dial timeout=5", addr.Port)

	ping := func(token string) error {
		c, err := mssql.NewAccessTokenConnector(dsn, func() (string, error) { return token, nil })
		if err != nil {
			return err
		}
		db := sql.OpenDB(c)
		defer db.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return db.PingContext(ctx)
	}

	if err := ping(good); err != nil {
		t.Fatalf("ping with accepted token: %v", err)
	}
	if err := ping("bad.jwt.token"); err == nil {
		t.Fatal("ping with rejected token should fail")
	}
}

// TestServerQuerySelect1 runs a real query through the driver and scans the
// result — the endpoint answers SELECT 1 with 1 over the result-token stream.
func TestServerQuerySelect1(t *testing.T) {
	srv := &Server{Auth: func(string) error { return nil }}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go srv.Serve(ln)
	addr := ln.Addr().(*net.TCPAddr)
	dsn := fmt.Sprintf("server=127.0.0.1;port=%d;encrypt=disable;dial timeout=5", addr.Port)

	c, err := mssql.NewAccessTokenConnector(dsn, func() (string, error) { return "a.b.c", nil })
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(c)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var got int
	if err := db.QueryRowContext(ctx, "select 1").Scan(&got); err != nil {
		t.Fatalf("query: %v", err)
	}
	if got != 1 {
		t.Fatalf("SELECT 1 returned %d", got)
	}
}

// TestServerRejectsNonFedAuth: a plain SQL login (no FedAuth token) is rejected.
func TestServerRejectsNonFedAuth(t *testing.T) {
	srv := &Server{Auth: func(string) error { return nil }}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go srv.Serve(ln)
	addr := ln.Addr().(*net.TCPAddr)

	// A SQL-login DSN (user/password) presents no FedAuth token → login rejected.
	dsn := fmt.Sprintf("server=127.0.0.1;port=%d;user id=sa;password=x;encrypt=disable;dial timeout=5", addr.Port)
	db, err := sql.Open("sqlserver", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err == nil {
		t.Fatal("SQL login without a federated token should be rejected")
	}
}
