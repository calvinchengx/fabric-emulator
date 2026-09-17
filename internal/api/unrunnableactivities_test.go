package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/pipeline"
)

func typedPipeline(actType string) string {
	return `{"properties":{"activities":[
      {"name":"Step","type":"` + actType + `","typeProperties":{}}]}}`
}

// TestUnrunnableActivitiesDoNotFakeSuccess is the load-bearing assertion for
// the whole set: each of these six used to fall to the dispatch default and be
// reported Succeeded. Removing the map lookup in that default puts them back
// there, and this fails. The check is not "the job failed" but "the STUB's
// output shape is absent" — a job can fail for many reasons, and only the
// missing `activityType` proves the stub is no longer what answered.
func TestUnrunnableActivitiesDoNotFakeSuccess(t *testing.T) {
	for actType := range unrunnableActivities {
		t.Run(actType, func(t *testing.T) {
			a, st := newAPI(t)
			agent := newFakeAgent(t, a)
			ws := seedWorkspace(t, st)

			pl := createPipeline(t, st, ws.ID, typedPipeline(actType))
			_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
			if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
				t.Fatalf("job = %s, want Failed — %s must not be reported as run", s, actType)
			}
			_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
			if out := outputOf(runs, "Step"); out["activityType"] != nil {
				t.Fatalf("%s fell through to the stubbed-success default: %+v", actType, out)
			}
			if got := agent.statements(); len(got) != 0 {
				t.Fatalf("%s ran %d statement(s) on the way to refusing: %q", actType, len(got), got)
			}
		})
	}
}

// TestUnrunnableActivitiesNameTheirCause: six refusals, six different reasons.
// Each assertion targets the substance of that activity's own problem, not a
// phrase shared with its neighbours — a message rewrite that swapped two
// causes would still satisfy a test that only looked for "not supported".
func TestUnrunnableActivitiesNameTheirCause(t *testing.T) {
	for _, tc := range []struct {
		actType string
		want    []string
	}{
		{"HDInsightHive", []string{"HiveQL", "Spark SQL", "not the same as the guarantee"}},
		{"HDInsightPig", []string{"Pig Latin", "nothing in the emulator interprets"}},
		// The cause moved when the JAR task started running: executing a main
		// class is no longer the obstacle, so asserting the old words would
		// pin a limitation the emulator has since removed. What must survive
		// is the RUNTIME argument.
		{"HDInsightMapReduce", []string{"mapper/reducer contract", "Hadoop", "different execution model"}},
		{"HDInsightStreaming", []string{"mapper and reducer", "no Hadoop Streaming harness"}},
		{"DataLakeAnalyticsU-SQL", []string{"U-SQL", "RETIRED THE SERVICE"}},
		{"ExecuteSSISPackage", []string{"integration runtime", "defined inside the package"}},
	} {
		t.Run(tc.actType, func(t *testing.T) {
			a, st := newAPI(t)
			ws := seedWorkspace(t, st)
			pl := createPipeline(t, st, ws.ID, typedPipeline(tc.actType))
			_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
			if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
				t.Fatalf("job = %s, want Failed", s)
			}
			_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
			e, _ := runs[0]["error"].(string)
			for _, want := range tc.want {
				if !strings.Contains(e, want) {
					t.Errorf("refusal does not carry %q:\n%s", want, e)
				}
			}
			if !strings.Contains(e, tc.actType) || !strings.Contains(e, `"Step"`) {
				t.Errorf("refusal names neither the type nor the activity:\n%s", e)
			}
			if !strings.Contains(e, "onInactiveMarkAs") {
				t.Errorf("refusal does not name the modelled way to skip the step:\n%s", e)
			}
		})
	}
}

// TestAnUndocumentedActivityTypeIsRefusedRatherThanStubbed replaces a test
// that asserted the opposite, and the reason it was wrong is worth keeping.
//
// It ran an activity of type "ServiceNowLeaf" and required Completed, on the
// grounds that a connector leaf really is reached in dependsOn order. But
// "ServiceNowLeaf" is not a type any authoring surface emits — nor is
// ServiceNow, in either oracle. A connector is the `type` of a Copy source or
// sink, and copyActivity refuses the ones it cannot run by name. The test was
// reaching the stub through an input invented to reach it, and what the stub
// actually answered for in practice was typos.
func TestAnUndocumentedActivityTypeIsRefusedRatherThanStubbed(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	pl := createPipeline(t, st, ws.ID, typedPipeline("ServiceNowLeaf"))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed — an undocumented type must not be stubbed", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if out := outputOf(runs, "Step"); out["activityType"] != nil {
		t.Fatalf("the stub answered after all: %+v", out)
	}
}

// A one-character typo of a real type is the case that makes this worth doing:
// it reported Succeeded having run no notebook, and the run record was
// indistinguishable from one that had.
func TestATypoIsRefusedAndTheRefusalNamesWhatWasMeant(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	pl := createPipeline(t, st, ws.ID, typedPipeline("TridentNotebok"))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed — a typo must not run as a notebook", s)
	}
	e := &pipelineExecutor{a: &API{}, wid: "ws"}
	_, err := e.Execute(pipeline.Activity{Name: "Step", Type: "TridentNotebok",
		TypeProperties: json.RawMessage(`{}`)}, func(json.RawMessage) (any, error) { return nil, nil })
	if err == nil {
		t.Fatal("no refusal")
	}
	if !strings.Contains(err.Error(), "TridentNotebook") {
		t.Errorf("refusal does not suggest the name that was meant:\n%s", err)
	}
}

