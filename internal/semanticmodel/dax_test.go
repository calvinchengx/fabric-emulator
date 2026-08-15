package semanticmodel

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type goldenFile struct {
	// DAXQueryCount is the fixture's own statement of how many dax-handler
	// queries it holds. Read rather than hardcoded here: three suites iterate
	// these and each needs the number, so a literal in each source file is
	// three things to move for one added query — and the way a suite quietly
	// stops covering what it claims to.
	DAXQueryCount int `json:"daxQueryCount"`
	Queries       []struct {
		Name     string `json:"name"`
		DAX      string `json:"dax"`
		Handler  string `json:"handler"`
		Expected struct {
			Columns []string         `json:"columns"`
			Rows    []map[string]any `json:"rows"`
		} `json:"expected"`
	} `json:"queries"`
}

func loadGolden(t *testing.T) goldenFile {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixturesDir(), "golden_queries.json"))
	if err != nil {
		t.Fatal(err)
	}
	var g goldenFile
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func rowsEqualUnordered(got, want []map[string]any) bool {
	if len(got) != len(want) {
		return false
	}
	used := make([]bool, len(got))
	for _, w := range want {
		found := false
		for i, g := range got {
			if used[i] || len(g) != len(w) {
				continue
			}
			match := true
			for k, wv := range w {
				if !valEq(g[k], wv) {
					match = false
					break
				}
			}
			if match {
				used[i] = true
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, v := range m {
		if v != 0 {
			return false
		}
	}
	return true
}

// TestDAXGoldenQueries runs every DAX-handler golden query through the evaluator
// and checks the rows against the hand-computed oracle — the whole point of the
// captured fixtures.
func TestDAXGoldenQueries(t *testing.T) {
	m, d, g := loadModel(t), loadData(t), loadGolden(t)
	ran := 0
	for _, q := range g.Queries {
		if q.Handler != "dax" {
			continue // DMV / schema-rowset asset is a separate handler (deferred)
		}
		ran++
		res, err := Evaluate(m, d, q.DAX)
		if err != nil {
			t.Fatalf("%s: %v", q.Name, err)
		}
		if !sameSet(res.Columns, q.Expected.Columns) {
			t.Errorf("%s: columns = %v, want %v", q.Name, res.Columns, q.Expected.Columns)
		}
		if !rowsEqualUnordered(res.Rows, q.Expected.Rows) {
			t.Errorf("%s: rows mismatch\n got=%v\nwant=%v", q.Name, res.Rows, q.Expected.Rows)
		}
	}
	if ran != g.DAXQueryCount {
		t.Fatalf("ran %d DAX golden queries, fixture declares %d", ran, g.DAXQueryCount)
	}
}

// TestDesktopFunctionGoldens replays Desktop-captured scalar probes against
// the Go evaluator (docs/52 Phase 3). CI never boots msmdsrv; the numbers
// were agreed on a UTM Windows 11 ARM guest + Power BI Desktop.
func TestDesktopFunctionGoldens(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(fixturesDir(), "desktop_goldens.json"))
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		ToleranceRel float64 `json:"toleranceRel"`
		Probes       []struct {
			Name   string  `json:"name"`
			DAX    string  `json:"dax"`
			Column string  `json:"column"`
			Want   float64 `json:"want"`
		} `json:"probes"`
	}
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Probes) == 0 {
		t.Fatal("desktop_goldens.json has no probes")
	}
	if g.ToleranceRel <= 0 {
		g.ToleranceRel = 1e-9
	}
	m, d := loadModel(t), loadData(t)
	for _, p := range g.Probes {
		res, err := Evaluate(m, d, p.DAX)
		if err != nil {
			t.Fatalf("%s: %v", p.Name, err)
		}
		if len(res.Rows) != 1 {
			t.Fatalf("%s: got %d rows, want 1", p.Name, len(res.Rows))
		}
		got := toF(res.Rows[0][p.Column])
		if math.Abs(got-p.Want) > g.ToleranceRel*math.Abs(p.Want) {
			t.Errorf("%s: got %v, want %v (rel %g)", p.Name, got, p.Want, g.ToleranceRel)
		}
	}
}

func TestDAXErrorsAndEdges(t *testing.T) {
	m, d := loadModel(t), loadData(t)

	bad := []string{
		"SELECT 1",               // not EVALUATE
		"EVALUATE 'NoSuchTable'", // unknown table
		`EVALUATE SUMMARIZECOLUMNS('Time'[FiscalYear], "x", [NoMeasure])`, // unknown measure
		"EVALUATE (",             // parse error
		"EVALUATE 'Store' extra", // trailing tokens
		`EVALUATE SUMMARIZECOLUMNS("v", SQRT(-1))`, // Desktop refuses negatives
		`EVALUATE SUMMARIZECOLUMNS("v", MOD(10, 0))`, // Desktop: division by zero
	}
	for _, q := range bad {
		if _, err := Evaluate(m, d, q); err == nil {
			t.Errorf("%q: expected error", q)
		}
	}

	// DIVIDE by zero → blank (nil), which SUMMARIZECOLUMNS keeps only if another
	// output is non-blank; a lone blank output drops the row.
	res, err := Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS('Store'[Territory], "z", DIVIDE([TotalUnits], 0))`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 0 {
		t.Errorf("all-blank groups should be dropped, got %d rows", len(res.Rows))
	}

	// COUNTROWS over an unfiltered table.
	res, err = Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS("n", COUNTROWS(Sales))`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || toF(res.Rows[0]["[n]"]) != 8 {
		t.Errorf("COUNTROWS(Sales) = %v, want 8", res.Rows)
	}
}

