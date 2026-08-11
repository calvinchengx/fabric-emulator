package tds

import (
	"strings"
	"testing"
)

// The whole point of collation.go: a per-item database must be created with
// Fabric's collation, not the SQL Server container's. Measured on a real tenant
// 2026-08-11 — the warehouse reports Latin1_General_100_BIN2_UTF8 (case-SENSITIVE)
// while the emulator's server collation is SQL_Latin1_General_CP1_CI_AS, so
// `information_schema.tables` lowercase worked here and was an invalid object
// name there.
func TestDatabasesAreCreatedWithFabricsCollation(t *testing.T) {
	b := &sqlServerBackend{}
	if got := b.collationFor("any"); got != CollationCaseSensitive {
		t.Fatalf("default collation = %q, want Fabric's %q", got, CollationCaseSensitive)
	}
}

func TestADeclaredCollationIsHonoured(t *testing.T) {
	b := &sqlServerBackend{CollationOf: func(string) string { return CollationCaseInsensitive }}
	if got := b.collationFor("wh"); got != CollationCaseInsensitive {
		t.Fatalf("declared collation ignored: %q", got)
	}
}

// An unrecognised value must not reach the DDL: an invalid COLLATE clause fails
// CREATE DATABASE outright, and a warehouse that cannot be created is a worse
// answer than one created case-sensitive.
func TestAnUnknownCollationFallsBackRatherThanFailingTheCreate(t *testing.T) {
	for _, bad := range []string{"", "Latin1_General_CI_AS", "'; DROP DATABASE x --", "utf8mb4_general_ci"} {
		b := &sqlServerBackend{CollationOf: func(string) string { return bad }}
		if got := b.collationFor("wh"); got != CollationCaseSensitive {
			t.Errorf("collationFor(%q) = %q, want the default", bad, got)
		}
	}
}

// The COLLATE clause is interpolated into DDL, so only values from the fixed set
// may ever reach it — checked here rather than trusted, because collationFor is
// the only thing standing between CollationOf and a CREATE DATABASE statement.
func TestOnlyFabricsTwoCollationsAreValid(t *testing.T) {
	if !ValidCollation(CollationCaseSensitive) || !ValidCollation(CollationCaseInsensitive) {
		t.Fatal("Fabric's own two collations must validate")
	}
	for _, bad := range []string{"", "SQL_Latin1_General_CP1_CI_AS", "Latin1_General_100_BIN2"} {
		if ValidCollation(bad) {
			t.Errorf("ValidCollation(%q) = true", bad)
		}
	}
	for _, c := range []string{CollationCaseSensitive, CollationCaseInsensitive} {
		if strings.ContainsAny(c, " ;'\"[]-") {
			t.Errorf("collation %q contains a character that is unsafe to interpolate", c)
		}
	}
}
