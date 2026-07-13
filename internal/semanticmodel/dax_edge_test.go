package semanticmodel

import "testing"

// TestLexErrors covers the tokenizer's failure + edge paths.
func TestLexErrors(t *testing.T) {
	for _, s := range []string{"EVALUATE 'unterminated", "EVALUATE [oops", `EVALUATE "nope`, "EVALUATE #"} {
		if _, err := lex(s); err == nil {
			t.Errorf("lex(%q) should error", s)
		}
	}
	// Negative-number and decimal literals tokenize.
	if toks, err := lex("DIVIDE(-1.5, 2)"); err != nil || len(toks) == 0 {
		t.Fatalf("lex numbers: %v", err)
	}
}

// TestParseAndEvalEdges hits the remaining parser/evaluator error branches.
func TestParseAndEvalEdges(t *testing.T) {
	m, d := loadModel(t), loadData(t)
	cases := []string{
		"",                          // empty → not EVALUATE
		"EVALUATE",                  // missing table expr
		"EVALUATE SUMMARIZECOLUMNS", // missing '('
		"EVALUATE SUMMARIZECOLUMNS('Time'[FiscalYear]",               // unterminated
		`EVALUATE SUMMARIZECOLUMNS("name")`,                          // output name with no expr
		"EVALUATE SUMMARIZECOLUMNS(Time)",                            // group ref without [col]
		`EVALUATE SUMMARIZECOLUMNS("x", SUM([TotalUnits]))`,          // SUM of non-column
		`EVALUATE SUMMARIZECOLUMNS("x", DIVIDE(1))`,                  // DIVIDE arity
		`EVALUATE SUMMARIZECOLUMNS("x", COUNTROWS([TotalUnits]))`,    // COUNTROWS non-table
		`EVALUATE SUMMARIZECOLUMNS("x", NOPE(1))`,                    // unknown function
		`EVALUATE SUMMARIZECOLUMNS('NoTable'[c], "x", [TotalUnits])`, // unknown group table
		`EVALUATE SUMMARIZECOLUMNS("x", 'Store'[StoreId])`,           // bare column outside aggregation
	}
	for _, q := range cases {
		if _, err := Evaluate(m, d, q); err == nil {
			t.Errorf("%q: expected error", q)
		}
	}

	// A measure with a broken expression surfaces the parse error at eval time.
	broken := &Model{Name: "x", Tables: []Table{{
		Name:     "T",
		Columns:  []Column{{Name: "v", DataType: "int64"}},
		Measures: []Measure{{Name: "Bad", Expression: "SUM("}},
	}}}
	if _, err := Evaluate(broken, Data{"T": {{"v": 1.0}}}, `EVALUATE SUMMARIZECOLUMNS("x", [Bad])`); err == nil {
		t.Error("broken measure expression should error")
	}
	// Referencing a measure that doesn't exist.
	if _, err := Evaluate(broken, Data{"T": {{"v": 1.0}}}, `EVALUATE SUMMARIZECOLUMNS("x", [Ghost])`); err == nil {
		t.Error("missing measure should error")
	}
}

// TestValueHelpers covers the numeric/equality helpers' branches.
func TestValueHelpers(t *testing.T) {
	if toF(3) != 3 || toF("2.5") != 2.5 || toF(nil) != 0 || toF(true) != 0 {
		t.Error("toF conversions")
	}
	if !valEq("a", "a") || valEq(1.0, 2.0) || !valEq(1.0, 1.0) {
		t.Error("valEq")
	}
	m := loadModel(t)
	if m.Table("Store").Column("nope") != nil {
		t.Error("unknown column should be nil")
	}
	if m.RelationshipBetween("Store", "Time") != nil {
		t.Error("Store<->Time are not directly related")
	}
}
