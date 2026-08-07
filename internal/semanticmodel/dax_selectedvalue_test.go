package semanticmodel

import (
	"strings"
	"testing"
)

// SELECTEDVALUE reads the one value a column has under the current filter
// context (issue #47). Until it existed there was no way to DERIVE anything
// from the column being grouped by: a bare column reference is refused — real
// DAX refuses it too — so `"FY" & Time[FiscalYear]` had no working spelling.
//
// The seeded fixture, which every expectation below is computed from:
//
//	Store  4 rows  Territory West(2) East(1) Central(1)
//	Time   4 rows  FiscalYear FY2013(Jan,Apr) FY2014(Jan,Apr)
//	Sales  8 rows  related to Store by StoreId and Time by MonthKey

// evalRows runs a query and returns its rows keyed as executeQueries keys them.
func evalRows(t *testing.T, dax string) []map[string]any {
	t.Helper()
	res, err := Evaluate(loadModel(t), loadData(t), dax)
	if err != nil {
		t.Fatalf("%s: %v", dax, err)
	}
	return res.Rows
}

// evalErr runs a query expected to fail and returns the message.
func evalErr(t *testing.T, dax string) string {
	t.Helper()
	if _, err := Evaluate(loadModel(t), loadData(t), dax); err != nil {
		return err.Error()
	}
	t.Fatalf("%s: expected an error, got none", dax)
	return ""
}

// --- the motivating case ------------------------------------------------------

func TestSelectedValueReadsTheGroupColumn(t *testing.T) {
	// One value per group by construction — this is what makes the function a
	// lookup rather than a new context mechanism.
	rows := evalRows(t, `EVALUATE SUMMARIZECOLUMNS('Time'[FiscalYear], "Y", SELECTEDVALUE('Time'[FiscalYear]))`)
	got := map[string]any{}
	for _, r := range rows {
		got[r["Time[FiscalYear]"].(string)] = r["[Y]"]
	}
	if len(got) != 2 || got["FY2013"] != "FY2013" || got["FY2014"] != "FY2014" {
		t.Fatalf("got %v", got)
	}
}

func TestSelectedValueBuildsADerivedLabel(t *testing.T) {
	// The exact shape from the issue: `&` was never broken, but it read as
	// broken because its only interesting operand could not be expressed.
	rows := evalRows(t, `EVALUATE SUMMARIZECOLUMNS('Time'[FiscalYear], "Label", "Fiscal " & SELECTEDVALUE('Time'[FiscalYear]))`)
	got := map[string]bool{}
	for _, r := range rows {
		got[r["[Label]"].(string)] = true
	}
	if len(got) != 2 || !got["Fiscal FY2013"] || !got["Fiscal FY2014"] {
		t.Fatalf("labels = %v", got)
	}
}

// --- not exactly one value ----------------------------------------------------

func TestSelectedValueFallsBackWhenTheColumnHasManyValues(t *testing.T) {
	// Grouping by FiscalYear leaves TWO FiscalMonths in each group (Jan, Apr),
	// so the column has no single value and the alternate is the answer.
	rows := evalRows(t, `EVALUATE SUMMARIZECOLUMNS('Time'[FiscalYear], "M", SELECTEDVALUE('Time'[FiscalMonth], "(mixed)"))`)
	for _, r := range rows {
		if r["[M]"] != "(mixed)" {
			t.Fatalf("FiscalYear %v: [M] = %v, want the alternate", r["Time[FiscalYear]"], r["[M]"])
		}
	}
}

func TestSelectedValueDefaultsToBlankWithNoAlternate(t *testing.T) {
	// Omitted alternate is BLANK, not an error and not an arbitrary pick.
	// SUMMARIZECOLUMNS drops all-blank groups, so an ungrouped query is used to
	// observe the blank rather than losing the row.
	rows := evalRows(t, `EVALUATE SUMMARIZECOLUMNS("M", SELECTEDVALUE('Time'[FiscalMonth]) & "!")`)
	if len(rows) != 1 || rows[0]["[M]"] != "!" {
		t.Fatalf("rows = %v, want one row whose value is blank concatenated to \"!\"", rows)
	}
}

