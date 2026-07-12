package pipeline

import (
	"encoding/json"
	"testing"
)

// recordExec is a test Executor that records the leaf activities it runs and
// returns a scripted output/error keyed by activity name.
type recordExec struct {
	seen    []string
	inputs  map[string]value
	fail    map[string]bool
	outputs map[string]map[string]any
}

func (e *recordExec) Execute(a Activity, resolve func(json.RawMessage) (value, error)) (map[string]any, error) {
	e.seen = append(e.seen, a.Name)
	// Resolve an "input" field if present, to prove expressions reach leaves.
	var tp map[string]json.RawMessage
	_ = json.Unmarshal(a.TypeProperties, &tp)
	if raw, ok := tp["input"]; ok {
		v, err := resolve(raw)
		if err != nil {
			return nil, err
		}
		if e.inputs == nil {
			e.inputs = map[string]value{}
		}
		e.inputs[a.Name] = v
	}
	if e.fail[a.Name] {
		return nil, errTest("boom")
	}
	if out, ok := e.outputs[a.Name]; ok {
		return out, nil
	}
	return map[string]any{"ok": true}, nil
}

type errTest string

func (e errTest) Error() string { return string(e) }

func mustRun(t *testing.T, def string, params map[string]value, exec Executor) *Result {
	t.Helper()
	p, err := Parse([]byte(def))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return p.Run(params, exec)
}

func byName(res *Result) map[string]ActivityRun {
	m := map[string]ActivityRun{}
	for _, a := range res.Activities {
		m[a.Name] = a // last wins (fine for these tests)
	}
	return m
}

// flakyExec fails an activity for its first failUntil[name] attempts, then
// succeeds — exercising the retry policy. It counts attempts per activity.
type flakyExec struct {
	failUntil map[string]int
	attempts  map[string]int
}

func (e *flakyExec) Execute(a Activity, resolve func(json.RawMessage) (value, error)) (map[string]any, error) {
	if e.attempts == nil {
		e.attempts = map[string]int{}
	}
	e.attempts[a.Name]++
	if e.attempts[a.Name] <= e.failUntil[a.Name] {
		return nil, errTest("transient")
	}
	return map[string]any{"attempt": e.attempts[a.Name]}, nil
}

func countRecords(res *Result, name string) int {
	n := 0
	for _, a := range res.Activities {
		if a.Name == name {
			n++
		}
	}
	return n
}

// TestRetryPolicySucceedsAfterRetries: a leaf failing its first two attempts
// succeeds on the third under policy.retry=2 — recorded once, with the
// retryAttempt count, not three duplicate runs.
func TestRetryPolicySucceedsAfterRetries(t *testing.T) {
	def := `{"properties":{"activities":[
        {"name":"flaky","type":"CustomLeaf","policy":{"retry":2,"retryIntervalInSeconds":30},"typeProperties":{}}
      ]}}`
	exec := &flakyExec{failUntil: map[string]int{"flaky": 2}}
	res := mustRun(t, def, nil, exec)
	if res.Status != StatusSucceeded {
		t.Fatalf("status = %q, want Succeeded", res.Status)
	}
	if got := exec.attempts["flaky"]; got != 3 {
		t.Fatalf("attempts = %d, want 3 (1 initial + 2 retries)", got)
	}
	if n := countRecords(res, "flaky"); n != 1 {
		t.Fatalf("record count = %d, want 1 (retries must not duplicate runs)", n)
	}
	if r := byName(res)["flaky"]; r.Retry != 2 {
		t.Fatalf("retryAttempt = %d, want 2", r.Retry)
	}
}

// TestRetryPolicyExhausted: retries run out and the activity (and pipeline)
// fail, with the retryAttempt count on the final record.
func TestRetryPolicyExhausted(t *testing.T) {
	def := `{"properties":{"activities":[
        {"name":"doomed","type":"CustomLeaf","policy":{"retry":1},"typeProperties":{}}
      ]}}`
	exec := &flakyExec{failUntil: map[string]int{"doomed": 99}}
	res := mustRun(t, def, nil, exec)
	if res.Status != StatusFailed {
		t.Fatalf("status = %q, want Failed", res.Status)
	}
	if got := exec.attempts["doomed"]; got != 2 {
		t.Fatalf("attempts = %d, want 2 (1 initial + 1 retry)", got)
	}
	if n := countRecords(res, "doomed"); n != 1 {
		t.Fatalf("record count = %d, want 1", n)
	}
	if r := byName(res)["doomed"]; r.Retry != 1 {
		t.Fatalf("retryAttempt = %d, want 1", r.Retry)
	}
}

