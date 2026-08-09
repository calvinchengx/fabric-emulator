package pipeline

import "testing"

// TestFunctionLibrary exercises the expression function set and value
// coercions through evalString (the real entry point).
func TestFunctionLibrary(t *testing.T) {
	ctx := &evalContext{
		Parameters: map[string]value{"p": "x"},
		Variables:  map[string]value{"v": float64(3)},
		Activities: map[string]value{"a": map[string]value{"output": map[string]value{"n": float64(9)}, "status": "Succeeded"}},
	}
	cases := []struct {
		expr string
		want value
	}{
		// strings
		{"@toLower('AbC')", "abc"},
		{"@trim('  hi  ')", "hi"},
		{"@replace('a-b-c','-','_')", "a_b_c"},
		{"@startsWith('hello','he')", true},
		{"@endsWith('hello','lo')", true},
		{"@length('abcd')", float64(4)},
		{"@guid()", "00000000-0000-0000-0000-000000000000"},
		// logic / comparison
		{"@lessOrEquals(2,2)", true},
		{"@less(1,2)", true},
		{"@greaterOrEquals(3,4)", false},
		{"@empty('')", true},
		{"@empty(createArray(1))", false},
		{"@contains('abcdef','cd')", true},
		{"@contains(createArray(1,2,3),4)", false},
		// math
		{"@sub(10,3)", float64(7)},
		{"@div(10,4)", float64(2.5)},
		{"@mod(10,3)", float64(1)},
		{"@max(1,7,3)", float64(7)},
		{"@min(5,2,9)", float64(2)},
		// conversions / arrays
		{"@int('42')", float64(42)},
		{"@float('3.5')", float64(3.5)},
		{"@bool('true')", true},
		{"@first(createArray('a','b'))", "a"},
		{"@last(createArray('a','b'))", "b"},
		{"@length(range(0,5))", float64(5)},
		{"@createArray(1,2,3)[1]", float64(2)},
		// context accessors
		{"@variables('v')", float64(3)},
		{"@pipeline().parameters.p", "x"},
		{"@activity('a').output.n", float64(9)},
		// coercions in interpolation
		{"n=@{add(1,2)} b=@{equals(1,1)} s=@{string(true)}", "n=3 b=true s=true"},
		{"@string(3.5)", "3.5"},
		{"@string(null)", ""},
	}
	for _, c := range cases {
		got, err := evalString(c.expr, ctx)
		if err != nil {
			t.Errorf("%s: %v", c.expr, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s = %v (%T), want %v", c.expr, got, got, c.want)
		}
	}
}

func TestExpressionErrors(t *testing.T) {
	ctx := &evalContext{Variables: map[string]value{}}
	bad := []string{
		"@div(1,0)",                   // divide by zero
		"@substring('hi',0,9)",        // out of range
		"@createArray(1)[5]",          // index out of range
		"@pipeline().parameters.nope", // missing member
		"@variables('missing')",       // unknown variable
		"@activity('none')",           // unknown activity
		"@item()",                     // item outside ForEach
		"@nope(1)",                    // unknown function
		"@equals(1)",                  // too few args -> recovered panic, not a crash
		"@(",                          // parse error
		"@1 2",                        // trailing tokens
		"@#bad",                       // bad character
	}
	for _, expr := range bad {
		if _, err := evalString(expr, ctx); err == nil {
			t.Errorf("%s: expected error", expr)
		}
	}
}

func TestContainsMap(t *testing.T) {
	ctx := &evalContext{Activities: map[string]value{
		"a": map[string]value{"output": map[string]value{"k": "v"}},
	}}
	got, err := evalString("@contains(activity('a').output,'k')", ctx)
	if err != nil || got != true {
		t.Fatalf("contains map = %v %v", got, err)
	}
}

// TestCoercionCarriesReaderWidths.
//
// Expression values are not all JSON-decoded literals. A Lookup over a Delta
// table or a Parquet file puts the WAREHOUSE READER's Go types into the row map
// verbatim, and toNumber listed only float64 and int — so every integer column
// coerced to 0. Measured before the fix:
//
//	toNumber(int16 2) = 0   toNumber(int32 3) = 0   toNumber(int64 4) = 0
//	toNumber(float32 1.5) = 0
//
// int32 and int64 are the common cases — a Delta int and a bigint — so
// `@activity('L').output.firstRow.amount` on a bigint column evaluated to 0 and
// every expression built on it was silently wrong. int16/float32 joined them
// when the reader learned Fabric's smallint and real widths.
func TestCoercionCarriesReaderWidths(t *testing.T) {
	for _, tc := range []struct {
		v    value
		want float64
	}{
		{int8(1), 1}, {int16(2), 2}, {int32(3), 3}, {int64(4), 4},
		{float32(1.5), 1.5}, {float64(2.5), 2.5}, {5, 5},
	} {
		if got := toNumber(tc.v); got != tc.want {
			t.Errorf("toNumber(%T %v) = %v; want %v", tc.v, tc.v, got, tc.want)
		}
	}
	// A non-zero integer is true — reading it as false is a wrong branch, not
	// merely a wrong number.
	if !toBool(int64(4)) || !toBool(int32(3)) || !toBool(float32(1.5)) {
		t.Error("toBool: a non-zero reader value must be true")
	}
	if toBool(int64(0)) || toBool(float32(0)) {
		t.Error("toBool: a zero reader value must be false")
	}
}

func TestCoercionEdges(t *testing.T) {
	if toNumber(true) != 1 {
		t.Error("toNumber(true)")
	}
	if toNumber("  5 ") != 5 {
		t.Error("toNumber trimmed string")
	}
	if toBool(float64(0)) {
		t.Error("toBool(0)")
	}
	if toBool(float64(2)) != true {
		t.Error("toBool(2)")
	}
	if toString(nil) != "" {
		t.Error("toString(nil)")
	}
	if length(map[string]value{"a": 1}) != 1 {
		t.Error("length(map)")
	}
	if length(float64(3)) != 0 {
		t.Error("length(number) should be 0")
	}
	if toArray("nope") != nil {
		t.Error("toArray(non-array)")
	}
}

// TestSafeNavigationAndTriggerEvent covers the `?.` operator and the
// TriggerEvent system object it exists for. Fabric's own event-trigger samples
// are written `@pipeline()?.TriggerEvent?.FileName`, and the point of the '?'
// is that the *same* definition must also run when started by hand — when
// there is no trigger event at all.
func TestSafeNavigationAndTriggerEvent(t *testing.T) {
	triggered := &evalContext{
		Parameters: map[string]value{"p": "x"},
		Trigger: map[string]value{
			"FileName": "orders.csv", "FolderPath": "Files/landing",
		},
	}
	manual := &evalContext{Parameters: map[string]value{"p": "x"}}

	cases := []struct {
		ctx  *evalContext
		expr string
		want value
	}{
		{triggered, "@pipeline()?.TriggerEvent?.FileName", "orders.csv"},
		{triggered, "@concat(pipeline()?.TriggerEvent?.FolderPath,'/',pipeline()?.TriggerEvent?.FileName)",
			"Files/landing/orders.csv"},
		// '?.' also works where '.' would: it is not a different lookup.
		{triggered, "@pipeline()?.parameters?.p", "x"},
		// Started by hand: TriggerEvent is absent, so the chain yields null
		// instead of failing, and interpolation renders it as empty.
		{manual, "@pipeline()?.TriggerEvent?.FileName", nil},
		{manual, "file=@{pipeline()?.TriggerEvent?.FileName}", "file="},
		// A missing member one level down is null too, not an error.
		{triggered, "@pipeline()?.TriggerEvent?.NoSuchField", nil},
	}
	for _, c := range cases {
		got, err := evalString(c.expr, c.ctx)
		if err != nil {
			t.Errorf("%s: %v", c.expr, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s = %v (%T), want %v", c.expr, got, got, c.want)
		}
	}

	// Plain '.' keeps failing loudly — safe navigation is opt-in, so a typo in
	// an ordinary expression is still an error rather than a silent null.
	if _, err := evalString("@pipeline().TriggerEvent.FileName", manual); err == nil {
		t.Error("plain '.' on a missing member did not fail")
	}
	if _, err := evalString("@pipeline()?.TriggerEvent?.", triggered); err == nil {
		t.Error("a dangling '?.' did not fail")
	}
}

// range()'s count comes from a pipeline definition and `make` reserves the
// capacity up front. A negative count panics `makeslice`, which evalExpr's
// recover turns into an error; a huge one does NOT panic — it allocates, and an
// OOM kill is a fatal that no per-request recover can catch. So the bound is the
// only thing standing between one malformed definition and the process, exactly
// as maxMeasureDepth is for a cyclic measure.
//
// Asserting the ERROR rather than timing the allocation is deliberate: a test
// that actually tried 2e9 would either pass by OOMing the runner or pass by
// having enough RAM, and neither outcome is about the guard.
func TestRangeRefusesAnUnboundedCount(t *testing.T) {
	ctx := &evalContext{Variables: map[string]value{}}
	for _, expr := range []string{
		"@range(0, -1)",         // makeslice would panic
		"@range(0, 100001)",     // one past the bound
		"@range(0, 2000000000)", // the OOM case CodeQL flagged
		"@range(0)",             // arity: indexing args[1] would panic
		"@range()",
	} {
		if _, err := evalString(expr, ctx); err == nil {
			t.Errorf("%s: expected an error, got none", expr)
		}
	}

	// The bound must not break the function it guards.
	for _, expr := range []string{"@range(0, 0)", "@range(5, 3)", "@range(0, 100000)"} {
		if _, err := evalString(expr, ctx); err != nil {
			t.Errorf("%s: refused a legitimate range: %v", expr, err)
		}
	}

	// And the values are still right, not merely non-erroring.
	v, err := evalString("@range(5, 3)", ctx)
	if err != nil {
		t.Fatal(err)
	}
	arr, ok := v.([]value)
	if !ok || len(arr) != 3 {
		t.Fatalf("range(5,3) = %#v", v)
	}
	for i, want := range []float64{5, 6, 7} {
		if arr[i] != want {
			t.Errorf("range(5,3)[%d] = %v, want %v", i, arr[i], want)
		}
	}
}
