package semanticmodel

import (
	"fmt"
	"strings"
	"testing"
)

// evalScalarErr runs one scalar DAX expression and returns its error, if any.
//
// SUMMARIZECOLUMNS with a single scalar output evaluates that output exactly
// once, which is the smallest harness that reaches evalFunc's families.
func evalScalarErr(t *testing.T, m *Model, d Data, expr string) error {
	t.Helper()
	_, err := Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS("v", `+expr+`)`)
	return err
}

// TestDAXFamilyErrorPaths pins the refusals of every DAX function, by MESSAGE
// rather than by "an error happened".
//
// The distinction is the point. `err != nil` passes when ABS() fails for the
// wrong reason -- a parse error, a missing table, an arity check that moved to
// a different function -- and a suite that only asserts non-nil cannot tell a
// working guard from a broken one. Every case below names the text it expects,
// so a refusal that changes meaning fails here rather than silently passing.
//
// These paths were entirely untested: splitting evalFunc into five families
// showed 107 uncovered blocks, and all but a handful were refusals like these.
func TestDAXFamilyErrorPaths(t *testing.T) {
	m, d := loadModel(t), loadData(t)

	cases := []struct {
		expr string
		want string
	}{
		// --- arity, one family at a time -------------------------------------
		{`SUM(1)`, "SUM expects a column reference"},
		{`COUNT(1)`, "COUNT expects a column reference"},
		{`DISTINCTCOUNT(1)`, "DISTINCTCOUNT expects a column reference"},
		{`AVERAGE(1)`, "AVERAGE expects a column reference"},
		{`COUNTROWS(1)`, "COUNTROWS expects a table"},

		{`ABS()`, "ABS expects 1 argument"},
		{`ABS(1, 2)`, "ABS expects 1 argument"},
		{`INT()`, "INT expects 1 argument"},
		{`SIGN()`, "SIGN expects 1 argument"},
		{`SQRT()`, "SQRT expects 1 argument"},
		{`LN()`, "LN expects 1 argument"},
		{`EXP()`, "EXP expects 1 argument"},
		{`LOG10()`, "LOG10 expects 1 argument"},
		{`PI(1)`, "PI expects 0 arguments"},
		{`ROUND(1)`, "ROUND expects 2 arguments"},
		{`POWER(1)`, "POWER expects 2 arguments"},
		{`MOD(1)`, "MOD expects 2 arguments"},
		{`FLOOR(1)`, "FLOOR expects 2 arguments"},
		{`CEILING(1)`, "CEILING expects 2 arguments"},
		{`QUOTIENT(1)`, "QUOTIENT expects 2 arguments"},
		{`DIVIDE(1)`, "DIVIDE expects 2 arguments"},
		{`LOG()`, "LOG expects 1 or 2 arguments"},
		{`TRUNC()`, "TRUNC expects 1 or 2 arguments"},

		{`ACOS()`, "ACOS expects 1 argument"},
		{`ASIN()`, "ASIN expects 1 argument"},
		{`ATAN()`, "ATAN expects 1 argument"},
		{`SIN()`, "SIN expects 1 argument"},
		{`COS()`, "COS expects 1 argument"},
		{`TAN()`, "TAN expects 1 argument"},
		{`DEGREES()`, "DEGREES expects 1 argument"},
		{`RADIANS()`, "RADIANS expects 1 argument"},

		{`DATE(1)`, "DATE expects 3 arguments"},
		{`TIME(1)`, "TIME expects 3 arguments"},
		{`EOMONTH(1)`, "EOMONTH expects 2 arguments"},
		{`EDATE(1)`, "EDATE expects 2 arguments"},

		{`BLANK(1)`, "BLANK expects 0 arguments"},
		{`ISBLANK()`, "ISBLANK expects 1 argument"},
		{`IF(1)`, "IF expects a condition and a value"},

		// --- domain refusals --------------------------------------------------
		// Each is a value the function is defined to REJECT rather than to
		// answer, so a silent number here would be a wrong answer, not a crash.
		{`ACOS(2)`, "ACOS argument must be between -1 and 1"},
		{`ACOS(-2)`, "ACOS argument must be between -1 and 1"},
		{`ASIN(2)`, "ASIN argument must be between -1 and 1"},
		{`ASIN(-2)`, "ASIN argument must be between -1 and 1"},
		{`SQRT(-1)`, "SQRT argument must be >= 0"},
		{`LN(0)`, "LN argument must be > 0"},
		{`LN(-1)`, "LN argument must be > 0"},
		{`LOG10(0)`, "LOG10 argument must be > 0"},
		{`LOG(0)`, "LOG argument must be > 0"},
		{`LOG(10, 1)`, "LOG base must be > 0 and not 1"},
		{`LOG(10, 0)`, "LOG base must be > 0 and not 1"},
		{`POWER(0, 0)`, "POWER(0, 0) is undefined"},
		{`MOD(1, 0)`, "MOD division by zero"},
		{`FLOOR(1, 0)`, "FLOOR division by zero"},
		{`QUOTIENT(1, 0)`, "QUOTIENT division by zero"},
		{`DATE(2020, 1, 0)`, "DATE day must be > 0"},
	}

	for _, c := range cases {
		err := evalScalarErr(t, m, d, c.expr)
		if err == nil {
			t.Errorf("%s: expected an error containing %q, got none", c.expr, c.want)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error = %q, want it to contain %q", c.expr, err, c.want)
		}
	}
}

// TestDAXFamilyArgumentErrorsPropagate: an argument that fails must fail the
// CALL, carrying the argument's own message rather than a generic one.
//
// These are the `if err != nil { return nil, err }` lines after every
// e.scalar(...) -- 52 in evalMath alone, none of them previously executed. A
// swallowed argument error is the dangerous shape: the function would answer
// a number computed from a value that was never valid.
//
// EOMONTH and EDATE take a VALID first argument here on purpose: they check
// that argument is a date before reading the second, so a bad first argument
// would mask the very propagation being tested.
func TestDAXFamilyArgumentErrorsPropagate(t *testing.T) {
	m, d := loadModel(t), loadData(t)
	cases := []struct{ expr, want string }{
		{`ABS(BLANK(1))`, "BLANK expects 0 arguments"},
		{`INT(BLANK(1))`, "BLANK expects 0 arguments"},
		{`SIGN(BLANK(1))`, "BLANK expects 0 arguments"},
		{`SQRT(BLANK(1))`, "BLANK expects 0 arguments"},
		{`LN(BLANK(1))`, "BLANK expects 0 arguments"},
		{`EXP(BLANK(1))`, "BLANK expects 0 arguments"},
		{`LOG10(BLANK(1))`, "BLANK expects 0 arguments"},
		{`ACOS(BLANK(1))`, "BLANK expects 0 arguments"},
		{`ASIN(BLANK(1))`, "BLANK expects 0 arguments"},
		{`ATAN(BLANK(1))`, "BLANK expects 0 arguments"},
		{`SIN(BLANK(1))`, "BLANK expects 0 arguments"},
		{`COS(BLANK(1))`, "BLANK expects 0 arguments"},
		{`TAN(BLANK(1))`, "BLANK expects 0 arguments"},
		{`DEGREES(BLANK(1))`, "BLANK expects 0 arguments"},
		{`RADIANS(BLANK(1))`, "BLANK expects 0 arguments"},
		{`ISBLANK(BLANK(1))`, "BLANK expects 0 arguments"},
		{`LOG(BLANK(1))`, "BLANK expects 0 arguments"},
		{`TRUNC(BLANK(1))`, "BLANK expects 0 arguments"},
		{`YEAR(BLANK(1))`, "BLANK expects 0 arguments"},
		{`MONTH(BLANK(1))`, "BLANK expects 0 arguments"},
		{`DAY(BLANK(1))`, "BLANK expects 0 arguments"},
		{`HOUR(BLANK(1))`, "BLANK expects 0 arguments"},
		{`MINUTE(BLANK(1))`, "BLANK expects 0 arguments"},
		{`SECOND(BLANK(1))`, "BLANK expects 0 arguments"},
		{`WEEKDAY(BLANK(1))`, "BLANK expects 0 arguments"},
		{`WEEKNUM(BLANK(1))`, "BLANK expects 0 arguments"},
		{`ROUND(BLANK(1), 1)`, "BLANK expects 0 arguments"},
		{`ROUND(1, BLANK(1))`, "BLANK expects 0 arguments"},
		{`POWER(BLANK(1), 1)`, "BLANK expects 0 arguments"},
		{`POWER(1, BLANK(1))`, "BLANK expects 0 arguments"},
		{`MOD(BLANK(1), 1)`, "BLANK expects 0 arguments"},
		{`MOD(1, BLANK(1))`, "BLANK expects 0 arguments"},
		{`FLOOR(BLANK(1), 1)`, "BLANK expects 0 arguments"},
		{`FLOOR(1, BLANK(1))`, "BLANK expects 0 arguments"},
		{`CEILING(BLANK(1), 1)`, "BLANK expects 0 arguments"},
		{`CEILING(1, BLANK(1))`, "BLANK expects 0 arguments"},
		{`QUOTIENT(BLANK(1), 1)`, "BLANK expects 0 arguments"},
		{`QUOTIENT(1, BLANK(1))`, "BLANK expects 0 arguments"},
		{`DIVIDE(BLANK(1), 1)`, "BLANK expects 0 arguments"},
		{`DIVIDE(1, BLANK(1))`, "BLANK expects 0 arguments"},
		{`EOMONTH(BLANK(1), 1)`, "BLANK expects 0 arguments"},
		{`EOMONTH(DATE(2020, 1, 1), BLANK(1))`, "BLANK expects 0 arguments"},
		{`EDATE(BLANK(1), 1)`, "BLANK expects 0 arguments"},
		{`EDATE(DATE(2020, 1, 1), BLANK(1))`, "BLANK expects 0 arguments"},
		{`DATE(BLANK(1), 1, 1)`, "BLANK expects 0 arguments"},
		{`DATE(1, BLANK(1), 1)`, "BLANK expects 0 arguments"},
		{`DATE(1, 1, BLANK(1))`, "BLANK expects 0 arguments"},
		{`TIME(BLANK(1), 1, 1)`, "BLANK expects 0 arguments"},
		{`TIME(1, BLANK(1), 1)`, "BLANK expects 0 arguments"},
		{`TIME(1, 1, BLANK(1))`, "BLANK expects 0 arguments"},
	}
	for _, c := range cases {
		err := evalScalarErr(t, m, d, c.expr)
		if err == nil {
			t.Errorf("%s: an argument error was swallowed; expected %q", c.expr, c.want)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error = %q, want it to contain %q", c.expr, err, c.want)
		}
	}
}

// TestDAXBlankPropagates covers the `if a == nil { return nil, nil }` guard.
//
// Only these functions carry it. BLANK in gives BLANK out -- not 0, which
// would be a wrong NUMBER rather than a missing one, and not an error.
// SUMMARIZECOLUMNS drops a row whose only output is blank, so blank is 0 rows.
func TestDAXBlankPropagates(t *testing.T) {
	m, d := loadModel(t), loadData(t)
	exprs := []string{
		`ABS(BLANK())`,
		`ASIN(BLANK())`,
		`ATAN(BLANK())`,
		`DEGREES(BLANK())`,
		`INT(BLANK())`,
		`RADIANS(BLANK())`,
		`SIGN(BLANK())`,
		`SIN(BLANK())`,
		`SQRT(BLANK())`,
		`TAN(BLANK())`,
		`TRUNC(BLANK())`,
		`CEILING(BLANK(), 1)`,
		`FLOOR(BLANK(), 1)`,
		`MOD(BLANK(), 1)`,
		`POWER(BLANK(), 1)`,
		`QUOTIENT(BLANK(), 1)`,
		`ROUND(BLANK(), 1)`,
		`EDATE(BLANK(), 1)`,
		`EOMONTH(BLANK(), 1)`,
	}
	for _, expr := range exprs {
		res, err := Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS("v", `+expr+`)`)
		if err != nil {
			t.Errorf("%s: BLANK should propagate, not fail: %v", expr, err)
			continue
		}
		if len(res.Rows) != 0 {
			t.Errorf("%s: want a blank result (0 rows), got %d", expr, len(res.Rows))
		}
	}
}