// TestTimeoutFailsLongWait: a Wait whose virtual duration exceeds policy.timeout
// fails deterministically (100s > 30s).
func TestTimeoutFailsLongWait(t *testing.T) {
	def := `{"properties":{"activities":[
        {"name":"w","type":"Wait","policy":{"timeout":"0.00:00:30"},"typeProperties":{"waitTimeInSeconds":100}}
      ]}}`
	res := mustRun(t, def, nil, &recordExec{})
	if res.Status != StatusFailed {
		t.Fatalf("status = %q, want Failed (100s > 30s timeout)", res.Status)
	}
	if r := byName(res)["w"]; r.Status != StatusFailed {
		t.Fatalf("wait status = %q, want Failed", r.Status)
	}
}

// TestTimeoutAllowsShortWait: a Wait within its timeout succeeds (100s < 120s).
func TestTimeoutAllowsShortWait(t *testing.T) {
	def := `{"properties":{"activities":[
        {"name":"w","type":"Wait","policy":{"timeout":"0.00:02:00"},"typeProperties":{"waitTimeInSeconds":100}}
      ]}}`
	res := mustRun(t, def, nil, &recordExec{})
	if res.Status != StatusSucceeded {
		t.Fatalf("status = %q, want Succeeded (100s < 120s timeout)", res.Status)
	}
}

func TestParseTimeout(t *testing.T) {
	cases := map[string]float64{
		"":           0,
		"0.00:00:30": 30,
		"0.00:02:00": 120,
		"12:00:00":   43200,
		"0.12:00:00": 43200,
		"1.00:00:00": 86400,
		"7.00:00:00": 604800,
		"garbage":    0,
		"1:2":        0,
	}
	for in, want := range cases {
		if got := parseTimeout(in); got != want {
			t.Errorf("parseTimeout(%q) = %g, want %g", in, got, want)
		}
	}
}

func TestVariablesAndExpressions(t *testing.T) {
	def := `{"properties":{
      "parameters":{"env":{"type":"String","defaultValue":"dev"}},
      "variables":{"greeting":{"type":"String"},"count":{"type":"Integer"}},
      "activities":[
        {"name":"set","type":"SetVariable","typeProperties":{"variableName":"greeting","value":"@concat('hi-',pipeline().parameters.env)"}},
        {"name":"calc","type":"SetVariable","dependsOn":[{"activity":"set","dependencyConditions":["Succeeded"]}],
         "typeProperties":{"variableName":"count","value":"@add(mul(2,3),1)"}}
      ]}}`
	res := mustRun(t, def, map[string]value{"env": "prod"}, nil)
	if res.Status != StatusSucceeded {
		t.Fatalf("status=%s err=%s", res.Status, res.Error)
	}
	if res.Variables["greeting"] != "hi-prod" {
		t.Errorf("greeting=%v", res.Variables["greeting"])
	}
	if res.Variables["count"] != float64(7) {
		t.Errorf("count=%v", res.Variables["count"])
	}
}

func TestAppendVariableInForEach(t *testing.T) {
	def := `{"properties":{
      "variables":{"acc":{"type":"Array"}},
      "activities":[
        {"name":"loop","type":"ForEach","typeProperties":{
          "items":"@createArray('a','b','c')",
          "activities":[
            {"name":"app","type":"AppendVariable","typeProperties":{"variableName":"acc","value":"@toUpper(item())"}}
          ]}}
      ]}}`
	res := mustRun(t, def, nil, nil)
	if res.Status != StatusSucceeded {
		t.Fatalf("status=%s err=%s", res.Status, res.Error)
	}
	arr := toArray(res.Variables["acc"])
	if len(arr) != 3 || arr[0] != "A" || arr[2] != "C" {
		t.Errorf("acc=%v", res.Variables["acc"])
	}
}

