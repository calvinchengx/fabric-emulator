package api

import (
	"strings"
	"testing"
)

// parameterOverrides renders the caller's values as the python cell Fabric adds
// beneath the parameters cell. It is the last step before a notebook sees its
// parameters at all, so a wrong literal here is a wrong value in the run with
// nothing to indicate it.
func TestParameterOverridesRendersPythonLiterals(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   map[string]any
		want string
	}{
		{name: "string", in: map[string]any{"s": "text"}, want: `s = "text"` + "\n"},
		{name: "int", in: map[string]any{"i": 7}, want: "i = 7\n"},
		{name: "zero is not empty", in: map[string]any{"i": 0}, want: "i = 0\n"},
		{name: "float", in: map[string]any{"f": 1.5}, want: "f = 1.5\n"},
		// JSON true/false/null are not python literals; emitting them verbatim
		// would raise NameError inside the notebook.
		{name: "true", in: map[string]any{"b": true}, want: "b = True\n"},
		{name: "false", in: map[string]any{"b": false}, want: "b = False\n"},
		{name: "null", in: map[string]any{"n": nil}, want: "n = None\n"},
		{name: "empty", in: map[string]any{}, want: ""},
		// A quote in a value must not end the literal early.
		{name: "quote in string", in: map[string]any{"s": `a"b`}, want: `s = "a\"b"` + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parameterOverrides(tc.in); got != tc.want {
				t.Errorf("parameterOverrides(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A name that is not a python identifier cannot be assigned to. Emitting it
// anyway would be a SyntaxError that fails the whole run, so it is skipped.
func TestParameterOverridesSkipsNonIdentifiers(t *testing.T) {
	got := parameterOverrides(map[string]any{
		"ok":        1,
		"has-dash":  2,
		"has space": 3,
		"":          4,
	})
	if !strings.Contains(got, "ok = 1") {
		t.Errorf("a valid identifier was dropped: %q", got)
	}
	for _, bad := range []string{"has-dash", "has space"} {
		if strings.Contains(got, bad) {
			t.Errorf("emitted %q, which is not assignable python: %q", bad, got)
		}
	}
}

// Ordering is sorted so the same parameters render the same cell every run;
// map iteration order would make an otherwise identical run non-reproducible.
func TestParameterOverridesIsDeterministic(t *testing.T) {
	in := map[string]any{"c": 3, "a": 1, "b": 2}
	first := parameterOverrides(in)
	if want := "a = 1\nb = 2\nc = 3\n"; first != want {
		t.Fatalf("parameterOverrides = %q, want sorted %q", first, want)
	}
	for i := 0; i < 20; i++ {
		if got := parameterOverrides(in); got != first {
			t.Fatalf("run %d differed: %q vs %q", i, got, first)
		}
	}
}

// The executionData shape wraps each value as {value: ...}; the inner value is
// what the notebook should be assigned, not the wrapper.
func TestParameterOverridesUnwrapsValueObject(t *testing.T) {
	got := parameterOverrides(map[string]any{"p": map[string]any{"value": "inner", "type": "string"}})
	if want := `p = "inner"` + "\n"; got != want {
		t.Errorf("parameterOverrides = %q, want %q (the wrapper must not reach the notebook)", got, want)
	}
}

// notebookActivityOutput is what a pipeline expression reads. Fabric publishes
// no schema, so the Synapse sample's names are the contract, and exitValue
// rides alongside exitCode because real pipelines use both spellings.
func TestNotebookActivityOutputFields(t *testing.T) {
	out := notebookActivityOutput("job-1", "nb-1", "Completed", "42", "notebook-job-1")
	if out["status"] != "Completed" || out["notebookId"] != "nb-1" || out["jobInstanceId"] != "job-1" {
		t.Fatalf("top-level fields wrong: %+v", out)
	}
	result := out["result"].(map[string]any)
	for k, want := range map[string]any{
		"runId": "job-1", "runStatus": "Completed",
		"exitCode": "42", "exitValue": "42", "sessionId": "notebook-job-1",
	} {
		if result[k] != want {
			t.Errorf("result[%q] = %v, want %v", k, result[k], want)
		}
	}
}

// No engine, no session: reporting a sessionId would name a Spark session that
// never existed.
func TestNotebookActivityOutputOmitsSessionWhenNotRun(t *testing.T) {
	out := notebookActivityOutput("job-1", "nb-1", "Pending", "", "")
	result := out["result"].(map[string]any)
	if _, present := result["sessionId"]; present {
		t.Errorf("sessionId reported for a run no engine executed: %+v", result)
	}
	if result["runStatus"] != "Pending" {
		t.Errorf("runStatus = %v, want Pending", result["runStatus"])
	}
}

// The session id the activity reports must be the one the drive actually opens.
func TestNotebookSessionIDMatchesDrive(t *testing.T) {
	if got, want := notebookSessionID("abc"), "notebook-abc"; got != want {
		t.Errorf("notebookSessionID = %q, want %q", got, want)
	}
}