func TestSelectedValueFallsBackWithNoRowsInContext(t *testing.T) {
	// Zero distinct values is also "not exactly one". Central is one store, so
	// filtering to it and reading a Territory that store does not have leaves
	// the Sales side empty.
	rows := evalRows(t, `EVALUATE SUMMARIZECOLUMNS('Store'[Territory], "S", SELECTEDVALUE('Store'[Store], "(none)"))`)
	byTerritory := map[string]any{}
	for _, r := range rows {
		byTerritory[r["Store[Territory]"].(string)] = r["[S]"]
	}
	// West has two stores → alternate; Central and East have one → the store.
	if byTerritory["West"] != "(none)" {
		t.Errorf("West = %v, want the alternate (two stores)", byTerritory["West"])
	}
	if byTerritory["Central"] != "Store C" {
		t.Errorf("Central = %v, want Store C", byTerritory["Central"])
	}
	if byTerritory["East"] != "Store B" {
		t.Errorf("East = %v, want Store B", byTerritory["East"])
	}
}

// --- context propagation ------------------------------------------------------

func TestSelectedValueHonoursRelatedTableFiltering(t *testing.T) {
	// The column is on a DIFFERENT table from the one grouped by, reached over
	// the Store→Sales relationship. This is the case a shortcut that read the
	// group value straight out of the filter context would answer wrongly.
	rows := evalRows(t, `EVALUATE SUMMARIZECOLUMNS('Store'[Store], "K", SELECTEDVALUE('Sales'[MonthKey], -1))`)
	for _, r := range rows {
		// Every store has sales in more than one month, so the alternate wins —
		// and it is the NUMBER -1, proving the alternate is evaluated as an
		// expression rather than pasted in as text.
		if r["[K]"] != -1.0 {
			t.Fatalf("%v: [K] = %v (%T), want -1", r["Store[Store]"], r["[K]"], r["[K]"])
		}
	}
}

func TestSelectedValueReturnsANumberNotAString(t *testing.T) {
	rows := evalRows(t, `EVALUATE SUMMARIZECOLUMNS('Store'[StoreId], "N", SELECTEDVALUE('Store'[StoreId]))`)
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4", len(rows))
	}
	for _, r := range rows {
		n, ok := r["[N]"].(float64)
		if !ok {
			t.Fatalf("[N] = %v (%T), want float64", r["[N]"], r["[N]"])
		}
		if n != r["Store[StoreId]"] {
			t.Fatalf("[N] = %v, want the group's own StoreId %v", n, r["Store[StoreId]"])
		}
	}
}

// --- the alternate is lazy ----------------------------------------------------

func TestSelectedValueDoesNotEvaluateTheAlternateWhenUnused(t *testing.T) {
	// SUM over a text column is an error. On the single-value path the
	// alternate must never be evaluated, so this query must SUCCEED — an eager
	// implementation fails it.
	rows := evalRows(t,
		`EVALUATE SUMMARIZECOLUMNS('Time'[FiscalYear], "Y", SELECTEDVALUE('Time'[FiscalYear], SUM('Time'[FiscalMonth])))`)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	for _, r := range rows {
		if r["[Y]"] != r["Time[FiscalYear]"] {
			t.Fatalf("[Y] = %v, want %v", r["[Y]"], r["Time[FiscalYear]"])
		}
	}
}

func TestSelectedValueSurfacesAFailingAlternateWhenItIsUsed(t *testing.T) {
	// The dual: when the alternate IS needed, its error must reach the caller
	// rather than being swallowed into a blank.
	msg := evalErr(t,
		`EVALUATE SUMMARIZECOLUMNS('Time'[FiscalYear], "M", SELECTEDVALUE('Time'[FiscalMonth], SUM('Time'[FiscalMonth])))`)
	if !strings.Contains(msg, "not a number") {
		t.Fatalf("error = %q, want the SUM failure to surface", msg)
	}
}

// --- refusals -----------------------------------------------------------------

func TestSelectedValueRefusesAnUnknownColumn(t *testing.T) {
	// Without this guard an unknown column reads as BLANK on every row,
	// collapses to one distinct value, and returns BLANK — a typo answering
	// confidently, and indistinguishable from a real single blank.
	msg := evalErr(t, `EVALUATE SUMMARIZECOLUMNS("v", SELECTEDVALUE('Time'[NoSuchColumn]))`)
	if !strings.Contains(msg, "no column") || !strings.Contains(msg, "NoSuchColumn") {
		t.Fatalf("error = %q", msg)
	}
}

func TestSelectedValueRefusesAnUnknownTable(t *testing.T) {
	msg := evalErr(t, `EVALUATE SUMMARIZECOLUMNS("v", SELECTEDVALUE('NoSuchTable'[X]))`)
	if !strings.Contains(msg, "no table") || !strings.Contains(msg, "NoSuchTable") {
		t.Fatalf("error = %q", msg)
	}
}