func TestIfConditionBranches(t *testing.T) {
	def := `{"properties":{
      "parameters":{"n":{"type":"Integer","defaultValue":10}},
      "activities":[
        {"name":"branch","type":"IfCondition","typeProperties":{
          "expression":{"value":"@greater(pipeline().parameters.n, 5)","type":"Expression"},
          "ifTrueActivities":[{"name":"big","type":"WebActivity","typeProperties":{}}],
          "ifFalseActivities":[{"name":"small","type":"WebActivity","typeProperties":{}}]
        }}
      ]}}`
	exec := &recordExec{fail: map[string]bool{}}
	res := mustRun(t, def, nil, exec)
	if res.Status != StatusSucceeded {
		t.Fatalf("status=%s err=%s", res.Status, res.Error)
	}
	if len(exec.seen) != 1 || exec.seen[0] != "big" {
		t.Errorf("expected only 'big' to run, got %v", exec.seen)
	}
}

func TestUntilLoop(t *testing.T) {
	def := `{"properties":{
      "variables":{"i":{"type":"Integer","defaultValue":0}},
      "activities":[
        {"name":"until","type":"Until","typeProperties":{
          "expression":{"value":"@greaterOrEquals(variables('i'), 3)","type":"Expression"},
          "activities":[
            {"name":"inc","type":"SetVariable","typeProperties":{"variableName":"i","value":"@add(variables('i'),1)"}}
          ]}}
      ]}}`
	res := mustRun(t, def, nil, nil)
	if res.Status != StatusSucceeded {
		t.Fatalf("status=%s err=%s", res.Status, res.Error)
	}
	if res.Variables["i"] != float64(3) {
		t.Errorf("i=%v", res.Variables["i"])
	}
}

func TestSwitch(t *testing.T) {
	def := `{"properties":{
      "parameters":{"mode":{"type":"String","defaultValue":"b"}},
      "activities":[
        {"name":"sw","type":"Switch","typeProperties":{
          "on":{"value":"@pipeline().parameters.mode","type":"Expression"},
          "cases":[
            {"value":"a","activities":[{"name":"ca","type":"X","typeProperties":{}}]},
            {"value":"b","activities":[{"name":"cb","type":"X","typeProperties":{}}]}
          ],
          "defaultActivities":[{"name":"cd","type":"X","typeProperties":{}}]
        }}
      ]}}`
	exec := &recordExec{fail: map[string]bool{}}
	mustRun(t, def, nil, exec)
	if len(exec.seen) != 1 || exec.seen[0] != "cb" {
		t.Errorf("expected 'cb', got %v", exec.seen)
	}
}

func TestSwitchDefault(t *testing.T) {
	def := `{"properties":{
      "activities":[
        {"name":"sw","type":"Switch","typeProperties":{
          "on":"@string('z')",
          "cases":[{"value":"a","activities":[{"name":"ca","type":"X","typeProperties":{}}]}],
          "defaultActivities":[{"name":"cd","type":"X","typeProperties":{}}]
        }}
      ]}}`
	exec := &recordExec{fail: map[string]bool{}}
	mustRun(t, def, nil, exec)
	if len(exec.seen) != 1 || exec.seen[0] != "cd" {
		t.Errorf("expected default 'cd', got %v", exec.seen)
	}
}

func TestDependencyConditionsFailAndSkip(t *testing.T) {
	// b runs only if a Failed; c runs only if a Succeeded. a fails → b runs, c skipped.
	def := `{"properties":{"activities":[
        {"name":"a","type":"Copy","typeProperties":{}},
        {"name":"b","type":"Copy","dependsOn":[{"activity":"a","dependencyConditions":["Failed"]}],"typeProperties":{}},
        {"name":"c","type":"Copy","dependsOn":[{"activity":"a","dependencyConditions":["Succeeded"]}],"typeProperties":{}}
      ]}}`
	exec := &recordExec{fail: map[string]bool{"a": true}}
	res := mustRun(t, def, nil, exec)
	runs := byName(res)
	if runs["a"].Status != StatusFailed {
		t.Errorf("a=%s", runs["a"].Status)
	}
	if runs["b"].Status != StatusSucceeded {
		t.Errorf("b should run on a's failure, got %s", runs["b"].Status)
	}
	if runs["c"].Status != StatusSkipped {
		t.Errorf("c should be skipped, got %s", runs["c"].Status)
	}
	// Pipeline overall failed because a failed and nothing recovered it into success.
	if res.Status != StatusFailed {
		t.Errorf("pipeline status=%s", res.Status)
	}
}

