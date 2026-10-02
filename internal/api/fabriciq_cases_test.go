package api

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
)

// cases/fabric-iq-tool-calls.json is the Fabric IQ tool calls this test and
// e2e/mcp-fabriciq/driver.py share: one call, as owner or viewer, and the answer
// it owes. This runs every case; the driver runs those that name it. The file's
// own "about" says what an expectation means, and scripts/casefiles.py is the
// Python reading of it, so the two evaluators below are pinned to one table in
// TestCaseExpectationsMeanWhatTheyMeanInPython.

const iqCasesFile = "../../cases/fabric-iq-tool-calls.json"

type iqCase struct {
	ID      string           `json:"id"`
	As      string           `json:"as"`
	Tool    string           `json:"tool"`
	Args    map[string]any   `json:"args"`
	Expect  []map[string]any `json:"expect"`
	Refused string           `json:"refused"`
	Why     string           `json:"why"`
}

func loadIQCases(t *testing.T) []iqCase {
	t.Helper()
	raw, err := os.ReadFile(iqCasesFile)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []iqCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) == 0 {
		t.Fatal("no cases: a suite that runs nothing passes")
	}
	return doc.Cases
}

// substitute replaces {name} in every string of v with ids[name].
func substitute(v any, ids map[string]string) any {
	switch x := v.(type) {
	case string:
		for name, real := range ids {
			x = strings.ReplaceAll(x, "{"+name+"}", real)
		}
		return x
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = substitute(e, ids)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = substitute(e, ids)
		}
		return out
	}
	return v
}

// missing is what a path that does not resolve yields: equal to nothing.
type missing struct{}

// atPath walks a decoded JSON document: a string step is a key, a number a
// list index (negative from the end), "*" collects the rest from every element.
func atPath(doc any, path []any) any {
	for n, step := range path {
		if step == "*" {
			list, ok := doc.([]any)
			if !ok {
				return missing{}
			}
			out := make([]any, len(list))
			for i, e := range list {
				out[i] = atPath(e, path[n+1:])
			}
			return out
		}
		switch s := step.(type) {
		case float64:
			list, ok := doc.([]any)
			i := int(s)
			if i < 0 {
				i += len(list)
			}
			if !ok || i < 0 || i >= len(list) {
				return missing{}
			}
			doc = list[i]
		case string:
			m, ok := doc.(map[string]any)
			v, has := m[s]
			if !ok || !has {
				return missing{}
			}
			doc = v
		default:
			return missing{}
		}
	}
	return doc
}

var expectOps = []string{"equals", "contains", "length", "order"}

// unmet says why doc fails the expectation, or "" when it holds.
func unmet(doc any, e map[string]any) string {
	var op string
	for _, o := range expectOps {
		if _, ok := e[o]; ok {
			if op != "" {
				op = ""
				break
			}
			op = o
		}
	}
	path, ok := e["at"].([]any)
	if op == "" || !ok || len(e) != 2 {
		return fmt.Sprintf("expectation %v must have `at` and exactly one of %v", e, expectOps)
	}
	want := e[op]
	got := atPath(doc, path)
	var holds bool
	switch op {
	case "equals":
		holds = sameJSON(got, want)
	case "contains":
		list, _ := got.([]any)
		for _, v := range list {
			holds = holds || sameJSON(v, want)
		}
	case "length":
		n, isNum := want.(float64)
		switch g := got.(type) {
		case []any:
			holds = isNum && float64(len(g)) == n
		case map[string]any:
			holds = isNum && float64(len(g)) == n
		case string:
			holds = isNum && float64(len(g)) == n
		}
	default:
		if want != "descending" {
			return fmt.Sprintf("order %q is not one this runner knows (descending)", want)
		}
		list, isList := got.([]any)
		holds = isList
		for i, v := range list {
			x, isNum := v.(float64)
			holds = holds && isNum && (i == 0 || list[i-1].(float64) >= x)
		}
	}
	if holds {
		return ""
	}
	shown := fmt.Sprintf("%#v", got)
	if _, gone := got.(missing); gone {
		shown = "nothing"
	}
	return fmt.Sprintf("at %v: want %s %#v, got %s", path, op, want, shown)
}

func sameJSON(a, b any) bool {
	if _, gone := a.(missing); gone {
		return false
	}
	return reflect.DeepEqual(a, b)
}

