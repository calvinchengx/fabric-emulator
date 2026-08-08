package semanticmodel

import (
	"strings"
	"testing"
)

// The value-coercion helpers (`truthy`, `dstr`, `asNumber`, `toF`) decide what
// every operator and every aggregate does with a value that is not already a
// float64. They were the least-covered code in this package — 33-62% — and the
// unreached arms are not exotic: booleans reach them through comparisons, and
// the sized integer types reach them through Direct Lake, where rows arrive
// from parquet as typed Go integers rather than as JSON numbers.
//
// So these tests drive the arms through real DAX rather than calling the
// helpers directly, because a coercion that is right in isolation and unreached
// from a query proves nothing about the evaluator.

// A comparison yields a Go bool, so any operator applied to one lands in the
// boolean arms. Real DAX renders those as TRUE/FALSE and counts them as 1/0.
func TestDAXBooleanValuesConcatenateAndCount(t *testing.T) {
	for _, tc := range []struct {
		expr string
		want any
	}{
		// dstr's boolean arm: `&` on a comparison result.
		{`(1 = 1) & ""`, "TRUE"},
		{`(1 = 2) & ""`, "FALSE"},
		{`"units: " & (1 < 2)`, "units: TRUE"},
		// asNumber's boolean arm: TRUE is 1, FALSE is 0, in arithmetic.
		{`(1 = 1) + 1`, 2.0},
		{`(1 = 2) + 1`, 1.0},
		{`(1 = 1) * 7`, 7.0},
		// And the two compose: a boolean is 1, then rendered as a number.
		{`((1 = 1) + 1) & ""`, "2"},
	} {
		t.Run(tc.expr, func(t *testing.T) {
			if got := evalScalar(t, tc.expr); fmtLike(got) != fmtLike(tc.want) {
				t.Errorf("%s = %v (%T), want %v", tc.expr, got, got, tc.want)
			}
		})
	}
}

// truthy decides IF's condition. Only its boolean arm was exercised; a
// condition can also be blank, a number, or text — and DAX's rule is that
// FALSE, BLANK, 0 and "" are false, everything else true.
func TestDAXIfConditionCoercion(t *testing.T) {
	for _, tc := range []struct {
		expr string
		want float64
	}{
		{`IF(1, 10, 20)`, 10},            // non-zero number is true
		{`IF(0, 10, 20)`, 20},            // zero is false
		{`IF(-1, 10, 20)`, 10},           // non-zero, including negative
		{`IF(DIVIDE(1, 0), 10, 20)`, 20}, // BLANK is false
		{`IF("x", 10, 20)`, 10},          // non-empty text is true
		{`IF("", 10, 20)`, 20},           // empty text is false
	} {
		t.Run(tc.expr, func(t *testing.T) {
			if got := toF(evalScalar(t, tc.expr)); got != tc.want {
				t.Errorf("%s = %v, want %v", tc.expr, got, tc.want)
			}
		})
	}
}

// toF's sized-integer arms exist for Direct Lake: a parquet `int32` column
// arrives as a Go int32, not a float64, and a SUM that silently returned 0 for
// it would under-report a real table. Each type is summed with a second row so
// a dropped value shows as a wrong total rather than a plausible one.
func TestDAXSumsEveryNumericGoType(t *testing.T) {
	m := loadModel(t)
	for name, v := range map[string]any{
		"int":     int(41),
		"int8":    int8(41),
		"int16":   int16(41),
		"int32":   int32(41),
		"int64":   int64(41),
		"uint":    uint(41),
		"uint8":   uint8(41),
		"uint16":  uint16(41),
		"uint32":  uint32(41),
		"uint64":  uint64(41),
		"float32": float32(41),
		"float64": float64(41),
	} {
		t.Run(name, func(t *testing.T) {
			d := Data{"Sales": []Row{{"Units": v}, {"Units": 1}}}
			res, err := Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS("v", SUM(Sales[Units]))`)
			if err != nil {
				t.Fatalf("SUM over a %s column: %v", name, err)
			}
			if got := toF(res.Rows[0]["[v]"]); got != 42 {
				t.Errorf("SUM over a %s column = %v, want 42", name, got)
			}
		})
	}
}

// A value of no numeric type at all must refuse rather than count as zero —
// the fallback arm behind every coercion above, and the same rule the text
// refusal enforces for strings.
func TestDAXSumRefusesANonNumericGoValue(t *testing.T) {
	m := loadModel(t)
	d := Data{"Sales": []Row{{"Units": []int{1, 2}}, {"Units": 1}}}
	_, err := Evaluate(m, d, `EVALUATE SUMMARIZECOLUMNS("v", SUM(Sales[Units]))`)
	if err == nil {
		t.Fatal("a slice-valued column summed; it must refuse rather than coerce to 0")
	}
	// dstr's own fallback renders it, so the message names what it could not read.
	if want := "not a number"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not contain %q", err, want)
	}
}

// fmtLike compares values the way the other suites do: numbers by value rather
// than by Go type, everything else by its rendered form.
func fmtLike(v any) string {
	if f, ok := numeric(v); ok {
		return dstr(f)
	}
	return dstr(v)
}