// TestDAXBlankCoercesToZero is the OTHER half, and the reason the guard above
// is per-function rather than global: everything without it treats BLANK as 0,
// which is Desktop's coercion rule. Measured, not assumed -- each value below
// was read off the evaluator before being pinned here.
//
// LN, LOG and LOG10 land in this group too, and 0 is outside their domain, so
// BLANK reaches them as a refusal rather than a number.
func TestDAXBlankCoercesToZero(t *testing.T) {
	m, d := loadModel(t), loadData(t)
	for _, c := range []struct{ expr, want string }{
		{`EXP(BLANK())`, "1"},
		{`COS(BLANK())`, "1"},
		{`DIVIDE(BLANK(), 1)`, "0"},
		{`ISBLANK(BLANK())`, "true"},
	} {
		res, err := Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS("v", `+c.expr+`)`)
		if err != nil {
			t.Errorf("%s: unexpected error %v", c.expr, err)
			continue
		}
		if len(res.Rows) != 1 {
			t.Errorf("%s: want 1 row, got %d", c.expr, len(res.Rows))
			continue
		}
		if got := fmt.Sprint(res.Rows[0]["[v]"]); got != c.want {
			t.Errorf("%s = %s, want %s", c.expr, got, c.want)
		}
	}
	// BLANK becomes 0, and 0 is outside the log domain.
	for _, e := range []string{"LN(BLANK())", "LOG(BLANK())", "LOG10(BLANK())"} {
		if err := evalScalarErr(t, m, d, e); err == nil ||
			!strings.Contains(err.Error(), "must be > 0") {
			t.Errorf("%s: error = %v, want a domain refusal", e, err)
		}
	}
}

// TestDAXNonNumericArgumentsRefuse covers arithNum: the LAST uncovered shape
// in the five families, and the one that matters most.
//
// Every one of these once went through a coercion that returned 0 for anything
// it could not read, so ABS of a text column answered a confident zero. A
// wrong number is worse than an error, because a caller acts on it. The
// message names the function AND the offending value so the query can be
// fixed without bisecting it.
func TestDAXNonNumericArgumentsRefuse(t *testing.T) {
	m, d := loadModel(t), loadData(t)
	cases := []struct{ expr, want string }{
		{`ABS("x")`, `cannot apply "ABS" to the text value "x"`},
		{`INT("x")`, `cannot apply "INT" to the text value "x"`},
		{`SIGN("x")`, `cannot apply "SIGN" to the text value "x"`},
		{`SQRT("x")`, `cannot apply "SQRT" to the text value "x"`},
		{`LN("x")`, `cannot apply "LN" to the text value "x"`},
		{`EXP("x")`, `cannot apply "EXP" to the text value "x"`},
		{`LOG10("x")`, `cannot apply "LOG10" to the text value "x"`},
		{`LOG("x")`, `cannot apply "LOG" to the text value "x"`},
		{`TRUNC("x")`, `cannot apply "TRUNC" to the text value "x"`},
		{`ACOS("x")`, `cannot apply "ACOS" to the text value "x"`},
		{`ASIN("x")`, `cannot apply "ASIN" to the text value "x"`},
		{`ATAN("x")`, `cannot apply "ATAN" to the text value "x"`},
		{`SIN("x")`, `cannot apply "SIN" to the text value "x"`},
		{`COS("x")`, `cannot apply "COS" to the text value "x"`},
		{`TAN("x")`, `cannot apply "TAN" to the text value "x"`},
		{`DEGREES("x")`, `cannot apply "DEGREES" to the text value "x"`},
		{`RADIANS("x")`, `cannot apply "RADIANS" to the text value "x"`},
		{`ROUND("x", 1)`, `cannot apply "ROUND" to the text value "x"`},
		{`ROUND(1, "x")`, `cannot apply "ROUND" to the text value "x"`},
		{`POWER("x", 1)`, `cannot apply "POWER" to the text value "x"`},
		{`POWER(1, "x")`, `cannot apply "POWER" to the text value "x"`},
		{`MOD("x", 1)`, `cannot apply "MOD" to the text value "x"`},
		{`MOD(1, "x")`, `cannot apply "MOD" to the text value "x"`},
		{`FLOOR("x", 1)`, `cannot apply "FLOOR" to the text value "x"`},
		{`FLOOR(1, "x")`, `cannot apply "FLOOR" to the text value "x"`},
		{`CEILING("x", 1)`, `cannot apply "CEILING" to the text value "x"`},
		{`CEILING(1, "x")`, `cannot apply "CEILING" to the text value "x"`},
		{`QUOTIENT("x", 1)`, `cannot apply "QUOTIENT" to the text value "x"`},
		{`QUOTIENT(1, "x")`, `cannot apply "QUOTIENT" to the text value "x"`},
		{`DIVIDE("x", 1)`, `cannot apply "DIVIDE" to the text value "x"`},
		{`DIVIDE(1, "x")`, `cannot apply "DIVIDE" to the text value "x"`},
		{`DATE("x", 1, 1)`, `cannot apply "DATE" to the text value "x"`},
		{`DATE(1, "x", 1)`, `cannot apply "DATE" to the text value "x"`},
		{`DATE(1, 1, "x")`, `cannot apply "DATE" to the text value "x"`},
		{`TIME("x", 1, 1)`, `cannot apply "TIME" to the text value "x"`},
		{`TIME(1, "x", 1)`, `cannot apply "TIME" to the text value "x"`},
		{`TIME(1, 1, "x")`, `cannot apply "TIME" to the text value "x"`},
		// A result that leaves the reals is refused rather than returned as +Inf,
		// which would serialise as a number no client could interpret.
		{`POWER(10, 10000)`, "POWER result is not a number"},
		{`EXP(10000)`, "EXP result is not a number"},
		// The date family reports its own refusal before arithNum sees the value.
		{`WEEKDAY("x")`, "WEEKDAY expects a date"},
		{`WEEKNUM("x")`, "WEEKNUM expects a date"},
		{`EOMONTH("x", 1)`, "EOMONTH expects a date"},
		{`EDATE("x", 1)`, "EDATE expects a date"},
	}
	for _, c := range cases {
		err := evalScalarErr(t, m, d, c.expr)
		if err == nil {
			t.Errorf("%s: a non-numeric argument was accepted; expected %q", c.expr, c.want)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error = %q, want it to contain %q", c.expr, err, c.want)
		}
	}
}

// TestDAXOptionalSecondArgumentErrors reaches the guards inside the
// `if len(fc.args) == 2` branches -- the optional second argument of LOG,
// TRUNC, WEEKDAY and WEEKNUM. A one-argument call skips the branch entirely,
// so the whole first table above never entered it.
func TestDAXOptionalSecondArgumentErrors(t *testing.T) {
	m, d := loadModel(t), loadData(t)
	for _, c := range []struct{ expr, want string }{
		{`LOG(10, BLANK(1))`, "BLANK expects 0 arguments"},
		{`LOG(10, "x")`, `cannot apply "LOG" to the text value "x"`},
		{`TRUNC(1.5, BLANK(1))`, "BLANK expects 0 arguments"},
		{`TRUNC(1.5, "x")`, `cannot apply "TRUNC" to the text value "x"`},
		{`WEEKDAY(DATE(2020, 1, 1), BLANK(1))`, "BLANK expects 0 arguments"},
		{`WEEKDAY(DATE(2020, 1, 1), "x")`, `cannot apply "WEEKDAY" to the text value "x"`},
		{`WEEKNUM(DATE(2020, 1, 1), BLANK(1))`, "BLANK expects 0 arguments"},
		{`WEEKNUM(DATE(2020, 1, 1), "x")`, `cannot apply "WEEKNUM" to the text value "x"`},
		{`EOMONTH(DATE(2020, 1, 1), "x")`, `cannot apply "EOMONTH" to the text value "x"`},
		{`EDATE(DATE(2020, 1, 1), "x")`, `cannot apply "EDATE" to the text value "x"`},
		// IF evaluates its condition first, so a failing condition fails the call.
		{`IF(BLANK(1), 1, 2)`, "BLANK expects 0 arguments"},
	} {
		err := evalScalarErr(t, m, d, c.expr)
		if err == nil {
			t.Errorf("%s: expected %q, got no error", c.expr, c.want)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error = %q, want it to contain %q", c.expr, err, c.want)
		}
	}
}

// TestDAXAggregateOverUnknownColumn reaches resolveColumn's refusal, which the
// aggregate family shares. The column parses as a reference and fails only
// when the model is consulted -- so it is a runtime refusal, not a parse one.
func TestDAXAggregateOverUnknownColumn(t *testing.T) {
	m, d := loadModel(t), loadData(t)
	for _, f := range []string{"SUM", "COUNT", "DISTINCTCOUNT", "AVERAGE"} {
		expr := f + "('Store'[NoSuchColumn])"
		if err := evalScalarErr(t, m, d, expr); err == nil {
			t.Errorf("%s: an unknown column was accepted", expr)
		}
	}
}

// The two remaining uncovered lines in these families are deliberate dead
// guards, and are left uncovered rather than reached by contortion:
//
//   LOG "result is not a number" -- out = Log(x)/Log(base) with x > 0 and
//   base > 0, base != 1, both already enforced above it. Log of a finite
//   positive float is finite, and no DAX literal can deliver +Inf as x:
//   POWER refuses the overflow first (see TestDAXNonNumericArgumentsRefuse).
//
//   TAN "division by zero" -- math.Tan never returns Inf for a float64,
//   because pi/2 is not representable, so TAN(PI()/2) is a large finite
//   number rather than an overflow. Verified: it returns a value, not an error.
//
// Both are cheap insurance against a future change to their inputs. Deleting
// them to reach 100% would remove a check to flatter a number.