func TestSelectedValueRefusesNonColumnArguments(t *testing.T) {
	for _, q := range []string{
		`EVALUATE SUMMARIZECOLUMNS("v", SELECTEDVALUE())`,
		`EVALUATE SUMMARIZECOLUMNS("v", SELECTEDVALUE(42))`,
		`EVALUATE SUMMARIZECOLUMNS("v", SELECTEDVALUE("text"))`,
	} {
		if msg := evalErr(t, q); !strings.Contains(msg, "column reference") {
			t.Errorf("%s: error = %q, want it to ask for a column reference", q, msg)
		}
	}
}

// --- the message that sends people here ---------------------------------------

func TestBareColumnErrorNamesSelectedValue(t *testing.T) {
	// The issue's premise: the old message's only exit was "group by it", which
	// does not help when the point is to derive a label. Now that the function
	// exists the message may name it — pointing at an unimplemented function
	// would have walked the reader into a second wall.
	msg := evalErr(t, `EVALUATE SUMMARIZECOLUMNS('Time'[FiscalYear], "L", "FY" & 'Time'[FiscalYear])`)
	if !strings.Contains(msg, "SELECTEDVALUE(Time[FiscalYear])") {
		t.Fatalf("error = %q, want it to name SELECTEDVALUE with the column", msg)
	}
	// The grouping advice stays: it is still right for returning the raw column.
	if !strings.Contains(msg, "group by") {
		t.Fatalf("error = %q, want the grouping exit kept", msg)
	}
}

func TestBareColumnErrorStillOffersSumForNumericColumns(t *testing.T) {
	// Naming SELECTEDVALUE must not have displaced the aggregate advice, which
	// is offered only where it is sound.
	msg := evalErr(t, `EVALUATE SUMMARIZECOLUMNS('Store'[Territory], "U", 'Sales'[Units])`)
	if !strings.Contains(msg, "SUM(Sales[Units])") {
		t.Fatalf("error = %q, want SUM offered for a summable column", msg)
	}
	// ... and not for a text column, where it yields a label reading "FY0".
	msg = evalErr(t, `EVALUATE SUMMARIZECOLUMNS('Time'[FiscalYear], "M", 'Time'[FiscalMonth])`)
	if strings.Contains(msg, "SUM(") {
		t.Fatalf("error = %q, must not offer SUM on a text column", msg)
	}
}

func TestTheAdviceInTheBareColumnErrorActuallyWorks(t *testing.T) {
	// An error that suggests a fix is only as good as the fix. Both spellings
	// the message offers are run here, so a reworded suggestion that no longer
	// parses fails the build rather than misleading the next reader.
	//
	// The message prints table names unquoted (`Time[FiscalYear]`), which the
	// parser accepts for these names — a table whose name contained a space
	// would need quoting, which no fixture exercises and no message emits.
	rows := evalRows(t, `EVALUATE SUMMARIZECOLUMNS('Time'[FiscalYear], "L", "FY" & SELECTEDVALUE(Time[FiscalYear]))`)
	if len(rows) != 2 {
		t.Fatalf("SELECTEDVALUE advice: rows = %d, want 2", len(rows))
	}
	rows = evalRows(t, `EVALUATE SUMMARIZECOLUMNS('Store'[Territory], "U", SUM(Sales[Units]))`)
	if len(rows) != 3 {
		t.Fatalf("SUM advice: rows = %d, want 3 territories", len(rows))
	}
}

// --- BLANK is a value, not an absence -----------------------------------------

// blankModel is a two-column table whose `Note` is blank on every row and whose
// `Mixed` is blank on some — neither shape exists in the shared retail fixture,
// and changing that fixture would move an oracle four other suites compare
// against. A local model is the cheap way to assert a rule the shared data
// cannot express.
func blankModel() (*Model, Data) {
	m := &Model{Name: "Blanks", Tables: []Table{{
		Name: "T",
		Columns: []Column{
			{Name: "Grp", DataType: "string"},
			{Name: "Note", DataType: "string"},
			{Name: "Mixed", DataType: "string"},
		},
	}}}
	d := Data{"T": []Row{
		{"Grp": "a", "Note": nil, "Mixed": nil},
		{"Grp": "a", "Note": nil, "Mixed": "x"},
		{"Grp": "b", "Note": nil, "Mixed": nil},
	}}
	return m, d
}

