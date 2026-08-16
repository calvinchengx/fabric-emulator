package semanticmodel

import (
	"strings"
	"testing"
)

// TestDAXFunctionArity drives every arity guard in evalFunc.
//
// WHY, and why this is not a coverage-chasing test. The Phase 3 evaluator grew
// one `case` per DAX function, and each opens with the same shape:
//
//	if len(fc.args) != N { return nil, fmt.Errorf("F expects N arguments") }
//
// Those guards were the single largest block of unreached code in the package —
// evalFunc sat at 78.2% almost entirely because of them. That matters beyond the
// number: an arity guard is the boundary between "unsupported call" and
// "silently evaluates the wrong thing". A `case` that forgot its guard would
// index fc.args out of range and panic the server, or worse, read arg[0] and
// return a plausible number for a call the user wrote wrongly.
//
// The expectations here are OURS, not Desktop's — this asserts the emulator's
// own contract for a malformed call, so a green run is real evidence rather
// than agreement between two readings of the same document. Desktop's verdicts
// on WELL-FORMED calls live in desktop_goldens.json, which is the opposite kind
// of test and must stay that way.
//
// Each case asserts the message names the function. A guard that fired with a
// neighbour's name would still error, still pass a bare "did it error" check,
// and still send whoever hit it to the wrong `case`.
func TestDAXFunctionArity(t *testing.T) {
	m, d := loadModel(t), loadData(t)

	// fn -> the calls that must be rejected. Grouped by the guard's shape so a
	// newly added function is easy to slot in beside its own kind.
	type badCall struct{ expr, wantMsg string }

	var cases []badCall
	add := func(exprs []string, msg string) {
		for _, e := range exprs {
			cases = append(cases, badCall{e, msg})
		}
	}

	// Exactly one argument.
	for _, f := range []string{
		"ACOS", "ABS", "LOG10", "INT", "SIGN", "ASIN", "ATAN",
		"SIN", "COS", "TAN", "DEGREES", "RADIANS", "SQRT", "LN", "EXP", "ISBLANK",
	} {
		add([]string{f + "()", f + "(1, 2)"}, f+" expects 1 argument")
	}

	// Exactly two arguments.
	for _, f := range []string{
		"ROUND", "POWER", "MOD", "FLOOR", "CEILING", "EOMONTH", "EDATE", "QUOTIENT",
	} {
		add([]string{f + "(1)", f + "(1, 2, 3)"}, f+" expects 2 arguments")
	}

	// DIVIDE takes an OPTIONAL third argument, so only 1 and 4+ are malformed.
	// See TestDivideAlternateResult for what the third argument does.
	add([]string{"DIVIDE(1)", "DIVIDE(1, 2, 3, 4)"}, "DIVIDE expects 2 arguments")

	// Exactly three arguments.
	for _, f := range []string{"DATE", "TIME"} {
		add([]string{f + "(1, 2)", f + "(1, 2, 3, 4)"}, f+" expects 3 arguments")
	}

	// Exactly zero arguments — the guard nobody expects to need until a caller
	// passes the thing they meant to wrap.
	add([]string{"PI(1)"}, "PI expects 0 arguments")
	add([]string{"BLANK(1)"}, "BLANK expects 0 arguments")

	// One or two: the optional-argument functions. Both ends, because a `< 1`
	// guard and a `> 2` guard are separate branches and a missing `> 2` would
	// silently ignore a third argument.
	add([]string{"LOG()", "LOG(1, 2, 3)"}, "LOG expects 1 or 2 arguments")
	add([]string{"TRUNC()", "TRUNC(1, 2, 3)"}, "TRUNC expects 1 or 2 arguments")

	// Aggregations want a reference, not a scalar — a distinct message because
	// the fix is different (pass a column, not "pass fewer arguments").
	for _, f := range []string{"SUM", "DISTINCTCOUNT", "AVERAGE", "COUNT", "MIN", "MAX"} {
		add([]string{f + "()"}, f+" expects a column reference")
	}
	add([]string{"COUNTROWS()"}, "COUNTROWS expects a table")
	add([]string{"IF(1)"}, "IF expects a condition and a value")

	if len(cases) < 60 {
		t.Fatalf("only %d arity cases — the table lost entries", len(cases))
	}

	for _, c := range cases {
		t.Run(c.expr, func(t *testing.T) {
			_, err := Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS("v", `+c.expr+`)`)
			if err == nil {
				t.Fatalf("%s: evaluated without error, want %q", c.expr, c.wantMsg)
			}
			if !strings.Contains(err.Error(), c.wantMsg) {
				t.Errorf("%s: error = %q, want it to contain %q", c.expr, err, c.wantMsg)
			}
		})
	}
}

// TestDivideAlternateResult pins DIVIDE's optional third argument.
//
// PROVENANCE, because it differs from the goldens next door. These expectations
// come from Microsoft's DAX reference for DIVIDE — "the value returned when
// division by zero results in an error", defaulting to BLANK — NOT from a
// Power BI Desktop capture. Nobody ran the msmdsrv oracle for this; the
// Windows VM is not in this loop. So this asserts the documented contract and
// nothing stronger, and it is deliberately NOT in desktop_goldens.json, whose
// entries all mean "Desktop answered this". Whoever next boots the VM should
// pin DIVIDE(1, 0, -1) there and delete this note.
//
// The bug it was written for: the guard admitted a third argument and the body
// never read it, so DIVIDE(x, 0, alt) returned BLANK instead of alt. There was
// no local symptom — the query parsed, evaluated, and answered wrongly.
func TestDivideAlternateResult(t *testing.T) {
	m, d := loadModel(t), loadData(t)

	for _, c := range []struct {
		expr string
		want any
	}{
		{"DIVIDE(10, 2)", 5.0},      // unaffected: denominator non-zero
		{"DIVIDE(10, 2, -1)", 5.0},  // alternate ignored when it does not apply
		{"DIVIDE(10, 0)", nil},      // no alternate → BLANK, the documented default
		{"DIVIDE(10, 0, -1)", -1.0}, // the regression: was BLANK
		{"DIVIDE(10, 0, 0)", 0.0},   // 0 is a value, not "absent": a len==3 check
		// ...must not degrade to a truthiness test on the alternate itself.
		{"DIVIDE(10, BLANK(), 7)", 7.0}, // blank denominator takes the alternate too
	} {
		res, err := Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS("v", `+c.expr+`)`)
		if err != nil {
			t.Errorf("%s: %v", c.expr, err)
			continue
		}
		if c.want == nil {
			// A lone blank output drops the row (see TestDAXErrorsAndEdges).
			if len(res.Rows) != 0 {
				t.Errorf("%s: got rows %v, want the row dropped (BLANK)", c.expr, res.Rows)
			}
			continue
		}
		if len(res.Rows) != 1 {
			t.Errorf("%s: got %d rows, want 1", c.expr, len(res.Rows))
			continue
		}
		if got := toF(res.Rows[0]["[v]"]); got != c.want {
			t.Errorf("%s: got %v, want %v", c.expr, got, c.want)
		}
	}
}
