package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/pipeline"
)

// THE STRUCTURAL TEST. Every activity type Fabric documents must produce a real
// outcome — run, or be refused by name — and never the dispatch default, which
// answers {"status":"Succeeded"} and would report work that never happened.
//
// It walks fabricActivityTypes rather than asserting a hand-written list of
// cases, so the next rename in Fabric's table fails HERE instead of silently
// becoming a fabricated success. That is the whole point: the previous bug was
// not twelve mistakes, it was one missing check repeated twelve times.
//
// The probe is deliberately empty typeProperties. A type that needs real
// properties may legitimately fail on them — that is a real outcome and passes
// this test. What must NOT happen is the success stub.
func TestEveryDocumentedFabricActivityTypeIsHandled(t *testing.T) {
	e := &pipelineExecutor{a: &API{}, wid: "ws"}
	resolve := func(raw json.RawMessage) (any, error) {
		if len(raw) == 0 {
			return nil, nil
		}
		var v any
		_ = json.Unmarshal(raw, &v)
		return v, nil
	}

	var fabricated []string
	for _, typ := range fabricActivityTypes {
		if handledByInterpreter[typ] {
			continue // control flow never reaches the executor
		}
		act := pipeline.Activity{Name: "probe", Type: typ, TypeProperties: json.RawMessage(`{}`)}
		out, err := func() (out map[string]any, err error) {
			defer func() {
				// A panic is a bug, but it is NOT a fabricated success, which
				// is what this test is about. Recorded as handled and left for
				// whatever test covers that type properly.
				if r := recover(); r != nil {
					out, err = nil, nil
				}
			}()
			return e.Execute(act, resolve)
		}()
		if err != nil {
			continue // refused or failed by name — a real outcome
		}
		if out != nil && out["status"] == "Succeeded" && out["activityType"] == typ {
			fabricated = append(fabricated, typ)
		}
	}

	if len(fabricated) > 0 {
		t.Fatalf(`%d Fabric activity types fall to the dispatch default and are reported
Succeeded having run nothing:

  %s

Each must either RUN or be refused BY NAME. If Fabric has added a type, add it
to fabricActivityTypes (from the DataPipelineActivityTypes table) and then give
it an outcome — adding it to the list alone will keep this test red, which is
intended.`, len(fabricated), strings.Join(fabricated, "\n  "))
	}
}

// handledByInterpreter are the control-flow types internal/pipeline executes
// itself, so they never reach the Executor at all. Listed explicitly rather
// than skipped by guesswork, because a type wrongly assumed to be control flow
// would be exempted from the check above and could then fabricate freely.
var handledByInterpreter = map[string]bool{
	"AppendVariable": true,
	"Fail":           true,
	"Filter":         true,
	"ForEach":        true,
	"IfCondition":    true,
	"SetVariable":    true,
	"Switch":         true,
	"Until":          true,
	"Wait":           true,
}

// The interpreter list must stay true: anything claimed as control flow has to
// be a case in internal/pipeline, or the exemption above is a hole.
func TestInterpreterExemptionsAreReal(t *testing.T) {
	for typ := range handledByInterpreter {
		p, err := pipeline.Parse([]byte(`{"properties":{"activities":[
			{"name":"a","type":"` + typ + `","typeProperties":{}}]}}`))
		if err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		// A nil Executor is the assertion: if the interpreter delegated this
		// type, it would dereference nil and panic. Reaching a result at all
		// proves it handled the type itself.
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s is exempted as control flow but was delegated to the Executor", typ)
				}
			}()
			_ = p.Run(nil, nil)
		}()
	}
}

