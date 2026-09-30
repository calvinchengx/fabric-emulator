package semanticmodel

import (
	"slices"
	"strings"
	"testing"
)

// ORDER BY is the one piece of query syntax the Fabric IQ skill tells an agent
// to put on every multi-row EVALUATE ("ALWAYS include an ORDER BY clause when
// EVALUATE returns multiple rows"), so a subset without it refuses nearly every
// query an agent writes.

func resultColumn(res *Result, key string) []any {
	out := make([]any, 0, len(res.Rows))
	for _, r := range res.Rows {
		out = append(out, r[key])
	}
	return out
}

func TestOrderBySortsByAResultColumnInEitherDirection(t *testing.T) {
	m, d := loadModel(t), loadData(t)
	base := `EVALUATE SUMMARIZECOLUMNS('Store'[Territory], "Units", [TotalUnits])`

	asc, err := Evaluate(m, d, base+` ORDER BY [Units]`)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := Evaluate(m, d, base+` ORDER BY [Units] DESC`)
	if err != nil {
		t.Fatal(err)
	}
	if len(asc.Rows) < 2 {
		t.Fatalf("fixture needs several territories, got %d rows", len(asc.Rows))
	}
	up := resultColumn(asc, "[Units]")
	if !slices.IsSortedFunc(up, func(a, b any) int { return compareOrderValues(a, b) }) {
		t.Errorf("ASC is the default and was not ascending: %v", up)
	}
	down := resultColumn(desc, "[Units]")
	slices.Reverse(down)
	if !slices.Equal(up, down) {
		t.Errorf("DESC is not ASC reversed: asc %v, desc reversed %v", up, down)
	}
}

func TestOrderByAColumnReferenceAndSeveralKeys(t *testing.T) {
	m, d := loadModel(t), loadData(t)
	res, err := Evaluate(m, d,
		`EVALUATE SUMMARIZECOLUMNS('Store'[Territory], "Units", [TotalUnits]) ORDER BY 'Store'[Territory] DESC, [Units] ASC`)
	if err != nil {
		t.Fatal(err)
	}
	names := resultColumn(res, "Store[Territory]")
	want := slices.Clone(names)
	slices.SortFunc(want, func(a, b any) int { return -compareOrderValues(a, b) })
	if !slices.Equal(names, want) {
		t.Errorf("not sorted by territory descending: %v", names)
	}
}

func TestOrderByComparesTextWithoutCaseAndPutsBlankFirst(t *testing.T) {
	// DAX text comparison is case-insensitive, and BLANK sorts before any value
	// in ascending order.
	vals := []any{"beta", nil, "Alpha", 2.0, "alpha2"}
	slices.SortStableFunc(vals, compareOrderValues)
	if vals[0] != nil {
		t.Errorf("blank should sort first, got %v", vals)
	}
	if got := compareOrderValues("Alpha", "alpha"); got != 0 {
		t.Errorf("text comparison should ignore case, got %d", got)
	}
}

func TestOrderByRefusesWhatItCannotSortBy(t *testing.T) {
	m, d := loadModel(t), loadData(t)
	base := `EVALUATE SUMMARIZECOLUMNS('Store'[Territory], "Units", [TotalUnits])`
	for q, want := range map[string]string{
		base + ` ORDER BY`:                  "ORDER BY expects",
		base + ` ORDER BY [NotInResult]`:    "not a column of the query result",
		base + ` ORDER BY [Units] SIDEWAYS`: "trailing tokens",
		base + ` ORDER [Units]`:             "trailing tokens",
		base + ` ORDER BY [Units] + 1`:      "trailing tokens",
	} {
		_, err := Evaluate(m, d, q)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want an error containing %q", q[len(base):], err, want)
		}
	}
}