// TestDAXMeasureRecursionIsBounded pins the fix for a whole-PROCESS crash.
//
// A measure's expression is re-parsed and evaluated in place, so `M = [M]` — or
// any A→B→A cycle — recursed without end. Go treats a stack overflow as a FATAL
// error: net/http's per-request recover does not catch it, so a single
// executeQueries request killed the emulator and every other in-flight request
// with it. An error is the only acceptable outcome here.
//
// Without the depth bound this test does not fail, it takes the test binary
// down — which is exactly the severity being pinned.
func TestDAXMeasureRecursionIsBounded(t *testing.T) {
	for _, tc := range []struct {
		name     string
		measures []Measure
		query    string
	}{
		{
			name:     "self-referential measure",
			measures: []Measure{{Name: "Loop", Expression: "[Loop]"}},
			query:    `EVALUATE SUMMARIZECOLUMNS("x", [Loop])`,
		},
		{
			name: "mutually recursive measures",
			measures: []Measure{
				{Name: "Ping", Expression: "[Pong]"},
				{Name: "Pong", Expression: "[Ping]"},
			},
			query: `EVALUATE SUMMARIZECOLUMNS("x", [Ping])`,
		},
		{
			name: "cycle reached through an argument",
			measures: []Measure{
				{Name: "Outer", Expression: "DIVIDE([Outer], 2)"},
			},
			query: `EVALUATE SUMMARIZECOLUMNS("x", [Outer])`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, d := loadModel(t), loadData(t)
			m.Tables[0].Measures = append(m.Tables[0].Measures, tc.measures...)
			if _, err := Evaluate(m, d, tc.query); err == nil {
				t.Fatal("a cyclic measure evaluated without error; it must be refused")
			}
		})
	}
}

// TestDAXMeasureNestingStillWorks: the bound refuses cycles without refusing
// legitimate nesting, which is the failure mode a too-tight limit would have.
func TestDAXMeasureNestingStillWorks(t *testing.T) {
	m, d := loadModel(t), loadData(t)
	// A short chain that terminates: Chain0 -> Chain1 -> ... -> a real measure.
	m.Tables[0].Measures = append(m.Tables[0].Measures,
		Measure{Name: "Chain2", Expression: "[TotalUnits]"},
		Measure{Name: "Chain1", Expression: "[Chain2]"},
		Measure{Name: "Chain0", Expression: "[Chain1]"},
	)
	res, err := Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS("x", [Chain0])`)
	if err != nil {
		t.Fatalf("a terminating measure chain was refused: %v", err)
	}
	if len(res.Rows) == 0 {
		t.Fatal("nested measure chain produced no rows")
	}
}

// TestDAXZeroArgFunctionsAreRefused: SUM() and COUNTROWS() parse into an empty
// argument list, and both indexed args[0] before checking arity — so a query a
// client can send panicked the handler goroutine, giving the caller a dropped
// connection instead of the contractual DAXQueryError.
func TestDAXZeroArgFunctionsAreRefused(t *testing.T) {
	m, d := loadModel(t), loadData(t)
	for _, q := range []string{
		`EVALUATE SUMMARIZECOLUMNS("x", SUM())`,
		`EVALUATE SUMMARIZECOLUMNS("x", COUNTROWS())`,
		`EVALUATE SUMMARIZECOLUMNS("x", DIVIDE())`,
	} {
		t.Run(q, func(t *testing.T) {
			if _, err := Evaluate(m, d, q); err == nil {
				t.Fatalf("%s was accepted; want an error, not a panic", q)
			}
		})
	}
}

// TestGoldenFixtureStaysOneRowPerLine guards the fixture's SHAPE, not its data.
//
// The expectations in golden_queries.json are hand-computed, and reviewing a
// change to them means reading the diff. Each expected row is written on one
// line for exactly that reason: a changed value is a one-line diff. Any tool
// that round-trips the file through a JSON pretty-printer explodes every row
// into six lines and turns a one-line change into a 178-line diff that nobody
// can review — which is how this check came to exist.
//
// The invariant is narrow on purpose. Query objects legitimately span lines;
// only entries inside a "rows" array must stay on one.
func TestGoldenFixtureStaysOneRowPerLine(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(fixturesDir(), "golden_queries.json"))
	if err != nil {
		t.Fatal(err)
	}
	inRows := false
	for i, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		if !inRows {
			if k := strings.Index(line, `"rows": [`); k >= 0 && !strings.Contains(line[k+len(`"rows": [`):], "]") {
				inRows = true
			}
			continue
		}
		if trimmed == "]" || trimmed == "]," {
			inRows = false
			continue
		}
		if !strings.HasPrefix(trimmed, "{") || !strings.Contains(trimmed, "}") {
			t.Errorf("golden_queries.json:%d: expected rows are one per line, got %q\n"+
				"A JSON pretty-printer was probably run over the fixture. Restore the "+
				"compact form so a changed expectation stays a one-line diff.", i+1, trimmed)
		}
	}
	if inRows {
		t.Error("golden_queries.json: unterminated \"rows\" array")
	}
}
