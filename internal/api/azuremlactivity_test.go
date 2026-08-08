package api

import (
	"strings"
	"testing"
)

func azureMLPipeline(actType, extra string) string {
	return `{"properties":{"activities":[
      {"name":"Score","type":"` + actType + `"` + extra + `,"typeProperties":{
        "mlPipelineId":"3f6c8f0e-0000-0000-0000-000000000001",
        "experimentName":"nightly-scoring",
        "mlPipelineParameters":{"threshold":"0.8"}}}]}}`
}

// TestAzureMLDoesNotFakeSuccess is the load-bearing one, and it is first
// because the defect it pins is the reason this activity exists at all: these
// three type strings used to fall through the dispatch switch to its default,
// which returns {"status":"Succeeded"}. A pipeline that scored a model was
// reported as having scored it. Deleting the dispatch case restores exactly
// that, so this test fails — which is what makes the case load-bearing rather
// than decorative.
func TestAzureMLDoesNotFakeSuccess(t *testing.T) {
	for _, actType := range []string{
		"AzureMLExecutePipeline", "AzureMLBatchExecution", "AzureMLUpdateResource",
	} {
		t.Run(actType, func(t *testing.T) {
			a, st := newAPI(t)
			agent := newFakeAgent(t, a)
			ws := seedWorkspace(t, st)

			pl := createPipeline(t, st, ws.ID, azureMLPipeline(actType, ""))
			_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
			if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
				t.Fatalf("job = %s, want Failed — %s must not be reported as run", s, actType)
			}
			_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
			if got := runs[0]["status"]; got != "Failed" {
				t.Fatalf("activity status = %v, want Failed", got)
			}
			// The stub's own signature. Asserting its absence, not just that
			// the status is Failed, is what separates "this activity refuses"
			// from "something else happened to fail".
			out := outputOf(runs, "Score")
			if out["activityType"] != nil {
				t.Fatalf("the activity fell through to the stubbed-success default: %+v", out)
			}
			// Nothing was executed on the way to refusing.
			if got := agent.statements(); len(got) != 0 {
				t.Fatalf("the agent ran %d statement(s) for a refused activity: %q", len(got), got)
			}
		})
	}
}

// TestAzureMLRefusalsNameTheirCause: each of the three says the specific thing
// the emulator does not have, so a reader learns why rather than that. The
// assertions target the substance of each cause — the workspace the steps live
// in, the retired service — not a phrase that merely co-occurs with it.
func TestAzureMLRefusalsNameTheirCause(t *testing.T) {
	for _, tc := range []struct {
		actType string
		want    []string
	}{
		{"AzureMLExecutePipeline", []string{
			"PUBLISHED IN AN AZURE MACHINE LEARNING WORKSPACE",
			"mlPipelineId",
			"invent the published",
		}},
		{"AzureMLBatchExecution", []string{
			"Batch Execution Service",
			"retired ML Studio (classic)",
			"models no linked services",
		}},
		{"AzureMLUpdateResource", []string{
			".ilearner",
			"retired ML Studio (classic)",
			"MLModel items on the real MLflow",
		}},
	} {
		t.Run(tc.actType, func(t *testing.T) {
			a, st := newAPI(t)
			ws := seedWorkspace(t, st)
			pl := createPipeline(t, st, ws.ID, azureMLPipeline(tc.actType, ""))
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
			// The type is named, so a definition with several ML steps says
			// which one stopped, and the activity's own name is there too.
			if !strings.Contains(e, tc.actType) || !strings.Contains(e, `"Score"`) {
				t.Errorf("refusal names neither the type nor the activity:\n%s", e)
			}
			// Why it fails instead of stubbing — the reason a reader needs
			// when the previous release quietly returned Succeeded.
			if !strings.Contains(e, "downstream activities go on to read") {
				t.Errorf("refusal does not say why a fabricated success is worse:\n%s", e)
			}
		})
	}
}

// TestAzureMLRefusalOffersARemedyThatWorks: the refusal tells the caller to
// skip the step with state "Inactive" + onInactiveMarkAs. Advice in an error
// message is a claim like any other, so this runs the advice: the same
// definition, marked inactive, completes — and the activity is recorded
// Inactive rather than quietly executed. An error that recommended a remedy
// the emulator did not honour would be its own small fabrication.
func TestAzureMLRefusalOffersARemedyThatWorks(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)

	pl := createPipeline(t, st, ws.ID, azureMLPipeline("AzureMLExecutePipeline",
		`,"state":"Inactive","onInactiveMarkAs":"Succeeded"`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
		t.Fatalf("job = %s, want Completed with the step marked inactive; runs=%+v", s, runs)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if got := runs[0]["status"]; got != "Inactive" {
		t.Fatalf("activity status = %v, want Inactive — the run report must show it was skipped", got)
	}
}

// TestAzureMLRefusalNamesTheRemedy: the message actually carries that advice.
// Separate from the test above so a rewrite that drops the pointer fails here,
// and a regression in the Inactive gate fails there.
func TestAzureMLRefusalNamesTheRemedy(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	pl := createPipeline(t, st, ws.ID, azureMLPipeline("AzureMLExecutePipeline", ""))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	e, _ := runs[0]["error"].(string)
	if !strings.Contains(e, "onInactiveMarkAs") {
		t.Fatalf("refusal does not name the modelled way to skip the step:\n%s", e)
	}
	if !strings.Contains(e, "Notebook or Spark Job Definition") {
		t.Fatalf("refusal does not name what the emulator can run instead:\n%s", e)
	}
}

// TestAzureMLStopsDependents: a refusal must behave like any other failure in
// the graph — the successor that depends on the scored output does not run.
// Reaching it would mean the pipeline carried on past a step whose result it
// needed, which is the outcome the stubbed success used to produce.
func TestAzureMLStopsDependents(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)

	def := `{"properties":{"activities":[
      {"name":"Score","type":"AzureMLExecutePipeline","typeProperties":{"mlPipelineId":"p1"}},
      {"name":"Publish","type":"SetVariable","typeProperties":{"variableName":"v","value":"x"},
       "dependsOn":[{"activity":"Score","dependencyConditions":["Succeeded"]}]}]}}`
	pl := createPipeline(t, st, ws.ID, def)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	for _, r := range runs {
		if r["activityName"] == "Publish" && r["status"] == "Succeeded" {
			t.Fatalf("the dependent step ran on a refused ML activity: %+v", runs)
		}
	}
}
