package semanticmodel

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// The DAX lexer used to have no case for any operator character, so `+`, `-`,
// `*`, `/`, `&` and every comparison failed at tokenisation — inline in a query
// and, more quietly, inside a stored measure. `DIVIDE([A], [B])` worked while
// `[A] / [B]` did not (issue #42).

// evalScalar evaluates one scalar expression through the whole pipeline
// (lex → parse → evaluate) as the single output of an ungrouped
// SUMMARIZECOLUMNS, and returns its value.
func evalScalar(t *testing.T, expr string) any {
	t.Helper()
	m, d := loadModel(t), loadData(t)
	res, err := Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS("v", `+expr+`)`)
	if err != nil {
		t.Fatalf("%s: %v", expr, err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("%s: %d rows, want 1", expr, len(res.Rows))
	}
	return res.Rows[0]["[v]"]
}

// Σ UnitsThisYear = 4900, Σ UnitsLastYear = 3800, Σ Units = 275000 over the
// eight seeded Sales rows.
func TestDAXInfixOperators(t *testing.T) {
	for _, tc := range []struct {
		expr string
		want any
	}{
		// Arithmetic over measures — the shape the issue reported.
		{"[Total Units This Year] + [Total Units Last Year]", 8700.0},
		{"[Total Units This Year] - [Total Units Last Year]", 1100.0},
		{"[Total Units Last Year] * 2", 7600.0},
		{"[Total Units This Year] / 2", 2450.0},

		// Precedence and associativity: each of these has a different value
		// under the wrong reading, so they pin the grammar rather than just
		// proving the characters lex.
		{"2 + 3 * 4", 14.0},    // not 20
		{"2 * 3 + 4", 10.0},    // not 14
		{"(2 + 3) * 4", 20.0},  // parentheses override
		{"10 - 3 - 2", 5.0},    // left-associative, not 9
		{"100 / 5 / 2", 10.0},  // left-associative, not 40
		{"1 + 2 = 3", true},    // comparison binds loosest
		{`1 & 2 = "12"`, true}, // ... looser than `&`
		{"2 * 3 & 4", "64"},    // ... which is looser than arithmetic
		{"-2 + 3", 1.0},        // unary minus binds tightest
		{"10 -5", 5.0},         // subtraction, not the literal -5
		{"+7", 7.0},            // unary plus is a no-op
		{"- [Total Units Last Year] + [Total Units This Year]", 1100.0},

		// Comparisons yield TRUE/FALSE.
		{"1 = 1", true},
		{"1 <> 1", false},
		{"1 < 2", true},
		{"2 <= 2", true},
		{"3 > 4", false},
		{"4 >= 4", true},
		{"[Total Units This Year] > [Total Units Last Year]", true},
		{`"Apr" < "Jan"`, true},

		// Concatenation: numbers lose the float tail, blank is the empty string.
		{`"a" & "b"`, "ab"},
		{`[TotalUnits] & " units"`, "275000 units"},
		{`DIVIDE(1, 0) & "x"`, "x"},

		// BLANK coerces to 0 in arithmetic and comparison.
		{"DIVIDE(1, 0) + 5", 5.0},
		{"DIVIDE(1, 0) = 0", true},

		// IF, the reported home of the failing `>` — a comparison is not much
		// use without something that consumes one.
		{"IF([Total Units This Year] > [Total Units Last Year], 1, 0)", 1.0},
		{"IF(1 = 2, 1, 2)", 2.0},
		{`IF([TotalUnits] > 0, [TotalUnits] / 1000, 0)`, 275.0},
	} {
		t.Run(tc.expr, func(t *testing.T) {
			if got := evalScalar(t, tc.expr); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("%s = %v (%T), want %v", tc.expr, got, got, tc.want)
			}
		})
	}
}

// IF with no else branch returns blank, and SUMMARIZECOLUMNS drops the
// all-blank group — the same path DIVIDE-by-zero takes.
func TestDAXIfWithoutElseIsBlank(t *testing.T) {
	m, d := loadModel(t), loadData(t)
	res, err := Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS("v", IF(1 = 2, 1))`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 0 {
		t.Errorf("blank IF should drop the group, got %v", res.Rows)
	}
}