func TestActivityOutputWiring(t *testing.T) {
	def := `{"properties":{
      "variables":{"seen":{"type":"String"}},
      "activities":[
        {"name":"lookup","type":"Lookup","typeProperties":{}},
        {"name":"use","type":"SetVariable","dependsOn":[{"activity":"lookup","dependencyConditions":["Succeeded"]}],
         "typeProperties":{"variableName":"seen","value":"@activity('lookup').output.rows"}}
      ]}}`
	exec := &recordExec{outputs: map[string]map[string]any{"lookup": {"rows": float64(42)}}}
	res := mustRun(t, def, nil, exec)
	if res.Status != StatusSucceeded {
		t.Fatalf("status=%s err=%s", res.Status, res.Error)
	}
	if res.Variables["seen"] != float64(42) {
		t.Errorf("seen=%v", res.Variables["seen"])
	}
}

func TestLeafInputResolution(t *testing.T) {
	def := `{"properties":{
      "parameters":{"table":{"type":"String","defaultValue":"sales"}},
      "activities":[
        {"name":"copy","type":"Copy","typeProperties":{"input":"@concat('Tables/',pipeline().parameters.table)"}}
      ]}}`
	exec := &recordExec{}
	res := mustRun(t, def, nil, exec)
	if res.Status != StatusSucceeded {
		t.Fatalf("status=%s err=%s", res.Status, res.Error)
	}
	if exec.inputs["copy"] != "Tables/sales" {
		t.Errorf("resolved input=%v", exec.inputs["copy"])
	}
}

func TestFailActivity(t *testing.T) {
	def := `{"properties":{"activities":[
        {"name":"boom","type":"Fail","typeProperties":{"message":"@concat('stop ','now')"}}
      ]}}`
	res := mustRun(t, def, nil, nil)
	if res.Status != StatusFailed {
		t.Fatalf("status=%s", res.Status)
	}
	if byName(res)["boom"].Error != "stop now" {
		t.Errorf("error=%q", byName(res)["boom"].Error)
	}
}

func TestFilterActivity(t *testing.T) {
	def := `{"properties":{
      "variables":{"kept":{"type":"String"}},
      "activities":[
        {"name":"filt","type":"Filter","typeProperties":{
          "items":"@createArray(1,2,3,4)","condition":"@greater(item(),2)"}},
        {"name":"len","type":"SetVariable","dependsOn":[{"activity":"filt","dependencyConditions":["Succeeded"]}],
         "typeProperties":{"variableName":"kept","value":"@string(length(activity('filt').output.value))"}}
      ]}}`
	res := mustRun(t, def, nil, nil)
	if res.Status != StatusSucceeded {
		t.Fatalf("status=%s err=%s", res.Status, res.Error)
	}
	if res.Variables["kept"] != "2" {
		t.Errorf("kept=%v", res.Variables["kept"])
	}
}

func TestCycleDetection(t *testing.T) {
	def := `{"properties":{"activities":[
        {"name":"a","type":"X","dependsOn":[{"activity":"b","dependencyConditions":["Succeeded"]}],"typeProperties":{}},
        {"name":"b","type":"X","dependsOn":[{"activity":"a","dependencyConditions":["Succeeded"]}],"typeProperties":{}}
      ]}}`
	res := mustRun(t, def, nil, &recordExec{})
	if res.Status != StatusFailed {
		t.Errorf("cycle should fail, got %s", res.Status)
	}
}