// An activity with NO type at all was also reported Succeeded. It names no
// work, so there is nothing that could have run.
func TestATypelessActivityIsRefused(t *testing.T) {
	e := &pipelineExecutor{a: &API{}, wid: "ws"}
	_, err := e.Execute(pipeline.Activity{Name: "Step", Type: "",
		TypeProperties: json.RawMessage(`{}`)}, func(json.RawMessage) (any, error) { return nil, nil })
	if err == nil {
		t.Fatal("a typeless activity was not refused")
	}
	if !strings.Contains(err.Error(), "no type") {
		t.Errorf("refusal does not say what is wrong:\n%s", err)
	}
}

// The hint must not fire on a name that merely shares a few letters — a
// suggestion that is always wrong trains the reader to ignore it.
func TestTheNearMatchHintStaysQuietWhenNothingIsClose(t *testing.T) {
	for _, typ := range []string{"Frobnicate", "ServiceNow", "Sql"} {
		if near := nearestActivityTypes(typ); len(near) != 0 {
			t.Errorf("%s matched %v", typ, near)
		}
	}
	if near := nearestActivityTypes("Copy"); len(near) == 0 {
		t.Error("an exact name should still be reported as its own near match")
	}
}

// The commonest real mistake is not a typo: it is writing the bare product
// name. `Notebook` is eight edits from TridentNotebook and means it plainly,
// so edit distance alone would answer nothing where the answer is obvious.
func TestTheHintCatchesABareProductName(t *testing.T) {
	near := nearestActivityTypes("Notebook")
	if len(near) == 0 {
		t.Fatal("Notebook suggested nothing")
	}
	var found bool
	for _, n := range near {
		if n == "TridentNotebook" {
			found = true
		}
	}
	if !found {
		t.Errorf("Notebook did not suggest TridentNotebook: %v", near)
	}
	if len(near) > 4 {
		t.Errorf("a suggestion list too long to scan is not a suggestion: %v", near)
	}
}

// The list behind the hint may be incomplete, never wrong: every name in it
// must actually be handled. Without this the hint could point a reader at a
// type that refuses too.
func TestEveryAcceptedActivityTypeIsReallyHandled(t *testing.T) {
	e := &pipelineExecutor{a: &API{}, wid: "ws"}
	resolve := func(raw json.RawMessage) (any, error) {
		var v any
		_ = json.Unmarshal(raw, &v)
		return v, nil
	}
	for _, typ := range acceptedActivityTypes {
		if handledByInterpreter[typ] {
			continue
		}
		act := pipeline.Activity{Name: "probe", Type: typ, TypeProperties: json.RawMessage(`{}`)}
		_, err := func() (out map[string]any, err error) {
			defer func() {
				if r := recover(); r != nil {
					out, err = nil, nil
				}
			}()
			return e.Execute(act, resolve)
		}()
		if err != nil && strings.Contains(err.Error(), "is not in Fabric's") {
			t.Errorf("%s is offered as a near-match suggestion but is itself unhandled", typ)
		}
	}
}

// TestUnrunnableRefusalsCoverTheDiff pins the SET, not just its members. The
// list came from diffing ADF's discriminators against the dispatch;
//
// NOTE, now that both halves of that diff are DERIVED rather than typed:
// scripts/check_adf_activity_types.py and scripts/check_fabric_activity_types.py
// walk the vendored schemas, so a name missing from the dispatch already fails a
// check without this map. What this map still adds is the reverse direction with
// a REASON attached — it forces a deliberate edit, and its comments record why
// each entry is here and when two of them left. Worth revisiting whether that is
// enough to keep it. if someone
// later implements one of these for real they must remove it from the map, and
// if someone adds a type string here it must be a real discriminator. The
// names are asserted literally because a typo would silently un-refuse an
// activity — it would fall to the stub again with no test failing.
func TestUnrunnableRefusalsCoverTheDiff(t *testing.T) {
	want := map[string]bool{
		// From ADF's discriminators, the original diff.
		"HDInsightHive": true, "HDInsightPig": true, "HDInsightMapReduce": true,
		"HDInsightStreaming": true, "DataLakeAnalyticsU-SQL": true, "ExecuteSSISPackage": true,

		// From FABRIC's DataPipelineActivityTypes table — a second diff against
		// a different document, because the first one used ADF's schema and
		// Fabric renames several types. Each of these was reaching the stub and
		// being reported Succeeded having run nothing.
		"DataLakeAnalyticsScope": true, // Fabric's name for DataLakeAnalyticsU-SQL

		// Notification activities: the effect is delivery off-machine, so there
		// is no local approximation of "the message arrived".
		"Teams": true, "MicrosoftTeams": true, "Office365Email": true, "Email": true,

		"PBISemanticModelRefresh": true,

		// Synapse's spark-job activity. Found by the ADF half of the same
		// method — scripts/check_adf_activity_types.py, which walks the
		// VENDORED ADF schema rather than a list anyone typed. It was reaching
		// the stub: the only ADF discriminator still unhandled when that
		// checker was written.
		"SparkJob": true,

		// SparkJobDefinition and InvokeCopyJob WERE here, marked temporary
		// because the emulator already ran both item types and only the
		// activity wiring was missing. That wiring landed, so they are gone
		// from the map — which is exactly what this test was written to force.
	}
	for name := range want {
		if _, ok := unrunnableActivities[name]; !ok {
			t.Errorf("%s is no longer refused by name — if it now runs, say so here", name)
		}
	}
	for name := range unrunnableActivities {
		if !want[name] {
			t.Errorf("%s was added to the refusal list without being added to this test", name)
		}
	}
}
