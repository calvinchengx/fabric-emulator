package api

import (
	"strings"
	"testing"
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
		{"HDInsightMapReduce", []string{"NAMED JAVA MAIN CLASS", "both engines"}},
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

// TestTheStubStillAnswersForConnectorLeaves is the other side of the same
// change, and it is why the refusal list is a map consulted in the default
// rather than a wider net. A ServiceNow leaf really is reached in dependsOn
// order with its inputs resolved, and the emulator says so; turning that into
// a failure would break every pipeline whose shape is being exercised against
// connectors nobody expects to run here.
func TestTheStubStillAnswersForConnectorLeaves(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	pl := createPipeline(t, st, ws.ID, typedPipeline("ServiceNowLeaf"))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("job = %s, want Completed — a connector leaf is still stubbed", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if out := outputOf(runs, "Step"); out["activityType"] != "ServiceNowLeaf" {
		t.Fatalf("the leaf did not go through the stub: %+v", out)
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