func TestInterpolationAndEscape(t *testing.T) {
	cases := map[string]struct {
		expr string
		want value
	}{
		"literal":      {"plain text", "plain text"},
		"interp":       {"x=@{add(1,2)}!", "x=3!"},
		"whole":        {"@toUpper('hi')", "HI"},
		"escape":       {"email @@ home", "email @ home"},
		"bool":         {"@equals(1,1)", true},
		"nested":       {"@if(greater(3,2),'y','n')", "y"},
		"coalesce":     {"@coalesce(null,null,'last')", "last"},
		"contains-arr": {"@contains(createArray('a','b'),'b')", true},
		"substring":    {"@substring('hello',1,3)", "ell"},
	}
	for name, c := range cases {
		got, err := evalString(c.expr, &evalContext{})
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: got %v (%T), want %v", name, got, got, c.want)
		}
	}
}

func TestBadExpressionFailsActivity(t *testing.T) {
	def := `{"properties":{"variables":{"v":{"type":"String"}},"activities":[
        {"name":"bad","type":"SetVariable","typeProperties":{"variableName":"v","value":"@nosuchfunc(1)"}}
      ]}}`
	res := mustRun(t, def, nil, nil)
	if res.Status != StatusFailed {
		t.Errorf("bad expr should fail, got %s", res.Status)
	}
}

func TestParseError(t *testing.T) {
	if _, err := Parse([]byte("{not json")); err == nil {
		t.Error("expected parse error")
	}
}

// TestCompletedAndSkippedConditions covers the Completed and Skipped
// dependency conditions and the skip-propagation chain.
func TestCompletedAndSkippedConditions(t *testing.T) {
	def := `{"properties":{"activities":[
        {"name":"a","type":"Copy","typeProperties":{}},
        {"name":"b","type":"Copy","dependsOn":[{"activity":"a","dependencyConditions":["Succeeded"]}],"typeProperties":{}},
        {"name":"c","type":"Copy","dependsOn":[{"activity":"b","dependencyConditions":["Skipped"]}],"typeProperties":{}},
        {"name":"d","type":"Copy","dependsOn":[{"activity":"a","dependencyConditions":["Completed"]}],"typeProperties":{}}
      ]}}`
	exec := &recordExec{fail: map[string]bool{"a": true}}
	res := mustRun(t, def, nil, exec)
	runs := byName(res)
	if runs["b"].Status != StatusSkipped {
		t.Errorf("b = %s, want Skipped", runs["b"].Status)
	}
	if runs["c"].Status != StatusSucceeded {
		t.Errorf("c (depends on b Skipped) = %s", runs["c"].Status)
	}
	if runs["d"].Status != StatusSucceeded {
		t.Errorf("d (depends on a Completed) = %s", runs["d"].Status)
	}
}

// TestForEachInnerFailurePropagates: a failing leaf inside ForEach fails the
// container and the pipeline.
func TestForEachInnerFailurePropagates(t *testing.T) {
	def := `{"properties":{"activities":[
        {"name":"loop","type":"ForEach","typeProperties":{
          "items":"@createArray('a','b')",
          "activities":[{"name":"leaf","type":"Copy","typeProperties":{}}]
        }}
      ]}}`
	exec := &recordExec{fail: map[string]bool{"leaf": true}}
	res := mustRun(t, def, nil, exec)
	if res.Status != StatusFailed {
		t.Fatalf("pipeline status = %s", res.Status)
	}
	if byName(res)["loop"].Status != StatusFailed {
		t.Errorf("ForEach container should be Failed")
	}
}

// TestUntilNonConvergence: an always-false condition trips the iteration cap
// and fails the activity instead of looping forever.
func TestUntilNonConvergence(t *testing.T) {
	def := `{"properties":{"activities":[
        {"name":"spin","type":"Until","typeProperties":{
          "expression":{"value":"@equals(1,2)","type":"Expression"},
          "activities":[{"name":"noop","type":"Wait","typeProperties":{"waitTimeInSeconds":0}}]
        }}
      ]}}`
	res := mustRun(t, def, nil, nil)
	if res.Status != StatusFailed {
		t.Fatalf("expected Until to fail on non-convergence, got %s", res.Status)
	}
}