// The three true renames must route to the SAME behaviour as the ADF name the
// emulator already handled, not to a refusal — otherwise "handled" would be
// satisfied by refusing everything, which passes the structural test while
// losing real capability.
func TestFabricRenamesReachTheSameBehaviourAsTheirADFNames(t *testing.T) {
	e := &pipelineExecutor{a: &API{}, wid: "ws"}
	resolve := func(raw json.RawMessage) (any, error) { return nil, nil }
	for _, pair := range []struct{ fabric, adf string }{
		{"AzureFunction", "AzureFunctionActivity"},
		{"KustoQueryLanguage", "AzureDataExplorerCommand"},
		{"RefreshDataFlow", "RefreshDataflow"},
	} {
		probe := func(typ string) string {
			act := pipeline.Activity{Name: "p", Type: typ, TypeProperties: json.RawMessage(`{}`)}
			_, err := e.Execute(act, resolve)
			if err == nil {
				return "<no error>"
			}
			// Strip the activity type, which legitimately differs, and compare
			// what the handler actually said.
			return strings.ReplaceAll(err.Error(), typ, "<TYPE>")
		}
		if got, want := probe(pair.fabric), probe(pair.adf); got != want {
			t.Errorf("%s took a different path from %s:\n  fabric: %s\n  adf:    %s",
				pair.fabric, pair.adf, got, want)
		}
	}
}

// AzureHDInsight is one Fabric type over five programs, so it must dispatch on
// typeProperties. Aliasing it by name would send a Hive script to whichever
// single handler was picked — the failure unrunnableactivities.go exists for.
func TestAzureHDInsightDispatchesOnItsProgram(t *testing.T) {
	e := &pipelineExecutor{a: &API{}, wid: "ws"}
	resolve := func(raw json.RawMessage) (any, error) {
		var v any
		_ = json.Unmarshal(raw, &v)
		return v, nil
	}
	run := func(props string) error {
		act := pipeline.Activity{Name: "p", Type: "AzureHDInsight", TypeProperties: json.RawMessage(props)}
		_, err := e.Execute(act, resolve)
		return err
	}
	// Each non-Spark program is refused under ITS OWN name, carrying the cause
	// already written for that engine.
	for program, adf := range map[string]string{
		"Hive": "HDInsightHive", "Pig": "HDInsightPig",
		"MapReduce": "HDInsightMapReduce", "Streaming": "HDInsightStreaming",
	} {
		err := run(`{"type":"` + program + `"}`)
		if err == nil {
			t.Fatalf("%s was not refused", program)
		}
		if !strings.Contains(err.Error(), adf) {
			t.Errorf("%s refused without naming %s: %v", program, adf, err)
		}
	}
	// A missing program is refused rather than guessed.
	if err := run(`{}`); err == nil || !strings.Contains(err.Error(), "names no program") {
		t.Errorf("a programless HDInsight activity should be refused, got %v", err)
	}
	// An unknown program is refused rather than routed anywhere.
	if err := run(`{"type":"Nonsense"}`); err == nil || !strings.Contains(err.Error(), "not one of") {
		t.Errorf("an unknown program should be refused, got %v", err)
	}
}

// Every refusal must name the activity and say why. A refusal a reader cannot
// act on is only marginally better than the fabricated success it replaced.
func TestNewRefusalsExplainThemselves(t *testing.T) {
	e := &pipelineExecutor{a: &API{}, wid: "ws"}
	resolve := func(raw json.RawMessage) (any, error) { return nil, nil }
	for _, typ := range []string{
		"Teams", "MicrosoftTeams", "Office365Email", "Email",
		"DataLakeAnalyticsScope", "SparkJobDefinition", "InvokeCopyJob",
		"PBISemanticModelRefresh",
	} {
		act := pipeline.Activity{Name: "notify", Type: typ, TypeProperties: json.RawMessage(`{}`)}
		_, err := e.Execute(act, resolve)
		if err == nil {
			t.Errorf("%s did not fail", typ)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, "notify") || !strings.Contains(msg, typ) {
			t.Errorf("%s: refusal names neither the activity nor the type: %s", typ, msg)
		}
		if !strings.Contains(msg, "Inactive") {
			t.Errorf("%s: refusal does not tell the reader how to skip the step: %s", typ, msg)
		}
	}
	// The two that are wiring-not-boundary must say so, or a reader will file
	// them as permanently unsupported.
	for _, typ := range []string{"SparkJobDefinition", "InvokeCopyJob"} {
		act := pipeline.Activity{Name: "x", Type: typ, TypeProperties: json.RawMessage(`{}`)}
		_, err := e.Execute(act, resolve)
		if err == nil || !strings.Contains(err.Error(), "THE EMULATOR CAN DO THIS") {
			t.Errorf("%s should state that the capability exists: %v", typ, err)
		}
	}
}
