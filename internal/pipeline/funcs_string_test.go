package pipeline

import "testing"

// TestExpressionSplitJoinIndexOf covers the string/collection functions Fabric
// publishes that the library used to answer with `unsupported function`.
//
// The case-insensitivity of indexOf/lastIndexOf is the part worth pinning:
// Fabric documents the search as case-insensitive, so `indexOf('Hello
// World','WORLD')` is 6 and not -1, and reaching for strings.Index would give
// the opposite answer on exactly the expressions people write.
func TestExpressionSplitJoinIndexOf(t *testing.T) {
	ctx := &evalContext{Variables: map[string]value{"path": "Files/landing/orders.csv"}}

	cases := []struct {
		expr string
		want value
	}{
		// split -> array, observed through the functions that consume one.
		{"@length(split('a,b,c', ','))", float64(3)},
		{"@first(split('a,b,c', ','))", "a"},
		{"@last(split('a,b,c', ','))", "c"},
		{"@split('a,b,c', ',')[1]", "b"},
		// No delimiter in the text: one element, the text itself.
		{"@length(split('abc', ','))", float64(1)},
		// A multi-character delimiter, and an empty trailing field.
		{"@join(split('a--b--', '--'), '|')", "a|b|"},

		// join
		{"@join(split('a,b,c', ','), '|')", "a|b|c"},
		{"@join(createArray('a','b'), ', ')", "a, b"},
		{"@join(createArray(1,2,3), '-')", "1-2-3"}, // items go through toString
		{"@join(createArray(), ',')", ""},
		{"@last(split(variables('path'), '/'))", "orders.csv"},

		// indexOf / lastIndexOf: 0-based, case-insensitive, -1 when absent.
		{"@indexOf('Hello World', 'WORLD')", float64(6)},
		{"@indexOf('Hello World', 'World')", float64(6)},
		{"@indexOf('hello world', 'World')", float64(6)},
		{"@indexOf('abc', 'z')", float64(-1)},
		{"@indexOf('a/b/a', 'A')", float64(0)},
		{"@lastIndexOf('a/b/a', 'A')", float64(4)},
		{"@lastIndexOf('a/b/a', 'z')", float64(-1)},
		{"@lastIndexOf('Hello World', 'o')", float64(7)},
		// A needle longer than the haystack is absent, not an error.
		{"@indexOf('ab', 'abc')", float64(-1)},
		// They compose with the rest of the library.
		{"@substring('Hello World', add(indexOf('Hello World','WORLD'),0), 5)", "World"},
		{"@greater(lastIndexOf(variables('path'), '/'), indexOf(variables('path'), '/'))", true},
	}
	for _, c := range cases {
		got, err := evalString(c.expr, ctx)
		if err != nil {
			t.Errorf("%s: %v", c.expr, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s = %v (%T), want %v (%T)", c.expr, got, got, c.want, c.want)
		}
	}

	// split really does return the package's array type, so array indexing and
	// ForEach see it as a collection rather than a stringified blob.
	v, err := evalString("@split('a,b,c', ',')", ctx)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	arr, ok := v.([]value)
	if !ok {
		t.Fatalf("split returned %T, want []value", v)
	}
	for i, want := range []string{"a", "b", "c"} {
		if arr[i] != want {
			t.Errorf("split(...)[%d] = %#v, want %q", i, arr[i], want)
		}
	}

	// Wrong arity or type is an error, not a guess: a definition that calls
	// these wrongly must fail its activity rather than quietly produce a value.
	for _, expr := range []string{
		"@split('a,b,c')",           // missing delimiter
		"@split('a', ',', 'x')",     // too many
		"@join(createArray('a'))",   // missing delimiter
		"@join('a,b', ',')",         // a string is not a collection
		"@join(1, ',')",             // nor is a number
		"@indexOf('abc')",           // missing searchText
		"@indexOf()",                //
		"@lastIndexOf('abc')",       //
		"@lastIndexOf('a','b','c')", // too many
	} {
		if got, err := evalString(expr, ctx); err == nil {
			t.Errorf("%s: expected an error, got %#v", expr, got)
		}
	}
}
