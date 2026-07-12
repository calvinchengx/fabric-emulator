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
