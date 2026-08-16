package semanticmodel

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
		ProbeCount   int     `json:"probeCount"`
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
	// Cardinality, declared separately from the array — the same guard
	// TestDAXGoldenQueries gets from DAXQueryCount. Without it this test passes
	// whether it compares 175 probes or 5: a probe silently dropped by a bad
	// merge (this fixture is edited on many branches at once, and has already
	// collided once) reads as a pass, because every probe that REMAINS still
	// agrees. Assert the count the capture actually produced.
	if g.ProbeCount == 0 {
		t.Fatal("desktop_goldens.json declares no probeCount — add it, or a dropped probe is invisible")
	}
	if len(g.Probes) != g.ProbeCount {
		t.Fatalf("fixture holds %d probes, declares probeCount %d — a probe was added or lost without updating the count",
			len(g.Probes), g.ProbeCount)
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
		`EVALUATE SUMMARIZECOLUMNS("v", POWER(0, 0))`, // Desktop refuses 0^0
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

// TestDAXMinAverageCountBlankColumns: goldens only pin non-blank columns.
// Desktop MIN/AVERAGE of an all-blank column is BLANK (not 0); COUNT of that
// set is 0; COUNT still tallies non-blank text; empty/whitespace cells are
// skipped the same way as nil.
func TestDAXMinAverageCountBlankColumns(t *testing.T) {
	m := &Model{Name: "x", Tables: []Table{{
		Name: "T",
		Columns: []Column{
			{Name: "n", DataType: "int64"},
			{Name: "t", DataType: "string"},
		},
	}}}
	d := Data{"T": {
		{"n": nil, "t": "west"},
		{"n": "", "t": "  "},
	}}

	res, err := Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS("z", MIN(T[n]), "k", 1)`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || res.Rows[0]["[z]"] != nil {
		t.Errorf("MIN of all-blank = %v, want BLANK", res.Rows)
	}

	res, err = Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS("z", AVERAGE(T[n]), "k", 1)`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || res.Rows[0]["[z]"] != nil {
		t.Errorf("AVERAGE of all-blank = %v, want BLANK", res.Rows)
	}

	res, err = Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS("n", COUNT(T[n]))`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || toF(res.Rows[0]["[n]"]) != 0 {
		t.Errorf("COUNT of all-blank = %v, want 0", res.Rows)
	}

	res, err = Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS("n", COUNT(T[t]))`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || toF(res.Rows[0]["[n]"]) != 1 {
		t.Errorf("COUNT of text = %v, want 1 (only %q is non-blank)", res.Rows, "west")
	}
}

// TestDAXPowerBlankAndDomainErrors: Desktop POWER(BLANK, n) is BLANK,
// POWER(n, BLANK) is n^0 = 1. Goldens pin the finite cases and POWER(0, 0);
// the NaN/Inf domain (negative root, 0^-1) is only an error path here.
func TestDAXPowerBlankAndDomainErrors(t *testing.T) {
	m, d := loadModel(t), loadData(t)

	res, err := Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS("z", POWER(DIVIDE(1, 0), 2), "k", 1)`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || res.Rows[0]["[z]"] != nil {
		t.Errorf("POWER(BLANK, 2) = %v, want BLANK", res.Rows)
	}

	res, err = Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS("v", POWER(2, DIVIDE(1, 0)))`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || toF(res.Rows[0]["[v]"]) != 1 {
		t.Errorf("POWER(2, BLANK) = %v, want 1", res.Rows)
	}

	for _, q := range []string{
		`EVALUATE SUMMARIZECOLUMNS("v", POWER(-1, 0.5))`, // NaN
		`EVALUATE SUMMARIZECOLUMNS("v", POWER(0, -1))`,   // Inf
		`EVALUATE SUMMARIZECOLUMNS("v", POWER(2))`,
	} {
		if _, err := Evaluate(m, d, q); err == nil {
			t.Errorf("%q: expected error", q)
		}
	}
}

// TestDAXMinAverageCountArity: empty and non-column arguments used to be
// reachable as a panic (same class as SUM()/COUNTROWS()). AVERAGE of text
// must error rather than skip into a confident mean.
func TestDAXMinAverageCountArity(t *testing.T) {
	m, d := loadModel(t), loadData(t)
	for _, q := range []string{
		`EVALUATE SUMMARIZECOLUMNS("v", MIN())`,
		`EVALUATE SUMMARIZECOLUMNS("v", AVERAGE())`,
		`EVALUATE SUMMARIZECOLUMNS("v", COUNT())`,
		`EVALUATE SUMMARIZECOLUMNS("v", MIN(1))`,
		`EVALUATE SUMMARIZECOLUMNS("v", AVERAGE(1))`,
		`EVALUATE SUMMARIZECOLUMNS("v", COUNT(1))`,
		`EVALUATE SUMMARIZECOLUMNS("v", AVERAGE('Store'[Territory]))`,
		`EVALUATE SUMMARIZECOLUMNS("v", MIN(NoTable[n]))`,
	} {
		if _, err := Evaluate(m, d, q); err == nil {
			t.Errorf("%q: expected error", q)
		}
	}
}

// TestDAXTimeBlankAndNegative: TIME goldens pin wrap-around; they do not
// pin BLANK parts (coerce to 0) or a negative total (error). HOUR(BLANK)
// stays BLANK — the same datePart path YEAR/MONTH/DAY already use.
func TestDAXTimeBlankAndNegative(t *testing.T) {
	m, d := loadModel(t), loadData(t)

	res, err := Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS("v", HOUR(TIME(DIVIDE(1, 0), 0, 0)))`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || toF(res.Rows[0]["[v]"]) != 0 {
		t.Errorf("HOUR(TIME(BLANK, 0, 0)) = %v, want 0", res.Rows)
	}

	res, err = Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS("z", HOUR(DIVIDE(1, 0)), "k", 1)`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || res.Rows[0]["[z]"] != nil {
		t.Errorf("HOUR(BLANK) = %v, want BLANK", res.Rows)
	}

	if _, err := Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS("v", TIME(-1, 0, 0))`); err == nil {
		t.Error("TIME(-1, 0, 0): expected error")
	}
}

// TestDAXPhase3BatchEdges covers BLANK / error / arity paths the Desktop
// goldens do not pin. Those probes are the happy scalars; Windows total
// coverage still has to see the refusal and BLANK branches of the Phase 3
// batch helpers (SWITCH, WEEKDAY, TRUNC, QUOTIENT, ISBLANK, and kin).
func TestDAXPhase3BatchEdges(t *testing.T) {
	m, d := loadModel(t), loadData(t)

	blank := []struct {
		q    string
		name string
	}{
		{`EVALUATE SUMMARIZECOLUMNS("z", SWITCH(3, 1, 10, 2, 20), "k", 1)`, "SWITCH miss without else"},
		{`EVALUATE SUMMARIZECOLUMNS("z", SQRT(DIVIDE(1, 0)), "k", 1)`, "SQRT(BLANK)"},
		{`EVALUATE SUMMARIZECOLUMNS("z", MOD(DIVIDE(1, 0), 3), "k", 1)`, "MOD(BLANK, 3)"},
		{`EVALUATE SUMMARIZECOLUMNS("z", FLOOR(DIVIDE(1, 0), 1), "k", 1)`, "FLOOR(BLANK, 1)"},
		{`EVALUATE SUMMARIZECOLUMNS("z", CEILING(DIVIDE(1, 0), 1), "k", 1)`, "CEILING(BLANK, 1)"},
		{`EVALUATE SUMMARIZECOLUMNS("z", WEEKDAY(DIVIDE(1, 0)), "k", 1)`, "WEEKDAY(BLANK)"},
		{`EVALUATE SUMMARIZECOLUMNS("z", WEEKNUM(DIVIDE(1, 0)), "k", 1)`, "WEEKNUM(BLANK)"},
		{`EVALUATE SUMMARIZECOLUMNS("z", EOMONTH(DIVIDE(1, 0), 0), "k", 1)`, "EOMONTH(BLANK, 0)"},
		{`EVALUATE SUMMARIZECOLUMNS("z", EDATE(DIVIDE(1, 0), 1), "k", 1)`, "EDATE(BLANK, 1)"},
		{`EVALUATE SUMMARIZECOLUMNS("z", TRUNC(DIVIDE(1, 0)), "k", 1)`, "TRUNC(BLANK)"},
		{`EVALUATE SUMMARIZECOLUMNS("z", QUOTIENT(DIVIDE(1, 0), 3), "k", 1)`, "QUOTIENT(BLANK, 3)"},
		{`EVALUATE SUMMARIZECOLUMNS("z", BLANK(), "k", 1)`, "BLANK()"},
		{`EVALUATE SUMMARIZECOLUMNS("z", INT(DIVIDE(1, 0)), "k", 1)`, "INT(BLANK)"},
	}
	for _, tc := range blank {
		res, err := Evaluate(m, d, tc.q)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if len(res.Rows) != 1 || res.Rows[0]["[z]"] != nil {
			t.Errorf("%s = %v, want BLANK", tc.name, res.Rows)
		}
	}

	res, err := Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS("v", SWITCH(BLANK(), BLANK(), 10, 99))`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || toF(res.Rows[0]["[v]"]) != 10 {
		t.Errorf("SWITCH(BLANK, BLANK, 10) = %v, want 10", res.Rows)
	}

	// Sunday 2024-08-18: return_type 2 → 7, return_type 3 → 6.
	res, err = Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS("v", WEEKDAY(DATE(2024, 8, 18), 2))`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || toF(res.Rows[0]["[v]"]) != 7 {
		t.Errorf("WEEKDAY(Sunday, 2) = %v, want 7", res.Rows)
	}
	res, err = Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS("v", WEEKDAY(DATE(2024, 8, 18), 3))`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || toF(res.Rows[0]["[v]"]) != 6 {
		t.Errorf("WEEKDAY(Sunday, 3) = %v, want 6", res.Rows)
	}

	// Desktop ISBLANK("") is false. ISBLANK(BLANK()) is true.
	res, err = Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS("v", IF(ISBLANK(""), 1, 0))`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || toF(res.Rows[0]["[v]"]) != 0 {
		t.Errorf("ISBLANK(\"\") = %v, want 0", res.Rows)
	}

	for _, q := range []string{
		`EVALUATE SUMMARIZECOLUMNS("v", SWITCH())`,
		`EVALUATE SUMMARIZECOLUMNS("v", SWITCH(1))`,
		`EVALUATE SUMMARIZECOLUMNS("v", SQRT())`,
		`EVALUATE SUMMARIZECOLUMNS("v", SQRT(-1))`,
		`EVALUATE SUMMARIZECOLUMNS("v", MOD(10))`,
		`EVALUATE SUMMARIZECOLUMNS("v", MOD(10, 0))`,
		`EVALUATE SUMMARIZECOLUMNS("v", FLOOR(10.5))`,
		`EVALUATE SUMMARIZECOLUMNS("v", FLOOR(10.5, 0))`,
		`EVALUATE SUMMARIZECOLUMNS("v", CEILING(10.5))`,
		`EVALUATE SUMMARIZECOLUMNS("v", LN())`,
		`EVALUATE SUMMARIZECOLUMNS("v", LN(0))`,
		`EVALUATE SUMMARIZECOLUMNS("v", EXP())`,
		`EVALUATE SUMMARIZECOLUMNS("v", EXP(1000))`,
		`EVALUATE SUMMARIZECOLUMNS("v", WEEKDAY())`,
		`EVALUATE SUMMARIZECOLUMNS("v", WEEKDAY(1))`,
		`EVALUATE SUMMARIZECOLUMNS("v", WEEKDAY(DATE(2024, 8, 15), 4))`,
		`EVALUATE SUMMARIZECOLUMNS("v", WEEKNUM())`,
		`EVALUATE SUMMARIZECOLUMNS("v", WEEKNUM(DATE(2024, 8, 15), 3))`,
		`EVALUATE SUMMARIZECOLUMNS("v", EOMONTH(DATE(2024, 1, 15)))`,
		`EVALUATE SUMMARIZECOLUMNS("v", EOMONTH(1, 0))`,
		`EVALUATE SUMMARIZECOLUMNS("v", EDATE(DATE(2024, 1, 15)))`,
		`EVALUATE SUMMARIZECOLUMNS("v", EDATE(1, 1))`,
		`EVALUATE SUMMARIZECOLUMNS("v", TRUNC())`,
		`EVALUATE SUMMARIZECOLUMNS("v", TRUNC(1, 2, 3))`,
		`EVALUATE SUMMARIZECOLUMNS("v", QUOTIENT(10))`,
		`EVALUATE SUMMARIZECOLUMNS("v", QUOTIENT(10, 0))`,
		`EVALUATE SUMMARIZECOLUMNS("v", BLANK(1))`,
		`EVALUATE SUMMARIZECOLUMNS("v", ISBLANK())`,
		`EVALUATE SUMMARIZECOLUMNS("v", ISBLANK(1, 2))`,
		`EVALUATE SUMMARIZECOLUMNS("v", DISTINCTCOUNT())`,
		`EVALUATE SUMMARIZECOLUMNS("v", DISTINCTCOUNT(1))`,
		`EVALUATE SUMMARIZECOLUMNS("v", MAX())`,
		`EVALUATE SUMMARIZECOLUMNS("v", INT())`,
	} {
		if _, err := Evaluate(m, d, q); err == nil {
			t.Errorf("%q: expected error", q)
		}
	}

	// Happy paths the goldens do not all hit — these are the lines that
	// pulled Windows total coverage to 89.9% after the batch landed.
	for _, tc := range []struct {
		q    string
		want float64
		name string
	}{
		{`EVALUATE SUMMARIZECOLUMNS("v", WEEKDAY(DATE(2024, 8, 18), 1))`, 1, "WEEKDAY Sunday type 1"},
		{`EVALUATE SUMMARIZECOLUMNS("v", SQRT(9))`, 3, "SQRT(9)"},
		{`EVALUATE SUMMARIZECOLUMNS("v", MOD(-10, 3))`, 2, "MOD(-10, 3)"},
		{`EVALUATE SUMMARIZECOLUMNS("v", FLOOR(10.5, 1))`, 10, "FLOOR(10.5, 1)"},
		{`EVALUATE SUMMARIZECOLUMNS("v", CEILING(10.5, 1))`, 11, "CEILING(10.5, 1)"},
		{`EVALUATE SUMMARIZECOLUMNS("v", CEILING(10.5, 0))`, 0, "CEILING(10.5, 0)"},
		{`EVALUATE SUMMARIZECOLUMNS("v", LN(1))`, 0, "LN(1)"},
		{`EVALUATE SUMMARIZECOLUMNS("v", EXP(0))`, 1, "EXP(0)"},
		{`EVALUATE SUMMARIZECOLUMNS("v", TRUNC(-2.9))`, -2, "TRUNC(-2.9)"},
		{`EVALUATE SUMMARIZECOLUMNS("v", TRUNC(2.15, 1))`, 2.1, "TRUNC(2.15, 1)"},
		{`EVALUATE SUMMARIZECOLUMNS("v", QUOTIENT(10, 3))`, 3, "QUOTIENT(10, 3)"},
		{`EVALUATE SUMMARIZECOLUMNS("v", SWITCH(2, 1, 10, 2, 20, 99))`, 20, "SWITCH match"},
		{`EVALUATE SUMMARIZECOLUMNS("v", SWITCH(9, 1, 10, 99))`, 99, "SWITCH else"},
		{`EVALUATE SUMMARIZECOLUMNS("v", IF(ISBLANK(BLANK()), 1, 0))`, 1, "ISBLANK(BLANK)"},
	} {
		res, err := Evaluate(m, d, tc.q)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if len(res.Rows) != 1 || toF(res.Rows[0]["[v]"]) != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, res.Rows, tc.want)
		}
	}
}

// TestDAXPhase3DesktopBlankEdges pins the eight Desktop-proved BLANK / error
// cases the numeric golden harness cannot store. Goldens coerce BLANK to 0
// and cannot hold an error; these are the traps that would otherwise look
// like a happy scalar.
func TestDAXPhase3DesktopBlankEdges(t *testing.T) {
	m, d := loadModel(t), loadData(t)

	res, err := Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS("v", EXP(BLANK()))`)
	if err != nil {
		t.Fatalf("EXP(BLANK()): %v", err)
	}
	if len(res.Rows) != 1 || toF(res.Rows[0]["[v]"]) != 1 {
		t.Errorf("EXP(BLANK()) = %v, want 1", res.Rows)
	}

	if _, err := Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS("v", LN(-1))`); err == nil {
		t.Error("LN(-1): expected error")
	}

	res, err = Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS("v", CEILING(10.5, BLANK()))`)
	if err != nil {
		t.Fatalf("CEILING(10.5, BLANK()): %v", err)
	}
	if len(res.Rows) != 1 || toF(res.Rows[0]["[v]"]) != 0 {
		t.Errorf("CEILING(10.5, BLANK()) = %v, want 0", res.Rows)
	}

	res, err = Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS("v", DAY(EOMONTH(DATE(2024, 1, 15), BLANK())))`)
	if err != nil {
		t.Fatalf("EOMONTH(date, BLANK()): %v", err)
	}
	if len(res.Rows) != 1 || toF(res.Rows[0]["[v]"]) != 31 {
		t.Errorf("DAY(EOMONTH(2024-01-15, BLANK())) = %v, want 31 (months 0)", res.Rows)
	}

	res, err = Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS("v", DAY(EDATE(DATE(2024, 1, 15), BLANK())))`)
	if err != nil {
		t.Fatalf("EDATE(date, BLANK()): %v", err)
	}
	if len(res.Rows) != 1 || toF(res.Rows[0]["[v]"]) != 15 {
		t.Errorf("DAY(EDATE(2024-01-15, BLANK())) = %v, want 15 (months 0)", res.Rows)
	}

	res, err = Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS("v", TRUNC(2.9, BLANK()))`)
	if err != nil {
		t.Fatalf("TRUNC(2.9, BLANK()): %v", err)
	}
	if len(res.Rows) != 1 || toF(res.Rows[0]["[v]"]) != 2 {
		t.Errorf("TRUNC(2.9, BLANK()) = %v, want 2 (digits 0)", res.Rows)
	}

	blankCol := &Model{Name: "x", Tables: []Table{{
		Name:    "T",
		Columns: []Column{{Name: "n", DataType: "int64"}},
	}}}
	blankData := Data{"T": {
		{"n": nil},
		{"n": ""},
		{"n": 1},
		{"n": 1},
		{"n": nil},
		{"n": 2},
	}}

	res, err = Evaluate(blankCol, Data{"T": {{"n": nil}, {"n": ""}}}, `EVALUATE SUMMARIZECOLUMNS("z", MAX(T[n]), "k", 1)`)
	if err != nil {
		t.Fatalf("MAX all-blank: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0]["[z]"] != nil {
		t.Errorf("MAX of all-blank = %v, want BLANK", res.Rows)
	}

	res, err = Evaluate(blankCol, blankData, `EVALUATE SUMMARIZECOLUMNS("v", DISTINCTCOUNT(T[n]))`)
	if err != nil {
		t.Fatalf("DISTINCTCOUNT skips BLANK: %v", err)
	}
	if len(res.Rows) != 1 || toF(res.Rows[0]["[v]"]) != 2 {
		t.Errorf("DISTINCTCOUNT with blanks = %v, want 2 (1 and 2; BLANK skipped)", res.Rows)
	}
}

// TestDAXRowConstructor pins Desktop ROW traps the numeric golden harness
// cannot store: a BLANK cell is kept (SUMMARIZECOLUMNS would drop the
// group), two columns, and the name/arity refusals.
func TestDAXRowConstructor(t *testing.T) {
	m, d := loadModel(t), loadData(t)

	res, err := Evaluate(m, d, `EVALUATE ROW("v", BLANK())`)
	if err != nil {
		t.Fatalf("ROW(BLANK): %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0]["[v]"] != nil {
		t.Errorf("ROW(\"v\", BLANK()) = %v, want one row with BLANK", res.Rows)
	}

	res, err = Evaluate(m, d, `EVALUATE ROW("a", 1, "b", 2)`)
	if err != nil {
		t.Fatalf("ROW two columns: %v", err)
	}
	if len(res.Rows) != 1 || toF(res.Rows[0]["[a]"]) != 1 || toF(res.Rows[0]["[b]"]) != 2 {
		t.Errorf("ROW(\"a\", 1, \"b\", 2) = %v, want [a]=1 [b]=2", res.Rows)
	}

	for _, q := range []string{
		`EVALUATE ROW()`,
		`EVALUATE ROW("x")`,
		`EVALUATE ROW(1, 1)`,
		`EVALUATE ROW("a", 1, "b")`,
		`EVALUATE ROW("a", 1, 2, 3)`,
		`EVALUATE ROW("a", 1, "a", 2)`,
		`EVALUATE ROW("", 1)`,
	} {
		if _, err := Evaluate(m, d, q); err == nil {
			t.Errorf("%q: expected error", q)
		}
	}
}

func TestDAXPhase3Helpers(t *testing.T) {
	sun := time.Date(2024, 8, 18, 0, 0, 0, 0, time.UTC) // Sunday
	mon := time.Date(2024, 8, 19, 0, 0, 0, 0, time.UTC)
	jan31 := time.Date(2024, 1, 31, 0, 0, 0, 0, time.UTC)

	if v, err := daxWeekday(sun, 1); err != nil || v != 1 {
		t.Errorf("daxWeekday(Sun, 1) = %v %v, want 1", v, err)
	}
	if v, err := daxWeekday(mon, 1); err != nil || v != 2 {
		t.Errorf("daxWeekday(Mon, 1) = %v %v, want 2", v, err)
	}
	if v, err := daxWeekday(mon, 2); err != nil || v != 1 {
		t.Errorf("daxWeekday(Mon, 2) = %v %v, want 1", v, err)
	}
	if v, err := daxWeekday(mon, 3); err != nil || v != 0 {
		t.Errorf("daxWeekday(Mon, 3) = %v %v, want 0", v, err)
	}
	if _, err := daxWeekday(sun, 9); err == nil {
		t.Error("daxWeekday type 9: expected error")
	}

	if v, err := daxWeeknum(sun, 1); err != nil || v < 1 || v > 54 {
		t.Errorf("daxWeeknum(Sun, 1) = %v %v", v, err)
	}
	if v, err := daxWeeknum(sun, 2); err != nil || v < 1 || v > 54 {
		t.Errorf("daxWeeknum(Sun, 2) = %v %v", v, err)
	}
	if v, err := daxWeeknum(sun, 21); err != nil || v < 1 || v > 53 {
		t.Errorf("daxWeeknum(Sun, 21) = %v %v", v, err)
	}
	if _, err := daxWeeknum(sun, 3); err == nil {
		t.Error("daxWeeknum type 3: expected error")
	}

	eom := daxEomonth(jan31, 1)
	if eom.Year() != 2024 || eom.Month() != time.February || eom.Day() != 29 {
		t.Errorf("daxEomonth(2024-01-31, 1) = %v, want 2024-02-29", eom)
	}
	ed := daxEdate(jan31, 1)
	if ed.Year() != 2024 || ed.Month() != time.February || ed.Day() != 29 {
		t.Errorf("daxEdate(2024-01-31, 1) = %v, want 2024-02-29", ed)
	}

	if daxTrunc(2.9, 0) != 2 || daxTrunc(-2.9, 0) != -2 {
		t.Errorf("daxTrunc toward zero: %v %v", daxTrunc(2.9, 0), daxTrunc(-2.9, 0))
	}
	if daxTrunc(2.15, 1) != 2.1 {
		t.Errorf("daxTrunc(2.15, 1) = %v, want 2.1", daxTrunc(2.15, 1))
	}
	if daxTrunc(-2.15, 1) != -2.1 {
		t.Errorf("daxTrunc(-2.15, 1) = %v, want -2.1", daxTrunc(-2.15, 1))
	}

	if distinctKey(3.0) != "n:3" {
		t.Errorf("distinctKey(3) = %q", distinctKey(3.0))
	}
	if distinctKey("west") != "s:west" {
		t.Errorf("distinctKey(west) = %q", distinctKey("west"))
	}
	if !switchEq(nil, nil) || switchEq(nil, 1) || !switchEq(2.0, 2) {
		t.Error("switchEq")
	}
}

func TestDeployConverters(t *testing.T) {
	if daxDataType("int64") != "INTEGER" || daxDataType("double") != "DOUBLE" ||
		daxDataType("boolean") != "BOOLEAN" || daxDataType("datetime") != "DATETIME" ||
		daxDataType("string") != "STRING" {
		t.Fatal("daxDataType")
	}

	if n, err := asInt(int(3)); err != nil || n != 3 {
		t.Errorf("asInt(int) = %v %v", n, err)
	}
	if n, err := asInt(int32(3)); err != nil || n != 3 {
		t.Errorf("asInt(int32) = %v %v", n, err)
	}
	if n, err := asInt(int64(3)); err != nil || n != 3 {
		t.Errorf("asInt(int64) = %v %v", n, err)
	}
	if n, err := asInt(3.0); err != nil || n != 3 {
		t.Errorf("asInt(3.0) = %v %v", n, err)
	}
	if _, err := asInt(3.5); err == nil {
		t.Error("asInt(3.5) should fail")
	}
	if n, err := asInt(json.Number("7")); err != nil || n != 7 {
		t.Errorf("asInt(json.Number) = %v %v", n, err)
	}
	if n, err := asInt("8"); err != nil || n != 8 {
		t.Errorf("asInt(string) = %v %v", n, err)
	}
	if _, err := asInt(true); err == nil {
		t.Error("asInt(bool) should fail")
	}

	if f, err := asFloat(1.5); err != nil || f != 1.5 {
		t.Errorf("asFloat(float64) = %v %v", f, err)
	}
	if f, err := asFloat(2); err != nil || f != 2 {
		t.Errorf("asFloat(int) = %v %v", f, err)
	}
	if f, err := asFloat(int64(3)); err != nil || f != 3 {
		t.Errorf("asFloat(int64) = %v %v", f, err)
	}
	if f, err := asFloat(json.Number("4.5")); err != nil || f != 4.5 {
		t.Errorf("asFloat(json.Number) = %v %v", f, err)
	}
	if f, err := asFloat("6.25"); err != nil || f != 6.25 {
		t.Errorf("asFloat(string) = %v %v", f, err)
	}
	if _, err := asFloat(true); err == nil {
		t.Error("asFloat(bool) should fail")
	}

	if b, err := asBool(true); err != nil || !b {
		t.Errorf("asBool(true) = %v %v", b, err)
	}
	if b, err := asBool("false"); err != nil || b {
		t.Errorf("asBool(\"false\") = %v %v", b, err)
	}
	if _, err := asBool(1); err == nil {
		t.Error("asBool(1) should fail")
	}

	if s, err := asDateTime(time.Date(2024, 8, 15, 0, 0, 0, 0, time.UTC)); err != nil || s != "DATE(2024,8,15)" {
		t.Errorf("asDateTime(Time) = %q %v", s, err)
	}
	if s, err := asDateTime("2024-08-15T00:00:00Z"); err != nil || s != "DATE(2024,8,15)" {
		t.Errorf("asDateTime(RFC3339) = %q %v", s, err)
	}
	if s, err := asDateTime("2024-08-15"); err != nil || s != "DATE(2024,8,15)" {
		t.Errorf("asDateTime(date) = %q %v", s, err)
	}
	if _, err := asDateTime("nope"); err == nil {
		t.Error("asDateTime(nope) should fail")
	}
	if _, err := asDateTime(1); err == nil {
		t.Error("asDateTime(int) should fail")
	}

	if s, err := daxLiteral(nil, "int64"); err != nil || s != "BLANK()" {
		t.Errorf("daxLiteral(nil) = %q %v", s, err)
	}
	if s, err := daxLiteral(3, "int64"); err != nil || s != "3" {
		t.Errorf("daxLiteral(int) = %q %v", s, err)
	}
	if s, err := daxLiteral(1.5, "double"); err != nil || s != "1.5" {
		t.Errorf("daxLiteral(double) = %q %v", s, err)
	}
	if s, err := daxLiteral(true, "boolean"); err != nil || s != "TRUE()" {
		t.Errorf("daxLiteral(true) = %q %v", s, err)
	}
	if s, err := daxLiteral(false, "boolean"); err != nil || s != "FALSE()" {
		t.Errorf("daxLiteral(false) = %q %v", s, err)
	}
	if s, err := daxLiteral("2024-01-02", "datetime"); err != nil || s != "DATE(2024,1,2)" {
		t.Errorf("daxLiteral(datetime) = %q %v", s, err)
	}
	if s, err := daxLiteral("hi", "string"); err != nil || s != `"hi"` {
		t.Errorf("daxLiteral(string) = %q %v", s, err)
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
