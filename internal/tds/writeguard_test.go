package tds

import "testing"

// TestIsWriteStatement covers the read-only guard's classification, including
// firstKeyword's comment/whitespace skipping.
func TestIsWriteStatement(t *testing.T) {
	reads := []string{
		"SELECT 1",
		"  select * from t",
		"\n\t-- a comment\nSELECT x FROM t",
		"WITH q AS (SELECT 1 AS n) SELECT * FROM q",
		"SET NOCOUNT ON",
		"-- a trailing comment with no newline",
		"",
	}
	writes := []string{
		"INSERT INTO t VALUES (1)",
		"update t set x=1",
		"DELETE FROM t",
		"CREATE TABLE t(x int)",
		"DROP TABLE t",
		"ALTER TABLE t ADD y int",
		"TRUNCATE TABLE t",
		"MERGE t USING s ON t.id=s.id WHEN MATCHED THEN UPDATE SET x=1;",
		"exec sp_who",
		"  -- c\n  DROP DATABASE x",
		"/* a block comment */ INSERT INTO t VALUES (1)",
	}
	for _, q := range reads {
		if isWriteStatement(q) {
			t.Errorf("read misclassified as write: %q", q)
		}
	}
	for _, q := range writes {
		if !isWriteStatement(q) {
			t.Errorf("write misclassified as read: %q", q)
		}
	}
}

// A lakehouse's SQL analytics endpoint is read-only for data and the place its
// SQL objects and security are authored.
func TestIsEndpointWrite(t *testing.T) {
	forwarded := []string{
		"SELECT 1",
		"",
		"GRANT SELECT ON dbo.v TO [u]",
		"revoke select on dbo.t from [u]",
		"DENY SELECT ON dbo.t(c) TO [u]",
		"CREATE VIEW dbo.v AS SELECT 1 AS n",
		"CREATE OR ALTER VIEW dbo.v AS SELECT 1 AS n",
		"create or alter function dbo.f() returns int as begin return 1 end",
		"CREATE PROC dbo.p AS SELECT 1",
		"ALTER PROCEDURE dbo.p AS SELECT 2",
		"DROP VIEW dbo.v",
		"CREATE SCHEMA s",
		"CREATE ROLE analysts",
		"ALTER ROLE analysts ADD MEMBER [u]",
		"CREATE USER u WITHOUT LOGIN",
		"CREATE SECURITY POLICY s.p ADD FILTER PREDICATE s.f(c) ON dbo.t",
		"ALTER SECURITY POLICY s.p WITH (STATE = OFF)",
		"ALTER TABLE dbo.t ALTER COLUMN email ADD MASKED WITH (FUNCTION = 'email()')",
		"ALTER TABLE [dbo].[my table] ALTER COLUMN [e mail] DROP MASKED",
		"ALTER TABLE t ALTER COLUMN c ADD MASKED WITH (FUNCTION = 'default()')",
		"/* why */ CREATE VIEW dbo.v AS SELECT 1 AS n",
		"-- only a comment CREATE VIEW",
		"SELECT 'delete me' AS note, [insert] FROM dbo.t",
		"GRANT SELECT ON dbo.v TO [u]; DENY SELECT ON dbo.t TO [x]",
	}
	refused := []string{
		"INSERT INTO t VALUES (1)",
		"UPDATE t SET x = 1",
		"DELETE FROM t",
		"TRUNCATE TABLE t",
		"MERGE t USING s ON 1=1 WHEN MATCHED THEN DELETE;",
		"CREATE TABLE t (x int)",
		"ALTER TABLE t ADD y int",
		"DROP TABLE t",
		"CREATE INDEX i ON t(x)",
		"CREATE",
		"EXEC sp_who",
		"-- create view\nCREATE TABLE t (x int)",
		"/* create view */ DROP TABLE t",
		"/* note */ INSERT INTO t VALUES (1)",
		"-- note\n/* note */\nDELETE FROM t",
		"GRANT SELECT ON dbo.v TO [u] INSERT INTO t VALUES (1)",
		"SELECT 1; DROP TABLE t",
		"CREATE VIEW dbo.v AS SELECT 1 AS n; CREATE TABLE t (x int)",
		"ALTER TABLE t ADD maskedcol int",
		"ALTER TABLE t ALTER COLUMN c int",
		"ALTER TABLE t ALTER COLUMN c ADD MASKED WITH (FUNCTION = 'default()') ALTER TABLE t ADD y int",
		"/* unterminated CREATE VIEW",
		"SELECT 'unterminated",
		"ALTER TABLE",
		"ALTER TABLE dbo.",
		"ALTER TABLE (x int)",
		"ALTER TABLE dbo.t ALTER COLUMN",
	}
	for _, q := range forwarded {
		if isEndpointWrite(q) {
			t.Errorf("refused on the endpoint: %q", q)
		}
	}
	for _, q := range refused {
		if !isEndpointWrite(q) {
			t.Errorf("forwarded on the endpoint: %q", q)
		}
	}
	// Comments cannot smuggle a forwarded statement's words in front of a refused one.
	for _, q := range []string{"-- create view\nCREATE TABLE t (x int)", "/* create view */ DROP TABLE t"} {
		if !isEndpointWrite(q) {
			t.Errorf("a comment changed the verdict: %q", q)
		}
	}
	if got := keywords("  /* a */ -- b\n ALTER(TABLE", 5); len(got) != 2 || got[0] != "ALTER" || got[1] != "TABLE" {
		t.Errorf("keywords = %v", got)
	}
	for q, want := range map[string]int{"-- only a line": 0, "/* unterminated": 0, "SELECT /* x */ 1": 2} {
		if got := keywords(q, 5); len(got) != want {
			t.Errorf("keywords(%q) = %v", q, got)
		}
	}
}