func TestDAXOperatorErrors(t *testing.T) {
	m, d := loadModel(t), loadData(t)
	for _, tc := range []struct {
		query, want string
	}{
		// `/` by zero: real DAX yields Infinity, which has no JSON encoding, so
		// the emulator refuses instead of inventing a number.
		{`EVALUATE SUMMARIZECOLUMNS("v", [TotalUnits] / 0)`, "DIVIDE"},
		// Text that is not a number must not silently become zero.
		{`EVALUATE SUMMARIZECOLUMNS("v", "abc" + 1)`, "text value"},
		{`EVALUATE SUMMARIZECOLUMNS("v", 'Store'[Territory] + 1)`, "no single value"},
		// The shape someone reaches for first when building a label with `&`.
		// It is a genuine DAX error, not an operator gap, so the message has to
		// say so rather than read as "& is broken".
		{`EVALUATE SUMMARIZECOLUMNS('Store'[Territory], "v", "T: " & 'Store'[Territory])`, "group by Store[Territory]"},
		// Summing text used to return a confident 0, so `"FY" & SUM(text)` gave
		// a label reading "FY0" — a wrong answer, not an error.
		{`EVALUATE SUMMARIZECOLUMNS("v", SUM('Store'[Territory]))`, "is not a number"},
		{`EVALUATE SUMMARIZECOLUMNS("v", "FY" & SUM('Store'[Territory]))`, "cannot sum Store[Territory]"},
		// DIVIDE coerced the same way, and string literals as scalars are new
		// here, so this path is one this change opened.
		{`EVALUATE SUMMARIZECOLUMNS("v", DIVIDE("abc", 2))`, "text value"},
		// Malformed operator expressions.
		{`EVALUATE SUMMARIZECOLUMNS("v", 1 +)`, "expected"},
		{`EVALUATE SUMMARIZECOLUMNS("v", * 5)`, "no left-hand operand"},
		{`EVALUATE SUMMARIZECOLUMNS("v", (1 + 2)`, "unterminated"},
		{`EVALUATE SUMMARIZECOLUMNS("v", 2 ^ 3)`, "unexpected character"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			_, err := Evaluate(m, d, tc.query)
			if err == nil {
				t.Fatalf("%s was accepted; want an error", tc.query)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// A measure's expression is parsed on use, and the parser stopped at the first
// token it did not understand without complaining — so a measure it only half
// understood evaluated to the half it read. The tail must be an error, or the
// caller gets a plausible wrong number instead of a failure.
func TestDAXMeasureTailIsRefused(t *testing.T) {
	m, d := loadModel(t), loadData(t)
	m.Tables[0].Measures = append(m.Tables[0].Measures,
		Measure{Name: "Truncated", Expression: "[TotalUnits] SUM(Sales[Units])"})
	if _, err := Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS("v", [Truncated])`); err == nil {
		t.Fatal("a measure with an unparsed tail evaluated; it must be refused")
	}
}

// The witness the issue asks for, at evaluator level: a measure *defined* with
// an operator is only exercised when something queries it. `Units Growth Pct`
// nests `Units Delta`, so a stored operator expression is reached through
// another stored operator expression.
func TestDAXStoredOperatorMeasureRoundTrips(t *testing.T) {
	m := loadModel(t)
	if got := m.Measure("Units Delta").Expression; got != "[Total Units This Year] - [Total Units Last Year]" {
		t.Fatalf("fixture drifted: Units Delta = %q", got)
	}
	// Σ TY − Σ LY = 4900 − 3800; growth = 1100/3800 × 100.
	if got := evalScalar(t, "[Units Delta]"); toF(got) != 1100 {
		t.Errorf("[Units Delta] = %v, want 1100", got)
	}
	want := 1100.0 / 3800.0 * 100 // 28.947…; compare with a float tolerance
	if got := toF(evalScalar(t, "[Units Growth Pct]")); math.Abs(got-want) > 1e-9 {
		t.Errorf("[Units Growth Pct] = %v, want %v", got, want)
	}
}

// The bare-column message has to fit the column it names. Recommending SUM on a
// text column is how someone follows the advice and lands on "FY0"; recommending
// it on a numeric column is genuinely the fix.
func TestDAXBareColumnHintFitsTheColumnType(t *testing.T) {
	m, d := loadModel(t), loadData(t)
	for _, tc := range []struct {
		query, want   string
		wantAggregate bool
	}{
		{`EVALUATE SUMMARIZECOLUMNS('Store'[Territory], "v", "T: " & 'Store'[Territory])`, "group by Store[Territory]", false},
		{`EVALUATE SUMMARIZECOLUMNS("v", Sales[Units] * 2)`, "group by Sales[Units]", true},
	} {
		t.Run(tc.query, func(t *testing.T) {
			_, err := Evaluate(m, d, tc.query)
			if err == nil {
				t.Fatal("a bare column reference evaluated; it must be refused")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
			if got := strings.Contains(err.Error(), "SUM("); got != tc.wantAggregate {
				t.Errorf("error %q offers SUM = %v, want %v", err, got, tc.wantAggregate)
			}
		})
	}
}

// SUM still totals a column whose numbers arrived as strings — the refusal must
// reject text, not every value that is not already a float64.
func TestDAXSumParsesNumericStrings(t *testing.T) {
	m := loadModel(t)
	d := Data{"Sales": []Row{{"Units": "1.5"}, {"Units": 2}, {"Units": nil}}}
	res, err := Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS("v", SUM(Sales[Units]))`)
	if err != nil {
		t.Fatal(err)
	}
	if got := toF(res.Rows[0]["[v]"]); got != 3.5 {
		t.Errorf("SUM over mixed numeric strings = %v, want 3.5", got)
	}
}
