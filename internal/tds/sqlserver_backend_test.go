package tds

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
)

// TestDialSuccess covers the splice backend's dial + login handshake against a
// fake engine listener (no real SQL Server): Dial connects, logs in, and returns
// the engine's login-response tokens.
func TestDialSuccess(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	fakeEngine(ln, EncryptNotSup, concat(loginAck(), done(doneFinal, 0)))
	port := ln.Addr().(*net.TCPAddr).Port

	be, err := NewSQLServerBackend(fmt.Sprintf("sqlserver://sa:pw@127.0.0.1:%d?database=base&dial timeout=5", port))
	if err != nil {
		t.Fatal(err)
	}
	conn, login, err := be.Dial(context.Background(), "itemdb")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	if len(login) == 0 {
		t.Error("Dial returned an empty login response")
	}
}

// TestBackendPerDatabase covers the per-item database routing: pool caching,
// the context-threaded database, EnsureDatabase's name guard, and the
// test-backend (no base config) no-op paths. No SQL Server needed — pools open
// lazily and the connection error is enough to exercise the DDL path.
func TestBackendPerDatabase(t *testing.T) {
	be, err := NewSQLServerBackend("sqlserver://sa:x@127.0.0.1:1?database=base&dial timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	// Per-item pools are created lazily and cached; distinct items get distinct pools.
	p1 := be.DB("item-1")
	if p1 == nil {
		t.Fatal("DB returned nil")
	}
	if be.DB("item-1") != p1 {
		t.Error("per-item pool not cached")
	}
	if be.DB("item-2") == p1 {
		t.Error("distinct items share a pool")
	}
	// EnsureDatabase rejects an unsafe name before touching the network.
	if err := be.EnsureDatabase(context.Background(), "bad;name"); err == nil {
		t.Error("unsafe database name accepted")
	}
	// A safe name reaches the DDL exec (which errors — no server — but is covered).
	if err := be.EnsureDatabase(context.Background(), "safe-name"); err == nil {
		t.Error("expected a connection error from the DDL exec")
	}

	// Dial reaches the backend login handshake (which errors — no server — but the
	// dial+login path is covered).
	if _, _, err := be.Dial(context.Background(), "item-1"); err == nil {
		t.Error("expected a connect/login error from Dial with no server")
	}
	// A DSN with no explicit port exercises Dial's default-1433 branch (still
	// errors — nothing is listening — but the path is covered).
	noPort, err := NewSQLServerBackend("sqlserver://sa:x@127.0.0.1?database=base&dial timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := noPort.Dial(context.Background(), "item"); err == nil {
		t.Error("expected a connect error dialing the default port with no server")
	}

	// The test backend (no base config, e.g. an injected SQLite) is a no-op.
	tb := &sqlServerBackend{}
	if err := tb.EnsureDatabase(context.Background(), "x"); err != nil {
		t.Errorf("test-backend EnsureDatabase should be a no-op: %v", err)
	}
	if tb.DB("x") != nil {
		t.Error("test-backend DB should be the (nil) default")
	}
	// Dial on a backend with no base DSN is a clear error, not a panic.
	if _, _, err := tb.Dial(context.Background(), "x"); err == nil {
		t.Error("Dial without a base DSN should error")
	}
}

func TestWithDatabaseCtx(t *testing.T) {
	if got := dbFromCtx(withDatabase(context.Background(), "abc")); got != "abc" {
		t.Errorf("ctx db = %q, want abc", got)
	}
	if got := dbFromCtx(context.Background()); got != "" {
		t.Errorf("empty ctx db = %q", got)
	}
}

func TestSafeDBName(t *testing.T) {
	for _, s := range []string{"1c59edee-8643-44d5-b9a5-23b49b9fd4c2", "abc_123", "X"} {
		if !safeDBName(s) {
			t.Errorf("valid name rejected: %q", s)
		}
	}
	for _, s := range []string{"", "a b", "a;b", "a'b", "a]b", "naïve"} {
		if safeDBName(s) {
			t.Errorf("invalid name accepted: %q", s)
		}
	}
	if safeDBName(strings.Repeat("a", 129)) {
		t.Error("overlong name accepted")
	}
}
