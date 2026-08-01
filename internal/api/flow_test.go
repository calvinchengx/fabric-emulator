package api

// The flow stream's control-plane events: a job's start and outcome, each
// activity's settled result, and the attribution that says which unit of work
// moved which bytes. See docs/31-flow-observability.md.

import (
	"strings"
	"testing"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// drainEvents collects everything published within a short settling window.
func drainEvents(t *testing.T, ch <-chan store.Event) []store.Event {
	t.Helper()
	var out []store.Event
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
		case <-time.After(150 * time.Millisecond):
			return out
		case <-deadline:
			return out
		}
	}
}

func TestJobAndActivityEventsForASucceedingPipeline(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	pipe := createPipeline(t, st, ws.ID, `{"properties":{"activities":[
		{"name":"First","type":"Wait","typeProperties":{"waitTimeInSeconds":1}},
		{"name":"Second","type":"SetVariable","typeProperties":{"variableName":"v","value":"x"},
		 "dependsOn":[{"activity":"First","dependencyConditions":["Succeeded"]}]}],
		"variables":{"v":{"type":"String"}}}}`)
	events, cancel := st.Subscribe()
	defer cancel()

	j, err := a.startJob(ws.ID, pipe, "Pipeline", store.InvokeManual, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := drainEvents(t, events)

	// The job brackets the run: started first, terminal last.
	if len(got) < 4 {
		t.Fatalf("only %d events: %+v", len(got), got)
	}
	if got[0].Kind != store.KindJob || got[0].Status != store.JobStarted || got[0].JobID != j.ID {
		t.Fatalf("first event = %+v, want the job starting", got[0])
	}
	last := got[len(got)-1]
	if last.Kind != store.KindJob || last.Status != store.JobCompleted {
		t.Fatalf("last event = %+v, want the job completing", last)
	}

	// Both activities are reported, in execution order, with their real detail.
	var acts []store.Event
	for _, ev := range got {
		if ev.Kind == store.KindActivity {
			acts = append(acts, ev)
		}
	}
	if len(acts) != 2 || acts[0].ActivityName != "First" || acts[1].ActivityName != "Second" {
		t.Fatalf("activity events = %+v", acts)
	}
	if acts[0].ActivityType != "Wait" || acts[0].Status != "Succeeded" || acts[0].JobID != j.ID {
		t.Fatalf("first activity = %+v", acts[0])
	}
	if acts[0].Duration != 1 {
		t.Fatalf("durationInSeconds = %v, want the Wait's 1s", acts[0].Duration)
	}
	if acts[0].ItemID != pipe.ID || acts[0].WorkspaceID != ws.ID {
		t.Fatalf("activity scoping = %+v", acts[0])
	}
}

func TestActivityFailureIsOnTheStreamWithItsError(t *testing.T) {
	// The reason step 2 exists: a failure is visible as it happens, with the
	// error attached, instead of being reconstructed from queryactivityruns.
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	pipe := createPipeline(t, st, ws.ID, `{"properties":{"activities":[
		{"name":"Boom","type":"Fail","typeProperties":{"message":"synthetic failure","errorCode":"E42"}}]}}`)
	events, cancel := st.Subscribe()
	defer cancel()

	if _, err := a.startJob(ws.ID, pipe, "Pipeline", store.InvokeManual, nil); err != nil {
		t.Fatal(err)
	}
	got := drainEvents(t, events)

	var failed *store.Event
	for i, ev := range got {
		if ev.Kind == store.KindActivity && ev.Status == "Failed" {
			failed = &got[i]
		}
	}
	if failed == nil {
		t.Fatalf("no failed activity on the stream: %+v", got)
	}
	if failed.ActivityName != "Boom" || !strings.Contains(failed.Error, "synthetic failure") {
		t.Fatalf("failed activity = %+v", *failed)
	}
	// …and the job's own failure follows it.
	last := got[len(got)-1]
	if last.Kind != store.KindJob || last.Status != store.JobFailed || last.FailureReason == "" {
		t.Fatalf("last event = %+v, want the job failing with a reason", last)
	}
}

func TestRetriedAttemptsAreNotAnnouncedTwice(t *testing.T) {
	// The interpreter discards failed attempts and back-patches the survivor,
	// so the stream must report what the result reports — one settled outcome
	// carrying its retry count — not every attempt.
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	pipe := createPipeline(t, st, ws.ID, `{"properties":{"activities":[
		{"name":"Flaky","type":"Fail","typeProperties":{"message":"always fails"},
		 "policy":{"retry":2,"retryIntervalInSeconds":5}}]}}`)
	events, cancel := st.Subscribe()
	defer cancel()

	if _, err := a.startJob(ws.ID, pipe, "Pipeline", store.InvokeManual, nil); err != nil {
		t.Fatal(err)
	}
	var acts []store.Event
	for _, ev := range drainEvents(t, events) {
		if ev.Kind == store.KindActivity {
			acts = append(acts, ev)
		}
	}
	if len(acts) != 1 {
		t.Fatalf("%d activity events for one activity with retries: %+v", len(acts), acts)
	}
	if acts[0].RetryAttempt != 2 {
		t.Fatalf("retryAttempt = %d, want the 2 attempts the policy consumed", acts[0].RetryAttempt)
	}
	// The stream must agree with what the API will report.
	_, runsJSON, err := st.GetPipelineRun(pipelineJobID(t, st, pipe.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(runsJSON, `"retryAttempt":2`) {
		t.Fatalf("queryactivityruns disagrees with the stream: %s", runsJSON)
	}
}

// pipelineJobID returns the item's only job instance id.
func pipelineJobID(t *testing.T, st *store.Store, itemID string) string {
	t.Helper()
	jobs, err := st.ListItemJobInstances(itemID)
	if err != nil || len(jobs) == 0 {
		t.Fatalf("no jobs for %s: %v", itemID, err)
	}
	return jobs[0].ID
}

func TestSkippedActivitiesReachTheStream(t *testing.T) {
	// Skips are recorded outside the retry path; they must not be the events
	// that quietly go missing.
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	pipe := createPipeline(t, st, ws.ID, `{"properties":{"activities":[
		{"name":"Boom","type":"Fail","typeProperties":{"message":"nope"}},
		{"name":"Never","type":"Wait","typeProperties":{"waitTimeInSeconds":1},
		 "dependsOn":[{"activity":"Boom","dependencyConditions":["Succeeded"]}]}]}}`)
	events, cancel := st.Subscribe()
	defer cancel()

	if _, err := a.startJob(ws.ID, pipe, "Pipeline", store.InvokeManual, nil); err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, ev := range drainEvents(t, events) {
		if ev.Kind == store.KindActivity {
			seen[ev.ActivityName] = ev.Status
		}
	}
	if seen["Boom"] != "Failed" || seen["Never"] != "Skipped" {
		t.Fatalf("activity statuses = %v", seen)
	}
}

func TestGenericItemJobAnnouncesOnlyItsStart(t *testing.T) {
	// A generic item's status is clock-derived: there is no moment it becomes
	// terminal, so the stream claims nothing rather than inventing one.
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	it := &store.Item{WorkspaceID: ws.ID, DisplayName: "report", Type: "Report"}
	if err := st.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	events, cancel := st.Subscribe()
	defer cancel()

	if _, err := a.startJob(ws.ID, it, "Refresh", store.InvokeManual, nil); err != nil {
		t.Fatal(err)
	}
	got := drainEvents(t, events)
	if len(got) != 1 || got[0].Kind != store.KindJob || got[0].Status != store.JobStarted {
		t.Fatalf("events = %+v, want only the start", got)
	}
}

// --- attribution: which unit of work moved these bytes (docs/31, step 3) ---

// copyPipeline builds a pipeline whose Copy activity reads a CSV from the
// lakehouse's Files and writes it as a Delta table.
func copyPipeline(t *testing.T, st *store.Store, wsID, lakeID, activity string) *store.Item {
	t.Helper()
	ref := `{"linkedService":{"properties":{"type":"Lakehouse","typeProperties":{"workspaceId":"` +
		wsID + `","artifactId":"` + lakeID + `"}}}}`
	return createPipeline(t, st, wsID, `{"properties":{"activities":[
		{"name":"`+activity+`","type":"Copy","typeProperties":{
			"source":{"type":"DelimitedTextSource","rootFolder":"Files","folderPath":"landing",
				"fileName":"customers.csv","datasetSettings":`+ref+`},
			"sink":{"type":"LakehouseTableSink","tableActionOption":"Overwrite",
				"table":"bronze_customers","datasetSettings":`+ref+`}}}]}}`)
}

func TestCopyActivityAttributesTheBytesItMoved(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := &store.Item{WorkspaceID: ws.ID, DisplayName: "lake", Type: "Lakehouse"}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateOneLakePath(&store.OneLakePath{WorkspaceID: ws.ID, ItemID: lake.ID,
		RelPath: "Files/landing/customers.csv", Content: []byte("id,name\n1,ada\n2,grace\n")}, false); err != nil {
		t.Fatal(err)
	}
	pipe := copyPipeline(t, st, ws.ID, lake.ID, "IngestCustomers")

	events, cancel := st.Subscribe()
	defer cancel()
	j, err := a.startJob(ws.ID, pipe, "Pipeline", store.InvokeManual, nil)
	if err != nil {
		t.Fatal(err)
	}

	var table *store.Event
	var attributedFiles int
	for _, ev := range drainEvents(t, events) {
		if ev.Attribution == nil {
			continue
		}
		if ev.Attribution.JobID != j.ID || ev.Attribution.ActivityName != "IngestCustomers" {
			t.Fatalf("wrong attribution on %+v", ev)
		}
		if ev.Attribution.CellIndex != nil {
			t.Fatalf("a Copy is not a notebook cell: %+v", ev.Attribution)
		}
		switch ev.Kind {
		case store.KindFile:
			attributedFiles++
		case store.KindTable:
			table = &ev
		}
	}
	// Both writes the Delta path makes — the Parquet part and the commit —
	// name the activity, and so does the table event derived from the commit.
	if attributedFiles < 2 {
		t.Fatalf("%d attributed file events, want the data file and the commit", attributedFiles)
	}
	if table == nil {
		t.Fatal("the derived table event lost the attribution its commit carried")
	}
	if table.Table != "Tables/bronze_customers" || table.RowsAdded != 2 {
		t.Fatalf("table event = %+v", *table)
	}
}

func TestWritesWithNothingToSayCarryNoAttribution(t *testing.T) {
	// Attribution is never inferred. A plain store write — a seed, a git
	// checkout, a deployment — says nothing rather than guessing.
	_, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := &store.Item{WorkspaceID: ws.ID, DisplayName: "lake", Type: "Lakehouse"}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}
	events, cancel := st.Subscribe()
	defer cancel()
	if err := st.CreateOneLakePath(&store.OneLakePath{WorkspaceID: ws.ID, ItemID: lake.ID,
		RelPath: "Files/plain.csv", Content: []byte("x")}, false); err != nil {
		t.Fatal(err)
	}
	for _, ev := range drainEvents(t, events) {
		if ev.Attribution != nil {
			t.Fatalf("invented attribution: %+v", *ev.Attribution)
		}
	}
}

func TestCellZeroIsDistinguishableFromNoCell(t *testing.T) {
	// The reason CellIndex is a pointer: cell 0 is a real cell, and a plain
	// int could not tell "the first cell" from "not a cell at all".
	zero := store.CellBy("job-1", 0)
	if zero.Empty() || zero.CellIndex == nil || *zero.CellIndex != 0 {
		t.Fatalf("CellBy(_, 0) = %+v", zero)
	}
	act := store.ActivityBy("job-1", "Copy1")
	if act.CellIndex != nil {
		t.Fatalf("an activity attribution invented a cell: %+v", act)
	}
	if !(store.Attribution{}).Empty() {
		t.Fatal("the zero Attribution is not empty")
	}
	if act.Empty() {
		t.Fatal("an activity attribution reports itself empty")
	}
}
