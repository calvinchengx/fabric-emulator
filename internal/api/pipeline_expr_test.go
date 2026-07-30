package api

// Expression-language edges driven through the real pipeline job API: escape
// and interpolation forms, member/index access, loose coercions, and the
// parser's error surface. Each bad expression fails its pipeline loudly.

import (
	"strings"
	"testing"
)

// TestPipelineExpressionEdges: a single pipeline whose SetVariable activities
// exercise the evaluator's less-traveled successful paths.
func TestPipelineExpressionEdges(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)

	exprs := []string{
		`@@esc`,                     // whole-value escape → literal "@esc"
		`x@@y`,                      // interpolated escape
		`a@{concat('{','}')}b`,      // braces inside an interpolated expression
		`@concat('it''s')`,          // escaped quote in a string literal
		`@{if(true,'x','y')}!`,      // interpolation with trailing text
		`@string(createArray(1,2)[1])`, // index into an array
		`@if(null,'a','b')`,         // null condition → false branch
		`@string(equals(coalesce(null,null),null))`, // coalesce of nothing
		`@string(and(true,false))`,
		`@string(and(true,true))`,
		`@string(or(false,true))`,
		`@string(or(false,false))`,
		`@string(equals(true,true))`,
		`@string(equals(true,false))`,
		`@string(greater(1,false))`, // bool coerced to 0
		`@string(empty(1))`,         // length of a number → 0
		`@string(empty())`,          // no argument at all
		`@string(equals(first(createArray()),null))`, // first of empty
		`@string(equals(last(createArray()),null))`,  // last of empty
		`@string(false)`,
		`@string(true)`,
	}
	var acts []string
	for i, e := range exprs {
		acts = append(acts, `{"name":"V`+string(rune('A'+i/10))+string(rune('0'+i%10))+
			`","type":"SetVariable","typeProperties":{"variableName":"s","value":"`+
			strings.ReplaceAll(e, `"`, `\"`)+`"}}`)
	}
	content := `{"properties":{"variables":{"s":{"type":"String"}},"activities":[` +
		strings.Join(acts, ",") + `]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
		t.Fatalf("expression edges = %s, want Completed; runs: %+v", s, runs)
	}
}

// TestPipelineExpressionErrors: parser and evaluator failures each fail the
// pipeline (not a panic, not a silent zero value).
func TestPipelineExpressionErrors(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)

	for name, expr := range map[string]string{
		"variables arity":      `@variables()`,
		"activity arity":       `@activity()`,
		"member of non-object": `@concat('x').a`,
		"member not a name":    `@pipeline().5`,
		"unterminated index":   `@createArray(1,2)[0`,
		"index of non-array":   `@concat('x')[0]`,
		"non-numeric index":    `@createArray(1)['x']`,
		"index out of range":   `@createArray(9)[99]`,
		"unknown identifier":   `@bogusident`,
		"unknown function":     `@nosuchfunc()`,
		"unterminated args":    `@concat('a'`,
		"missing comma":        `@concat('a' 'b')`,
		"empty expression":     `@`,
		"stray token":          `@)`,
		"number overflow":      `@greater(1e999,1)`,
	} {
		content := `{"properties":{"variables":{"s":{"type":"String"}},"activities":[
            {"name":"V","type":"SetVariable","typeProperties":{"variableName":"s","value":"` +
			strings.ReplaceAll(expr, `"`, `\"`) + `"}}
          ]}}`
		pl := createPipeline(t, st, ws.ID, content)
		_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
		if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Failed" {
			t.Errorf("%s (%s) = %s, want Failed", name, expr, s)
		}
	}
}
