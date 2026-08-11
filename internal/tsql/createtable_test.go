package tsql

import "strings"
import "testing"

// Each case is a statement a real Fabric Warehouse REFUSED on 2026-08-11, with
// the tenant's own message quoted in createtable.go. SQL Server accepts all of
// them, which is why the emulator has to say no itself: otherwise the model
// builds locally and fails on the tenant it was written for.
func TestFabricCreateTableRestrictionsAreRefused(t *testing.T) {
	for _, tc := range []struct{ name, sql, want string }{
		{"INT IDENTITY", "CREATE TABLE dbo.t (id INT IDENTITY(1,1), n INT)", "must be BIGINT"},
		{"SMALLINT IDENTITY", "CREATE TABLE t (id SMALLINT IDENTITY(1,1))", "must be BIGINT"},
		{"inline PRIMARY KEY", "CREATE TABLE dbo.t (id INT NOT NULL PRIMARY KEY)", "PRIMARY KEY is not supported"},
		{"NOT ENFORCED PRIMARY KEY",
			"CREATE TABLE dbo.t (id INT NOT NULL, CONSTRAINT pk PRIMARY KEY NONCLUSTERED (id) NOT ENFORCED)",
			"PRIMARY KEY is not supported"},
		{"PRIMARY KEY split by a comment",
			"CREATE TABLE t (id INT PRIMARY /* sneaky */ KEY)", "PRIMARY KEY is not supported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Adapt(tc.sql)
			if err == nil {
				t.Fatalf("accepted a statement Fabric refuses: %s", tc.sql)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error does not name the restriction: %v", err)
			}
		})
	}
}

// The refusal must not fire on statements Fabric accepts — a gate that blocks
// legal DDL is worse than none, because the workaround is to stop using the gate.
func TestLegalCreateTablesArePassedThrough(t *testing.T) {
	for _, sql := range []string{
		"CREATE TABLE dbo.t (id BIGINT IDENTITY(1,1), n INT)", // BIGINT is allowed
		"CREATE TABLE dbo.t (n INT, s VARCHAR(100))",
		// A column whose NAME contains the keyword: token-level matching, not text.
		"CREATE TABLE dbo.t (identity_provider VARCHAR(50), primary_contact VARCHAR(50))",
		// The words inside a string literal and a comment.
		"CREATE TABLE dbo.t (s VARCHAR(50) DEFAULT 'PRIMARY KEY') -- IDENTITY(1,1)",
		// Not a CREATE TABLE at all.
		"CREATE VIEW v AS SELECT 1 AS n",
		"ALTER TABLE dbo.t ADD CONSTRAINT pk PRIMARY KEY NONCLUSTERED (id) NOT ENFORCED",
		"SELECT 1",
	} {
		if _, _, err := Adapt(sql); err != nil {
			t.Errorf("refused a statement Fabric accepts: %s\n  %v", sql, err)
		}
	}
}