func TestSelectedValueReturnsBlankWhenBlankIsTheOnlyValue(t *testing.T) {
	// THE RULE: a column that is uniformly blank has exactly ONE value, and
	// that value is BLANK. Skipping nils while scanning would make it look like
	// zero values and hand back the alternate — a different fact, and one that
	// 100% line coverage does not distinguish.
	// Concatenated with "!" because SUMMARIZECOLUMNS DROPS an all-blank group,
	// so a bare blank output would vanish the row and the test would pass for
	// the wrong reason. "!" means BLANK came back; "(alt)!" means it did not.
	m, d := blankModel()
	res, err := Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS(T[Grp], "N", SELECTEDVALUE(T[Note], "(alt)") & "!")`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("rows = %d, want 2 groups", len(res.Rows))
	}
	for _, r := range res.Rows {
		if r["[N]"] != "!" {
			t.Fatalf("group %v: [N] = %v, want \"!\" — BLANK is the column's single value",
				r["T[Grp]"], r["[N]"])
		}
	}
}

func TestSelectedValueCountsBlankAlongsideARealValue(t *testing.T) {
	// Group "a" holds BLANK and "x": two distinct values, so the alternate
	// wins. Group "b" holds only BLANK: one value, so BLANK wins. The same
	// query answering differently per group is what pins blank as a *value*
	// rather than a hole.
	m, d := blankModel()
	res, err := Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS(T[Grp], "M", SELECTEDVALUE(T[Mixed], "(alt)") & "!")`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, r := range res.Rows {
		got[r["T[Grp]"].(string)] = r["[M]"]
	}
	if got["a"] != "(alt)!" {
		t.Errorf("group a = %v, want the alternate (BLANK and \"x\" are two values)", got["a"])
	}
	if got["b"] != "!" {
		t.Errorf("group b = %v, want \"!\" — BLANK alone is one value", got["b"])
	}
}

// --- the same guard, everywhere a column or table is read ---------------------

// A missing column is not absent — Row is a map, so a typo reads as BLANK on
// every row and each function folds that into its own confident answer. These
// used to return a NUMBER for a query that names nothing real.
func TestReadingSomethingThatDoesNotExistIsAnErrorNotAZero(t *testing.T) {
	for _, tc := range []struct{ name, dax, want string }{
		{
			name: "SUM of an unknown column returned 0",
			dax:  `EVALUATE SUMMARIZECOLUMNS('Store'[Territory], "u", SUM('Sales'[Unitz]))`,
			want: "no column",
		},
		{
			name: "SUM on an unknown table returned 0",
			dax:  `EVALUATE SUMMARIZECOLUMNS('Store'[Territory], "u", SUM('Salez'[Units]))`,
			want: "no table",
		},
		{
			name: "COUNTROWS of an unknown table returned 0",
			dax:  `EVALUATE SUMMARIZECOLUMNS('Store'[Territory], "n", COUNTROWS('Salez'))`,
			want: "no table",
		},
		{
			name: "SELECTEDVALUE of an unknown column",
			dax:  `EVALUATE SUMMARIZECOLUMNS('Store'[Territory], "v", SELECTEDVALUE('Sales'[Unitz]))`,
			want: "no column",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := evalErr(t, tc.dax)
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("error = %q, want it to mention %q", msg, tc.want)
			}
		})
	}
}

func TestTheGuardNamesTheFunctionThatFailed(t *testing.T) {
	// Three functions share one helper, so the message must say which one the
	// caller wrote — otherwise a nested expression gives no clue where to look.
	if msg := evalErr(t, `EVALUATE SUMMARIZECOLUMNS("u", SUM('Sales'[Unitz]))`); !strings.Contains(msg, "SUM:") {
		t.Errorf("SUM: error = %q", msg)
	}
	if msg := evalErr(t, `EVALUATE SUMMARIZECOLUMNS("n", COUNTROWS('Salez'))`); !strings.Contains(msg, "COUNTROWS:") {
		t.Errorf("COUNTROWS: error = %q", msg)
	}
	if msg := evalErr(t, `EVALUATE SUMMARIZECOLUMNS("v", SELECTEDVALUE('Sales'[Unitz]))`); !strings.Contains(msg, "SELECTEDVALUE:") {
		t.Errorf("SELECTEDVALUE: error = %q", msg)
	}
}

func TestValidColumnsAndTablesStillEvaluate(t *testing.T) {
	// The guard must not have made a correct query fail: 275000 units over the
	// eight seeded Sales rows, in 8 rows.
	rows := evalRows(t, `EVALUATE SUMMARIZECOLUMNS("u", SUM('Sales'[Units]), "n", COUNTROWS('Sales'))`)
	if len(rows) != 1 || rows[0]["[u]"] != 275000.0 || rows[0]["[n]"] != 8.0 {
		t.Fatalf("rows = %v, want one row with 275000 units over 8 rows", rows)
	}
}
