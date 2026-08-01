package tsql

import (
	"encoding/json"
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

// Emits {original, flattened} pairs as JSON for the captured corpus when
// TSQL_FLATTEN_OUT is set, so the pairs can be executed against a real engine
// (the T6f witness). Skipped by default.
func TestDumpFlattenedCorpus(t *testing.T) {
	dir, out := os.Getenv("TSQL_CORPUS"), os.Getenv("TSQL_FLATTEN_OUT")
	if dir == "" || out == "" {
		t.Skip("set TSQL_CORPUS and TSQL_FLATTEN_OUT")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.sql"))
	type pair struct {
		Name, Original, Flattened string
		Changed                   bool
	}
	var pairs []pair
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		fl, changed, err := Flatten(string(b))
		if err != nil {
			t.Fatalf("%s: %v", filepath.Base(f), err)
		}
		pairs = append(pairs, pair{filepath.Base(f), string(b), fl, changed})
	}
	blob, err := json.MarshalIndent(pairs, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d pairs to %s", len(pairs), out)
}