func TestFabricIQCases(t *testing.T) {
	f := newIQ(t)
	ids := map[string]string{"workspace": f.ws.ID, "model": f.model.ID, "report": f.report.ID}
	as := map[string]*auth.Principal{"owner": owner, "viewer": viewer}
	for _, c := range loadIQCases(t) {
		t.Run(c.ID, func(t *testing.T) {
			p := as[c.As]
			if p == nil {
				t.Fatalf("as %q: no such caller", c.As)
			}
			args := substitute(c.Args, ids).(map[string]any)
			text, isErr := iqCall(t, f.a, p, c.Tool, args)
			if c.Refused != "" {
				if !isErr || !strings.Contains(text, c.Refused) {
					t.Errorf("want a tool error containing %q, got isError=%v %s\nwhy: %s", c.Refused, isErr, text, c.Why)
				}
				return
			}
			if isErr {
				t.Fatalf("tool error %s\nwhy: %s", text, c.Why)
			}
			var doc any
			if err := json.Unmarshal([]byte(text), &doc); err != nil {
				t.Fatalf("%s: %v", text, err)
			}
			if len(c.Expect) == 0 {
				t.Fatal("a case with neither expect nor refused asserts nothing")
			}
			for _, e := range c.Expect {
				if why := unmet(doc, substitute(e, ids).(map[string]any)); why != "" {
					t.Errorf("%s\nwhy: %s", why, c.Why)
				}
			}
		})
	}
}

// The same table as python/tests/test_casefiles.py's: an expectation means one
// thing whichever runner reads it.
func TestCaseExpectationsMeanWhatTheyMeanInPython(t *testing.T) {
	var doc any
	_ = json.Unmarshal([]byte(`{"Count": 2, "Rows": [{"u": 9, "t": "West"}, {"u": 4.0, "t": "East"}],
	  "Flag": true, "None": null, "Pairs": [{"PK": "a", "FK": "b"}]}`), &doc)
	parse := func(s string) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatal(s, err)
		}
		return m
	}
	for _, e := range []string{
		`{"at": ["Count"], "equals": 2}`,
		`{"at": ["Count"], "equals": 2.0}`,
		`{"at": ["Rows", "*", "t"], "equals": ["West", "East"]}`,
		`{"at": ["Rows", -1, "t"], "equals": "East"}`,
		`{"at": ["Rows", "*", "t"], "contains": "East"}`,
		`{"at": ["Pairs"], "contains": {"FK": "b", "PK": "a"}}`,
		`{"at": ["Rows"], "length": 2}`,
		`{"at": ["Rows", "*", "u"], "order": "descending"}`,
		`{"at": ["Flag"], "equals": true}`,
		`{"at": ["None"], "equals": null}`,
	} {
		if why := unmet(doc, parse(e)); why != "" {
			t.Errorf("%s should hold: %s", e, why)
		}
	}
	for e, fragment := range map[string]string{
		`{"at": ["Count"], "equals": 3}`:                         "got 2",
		`{"at": ["Flag"], "equals": 1}`:                          "got true",
		`{"at": ["Missing"], "equals": null}`:                    "got nothing",
		`{"at": ["Rows", 5], "equals": null}`:                    "got nothing",
		`{"at": ["Count", "*"], "length": 0}`:                    "got nothing",
		`{"at": ["Rows", "x"], "equals": null}`:                  "got nothing",
		`{"at": ["Rows", "*", "t"], "contains": "North"}`:        `contains "North"`,
		`{"at": ["Pairs"], "contains": {"PK": "a"}}`:             "contains",
		`{"at": ["Rows", "*", "t"], "equals": ["East", "West"]}`: "equals",
		`{"at": ["Rows"], "length": 3}`:                          "length 3",
		`{"at": ["Rows", "*", "t"], "order": "descending"}`:      "order",
		`{"at": ["Rows", "*", "u"], "order": "ascending"}`:       "not one this runner knows",
		`{"at": ["Count"]}`:                                      "exactly one of",
		`{"at": ["Count"], "equals": 2, "length": 1}`:            "exactly one of",
		`{"equals": 2}`: "exactly one of",
	} {
		if why := unmet(doc, parse(e)); !strings.Contains(why, fragment) {
			t.Errorf("%s: %q, want it to say %q", e, why, fragment)
		}
	}
	if unmet(map[string]any{"r": []any{1.0, 2.0}}, map[string]any{"at": []any{"r"}, "order": "descending"}) == "" {
		t.Error("ascending numbers are not descending")
	}
}
