package tsql

import (
	"os"
	"path/filepath"
	"testing"
)

// Parses SQL captured from a real dbt-fabric run when TSQL_CORPUS points at a
// directory of .sql files. Skipped by default; a local sanity check against
// statements this package was designed from rather than a CI gate.
func TestParseRealWorldCorpus(t *testing.T) {
	dir := os.Getenv("TSQL_CORPUS")
	if dir == "" {
		t.Skip("set TSQL_CORPUS to a directory of captured .sql files")
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no .sql files in %s (%v)", dir, err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		st, err := Parse(string(b))
		switch {
		case err != nil:
			t.Errorf("%s: %v", filepath.Base(f), err)
		case st == nil:
			t.Logf("%-56s no WITH prefix", filepath.Base(f))
		default:
			t.Logf("%-56s ctes=%d nested=%v", filepath.Base(f), len(st.With.CTEs), st.HasNestedCTE())
		}
	}
}
