package semanticmodel

import (
	"strings"
	"testing"
)

func filterModel() *Model {
	return &Model{Tables: []Table{
		{Name: "Store", Columns: []Column{{Name: "Territory"}, {Name: "Amount"}, {Name: "Email"}, {Name: "Open"}}},
		{Name: "Other", Columns: []Column{{Name: "X"}}},
	}}
}

// Every admission is paired with the row it must refuse. A filter evaluator
// that answered TRUE for everything would pass the first half of each.
func TestRowFilterEvaluation(t *testing.T) {
	m := filterModel()
	west := Row{"Territory": "West", "Amount": 150.0, "Email": "ada@contoso.com", "Open": true}
	east := Row{"Territory": "East", "Amount": 50, "Email": "grace@contoso.com", "Open": false}
	blank := Row{"Territory": nil, "Amount": nil, "Email": nil}
	padded := Row{"Territory": "West", "Amount": "  "}
	env := SecurityEnv{UPN: "ADA@contoso.com"}

	for _, tc := range []struct {
		expr    string
		yes, no Row
	}{
		{`[Territory] = "West"`, west, east},
		{`[Territory] = "WEST"`, west, east}, // DAX strings compare case-insensitively
		{`'Store'[Territory] = "West"`, west, east},
		{`Store[Territory] = "West"`, west, east},
		{`store[territory] = "West"`, west, east}, // names resolve case-insensitively too
		{`([Territory] = "West")`, west, east},
		{`[Amount] > 100`, west, east},
		{`[Amount] >= 150`, west, east},
		{`[Amount] < 100`, east, west},
		{`[Amount] <= 50`, east, west},
		{`[Territory] <> "West"`, east, west},
		{`-5 < [Amount] && [Amount] > 100`, west, east},
		{`[Territory] IN {"West", "North"}`, west, east},
		{`[Amount] IN {50, 60}`, east, west},
		{`[Territory] = "West" || [Territory] = "North"`, west, east},
		{`[Territory] = "West" && [Amount] > 100`, west, east},
		{`[Territory] = "North" || [Territory] = "West" && [Amount] > 100`, west, east}, // && binds tighter
		{`NOT([Territory] = "East")`, west, east},
		{`NOT [Territory] = "East"`, west, east},
		{`AND([Territory] = "West", [Open] = TRUE())`, west, east},
		{`OR([Territory] = "West", FALSE())`, west, east},
		{`[Open]`, west, east},
		{`[Territory] = "West" || [Open]`, west, blank}, // BLANK reads as FALSE in a logical position
		{`[Email] = USERPRINCIPALNAME()`, west, east},   // the UPN, case-insensitively
		{`[Email] = USERNAME()`, west, east},
		{`[Territory] = BLANK()`, blank, west},
		{`[Territory] = ""`, blank, west}, // BLANK is the empty string against text
		{`[Amount] = 0`, blank, west},     // …and zero against a number
		{`[Amount] = 0`, padded, west},    // a blank-text column value reads as BLANK against a number
		{`[Open] = FALSE()`, blank, west}, // …and FALSE against a boolean
		{`TRUE() = TRUE()`, west, nil},
	} {
		f, err := CompileRowFilter(m, "Store", tc.expr)
		if err != nil {
			t.Errorf("%s: compile: %v", tc.expr, err)
			continue
		}
		if ok, err := f.Admits(tc.yes, env); err != nil || !ok {
			t.Errorf("%s: admits %v = %v, %v; want true", tc.expr, tc.yes, ok, err)
		}
		if tc.no != nil {
			if ok, err := f.Admits(tc.no, env); err != nil || ok {
				t.Errorf("%s: admits %v = %v, %v; want false", tc.expr, tc.no, ok, err)
			}
		}
	}
}

// Outside the subset, compilation refuses by name.
func TestRowFilterCompileRefusals(t *testing.T) {
	m := filterModel()
	for expr, want := range map[string]string{
		`[Nope] = 1`:                             "has no column [Nope]",
		`'Other'[X] = 1`:                         "belongs to another table",
		`LOOKUPVALUE([Territory], [Email], "x")`: "LOOKUPVALUE is not supported",
		`RELATED(Other[X]) = 1`:                  "belongs to another table",
		`[Territory] = "West" "junk"`:            "unsupported DAX from",
		`[Territory] IN {"West"`:                 `expected "}"`,
		`[Territory] IN ("West")`:                `expected "{"`,
		`[Territory] = "West`:                    "unterminated",
		`NOT()`:                                  "NOT expects 1 argument",
		`AND(TRUE())`:                            "AND expects 2 argument",
		`TRUE(1)`:                                "TRUE expects 0 argument",
		`USERPRINCIPALNAME`:                      "unsupported \"USERPRINCIPALNAME\"",
		`= 1`:                                    "unexpected",
		``:                                       "ends where a value was expected",
		`[Amount] = 1.2.3`:                       "invalid number",
		`([Territory] = "West"`:                  `expected ")"`,
		`OR(TRUE(), FALSE()`:                     `expected ")"`,
		`TRUE() || [Nope]`:                       "has no column [Nope]",
		`TRUE() && [Nope]`:                       "has no column [Nope]",
		`[Territory] IN {"West", [Nope]}`:        "has no column [Nope]",
		`([Nope])`:                               "has no column [Nope]",
		`NOT [Nope]`:                             "has no column [Nope]",
	} {
		_, err := CompileRowFilter(m, "Store", expr)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", expr, err, want)
		}
	}
	if _, err := CompileRowFilter(m, "Nowhere", `TRUE()`); err == nil || !strings.Contains(err.Error(), "does not have") {
		t.Errorf("unknown table: err = %v", err)
	}
}

// An evaluation that cannot decide is an error, never a quiet FALSE: the caller
// refuses the query rather than dropping rows or keeping them.
func TestRowFilterEvaluationErrors(t *testing.T) {
	m := filterModel()
	row := Row{"Territory": "West", "Amount": 150.0, "Open": true}
	for _, expr := range []string{
		`[Territory] = 1`,             // text against a number
		`[Open] = "yes"`,              // a boolean against text
		`[Amount] = TRUE()`,           // a number against a boolean
		`[Territory]`,                 // text is not TRUE or FALSE
		`[Amount] && TRUE()`,          // nor is a number
		`TRUE() || [Territory]`,       // on either side
		`NOT [Territory]`,             // nor under NOT
		`[Territory] IN {1, 2}`,       // IN compares like =
		`NOT([Territory] = 1)`,        // an error inside NOT surfaces
		`([Territory] = 1) && TRUE()`, // …inside && on the left
		`TRUE() && ([Territory] = 1)`,
		`FALSE() || ([Territory] = 1)`,               // …and under || on the right
		`TRUE() && (TRUE() || [Territory] = 1)`,      // …inside parentheses
		`NOT [Territory] = 1`,                        // …under the NOT operator                // …and on the right
		`([Territory] = 1) IN {TRUE()}`,              // an IN over an erroring operand
		`[Territory] IN {"East", ([Territory] = 1)}`, // …or an erroring member
	} {
		f, err := CompileRowFilter(m, "Store", expr)
		if err != nil {
			t.Errorf("%s: compile: %v", expr, err)
			continue
		}
		if ok, err := f.Admits(row, SecurityEnv{}); err == nil {
			t.Errorf("%s: admits = %v with no error", expr, ok)
		}
	}
}
