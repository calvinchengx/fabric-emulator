package tds

import (
	"context"
	"strings"
	"testing"
)

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

	// The test backend (no base config, e.g. an injected SQLite) is a no-op.
	tb := &sqlServerBackend{}
	if err := tb.EnsureDatabase(context.Background(), "x"); err != nil {
		t.Errorf("test-backend EnsureDatabase should be a no-op: %v", err)
	}
	if tb.DB("x") != nil {
		t.Error("test-backend DB should be the (nil) default")
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
