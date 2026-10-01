package pipeline

import "testing"

// TestExpressionSkipTakeUnionIntersection covers Fabric's collection functions,
// which the library used to answer with `unsupported function`.
//
// The parts worth pinning down are the ones a plain implementation gets wrong:
// a count past the end of a collection CLAMPS (skip/take are how definitions
// slice a lookup result of unknown length, so erroring there would fail working
// pipelines), while a negative count is an ERROR rather than a count from the
// other end; union/intersection keep first-seen order so their result can drive
// a ForEach; and over objects they merge and compare keys instead of treating
// the object as a one-item collection.
func TestExpressionSkipTakeUnionIntersection(t *testing.T) {
	ctx := &evalContext{Variables: map[string]value{
		"path": "Files/landing/orders.csv",
		"o1":   map[string]value{"env": "dev", "region": "westus"},
		"o2":   map[string]value{"env": "prod", "owner": "data"},
		"o3":   map[string]value{"env": "dev", "owner": "data"},
	}}

	cases := []struct {
		expr string
		want value
	}{
		// skip / take over arrays, observed through join and length.
		{"@join(take(createArray('a','b','c'), 2), ',')", "a,b"},
		{"@join(skip(createArray('a','b','c'), 1), ',')", "b,c"},
		{"@join(skip(createArray('a','b','c'), 0), ',')", "a,b,c"},
		{"@length(take(createArray('a','b','c'), 0))", float64(0)},
		// A count past the end clamps at both ends rather than erroring.
		{"@length(take(createArray(1,2,3), 10))", float64(3)},
		{"@length(skip(createArray(1,2,3), 10))", float64(0)},
		{"@length(take(createArray(), 5))", float64(0)},

		// skip / take over strings.
		{"@take('abcdef', 3)", "abc"},
		{"@skip('abcdef', 3)", "def"},
		{"@take('abc', 99)", "abc"},
		{"@skip('abc', 99)", ""},
		{"@take('abc', 0)", ""},
		// They compose with the string half of the library.
		{"@skip(variables('path'), add(lastIndexOf(variables('path'), '/'), 1))", "orders.csv"},
		{"@take(variables('path'), indexOf(variables('path'), '/'))", "Files"},

		// union over arrays: every item, duplicates dropped, first-seen order.
		{"@join(union(createArray('a','b'), createArray('b','c')), ',')", "a,b,c"},
		{"@join(union(createArray('b'), createArray('a')), ',')", "b,a"},
		{"@join(union(createArray('a','a'), createArray('a')), ',')", "a"},
		{"@join(union(createArray(1,2), createArray(2,3), createArray(3,4)), ',')", "1,2,3,4"},
		{"@length(union(createArray(), createArray()))", float64(0)},
		{"@join(union(createArray(), createArray('a')), ',')", "a"},

		// intersection over arrays: only items present in every one of them.
		{"@join(intersection(createArray('a','b','c'), createArray('b','c','d')), ',')", "b,c"},
		{"@length(intersection(createArray('a'), createArray('b')))", float64(0)},
		{"@join(intersection(createArray(1,2,3), createArray(2,3), createArray(3)), ',')", "3"},
		// Order comes from the first argument, and duplicates in it are dropped.
		{"@join(intersection(createArray('c','b','a'), createArray('a','b','c')), ',')", "c,b,a"},
		{"@join(intersection(createArray('a','a','b'), createArray('a','b')), ',')", "a,b"},
		{"@length(intersection(createArray('a'), createArray()))", float64(0)},

		// union over objects: keys merge, and a later argument wins.
		{"@length(union(variables('o1'), variables('o2')))", float64(3)},
		{"@union(variables('o1'), variables('o2')).env", "prod"},
		{"@union(variables('o1'), variables('o2')).region", "westus"},
		{"@union(variables('o1'), variables('o2')).owner", "data"},
		{"@union(variables('o1'), variables('o2'), variables('o3')).env", "dev"},

		// intersection over objects: keys whose values agree everywhere. 'env'
		// is in both o1 and o2, but with different values, so it is not shared.
		{"@length(intersection(variables('o1'), variables('o2')))", float64(0)},
		{"@length(intersection(variables('o1'), variables('o3')))", float64(1)},
		{"@intersection(variables('o1'), variables('o3')).env", "dev"},
		{"@length(intersection(variables('o2'), variables('o3')))", float64(1)},
		{"@intersection(variables('o2'), variables('o3')).owner", "data"},
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

	// The array results really are the package's collection type, so indexing,
	// first/last and ForEach see them as collections and not as a stringified
	// blob.
	for _, tc := range []struct {
		expr string
		want []string
	}{
		{"@take(createArray('a','b','c'), 2)", []string{"a", "b"}},
		{"@skip(createArray('a','b','c'), 1)", []string{"b", "c"}},
		{"@union(createArray('a'), createArray('b','a'))", []string{"a", "b"}},
		{"@intersection(createArray('a','b'), createArray('b'))", []string{"b"}},
	} {
		v, err := evalString(tc.expr, ctx)
		if err != nil {
			t.Errorf("%s: %v", tc.expr, err)
			continue
		}
		arr, ok := v.([]value)
		if !ok {
			t.Errorf("%s returned %T, want []value", tc.expr, v)
			continue
		}
		if len(arr) != len(tc.want) {
			t.Errorf("%s = %#v, want %v", tc.expr, arr, tc.want)
			continue
		}
		for i, want := range tc.want {
			if arr[i] != want {
				t.Errorf("%s[%d] = %#v, want %q", tc.expr, i, arr[i], want)
			}
		}
	}

	// take/skip must not alias their input: appending to the result of one is a
	// normal thing for the runtime to do, and it must not scribble on the array
	// the variable still holds.
	src := []value{"a", "b", "c"}
	ctx.Variables["src"] = src
	v, err := evalString("@take(variables('src'), 2)", ctx)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	_ = append(v.([]value), "clobber")
	if src[2] != "c" {
		t.Errorf("take aliased its input: src = %#v", src)
	}

	// Wrong arity, a negative count, or the wrong shape is an error rather than
	// a guess — a definition that calls these wrongly fails its activity.
	for _, expr := range []string{
		"@skip(createArray(1,2), -1)",             // negative counts are refused,
		"@take('abc', -2)",                        // not read as "from the end"
		"@skip(createArray(1))",                   // missing count
		"@take('abc')",                            //
		"@take('abc', 1, 2)",                      // too many
		"@skip()",                                 //
		"@skip(1, 2)",                             // a number is not a collection
		"@take(variables('o1'), 1)",               // nor is an object, for skip/take
		"@union(createArray(1))",                  // union needs two collections
		"@intersection(createArray(1))",           //
		"@union()",                                //
		"@intersection()",                         //
		"@union(createArray(1), 'abc')",           // a string is not a collection here
		"@intersection('ab', 'bc')",               //
		"@union(createArray(1), variables()) ",    // (bad inner call propagates)
		"@union(variables('o1'), createArray(1))", // objects and arrays do not mix
		"@intersection(createArray(1), variables('o1'))",
		"@union(variables('o1'), 'abc')",
	} {
		if got, err := evalString(expr, ctx); err == nil {
			t.Errorf("%s: expected an error, got %#v", expr, got)
		}
	}
}
