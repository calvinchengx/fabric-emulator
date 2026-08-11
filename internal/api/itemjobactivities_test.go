package api

import (
	"fmt"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// These two activities used to be REFUSED by name (#180), with a cause saying
// the capability existed and only the wiring was missing. The assertions below
// are what "wired" has to mean: not "no longer refused", but the work actually
// happened — bytes at the destination, a real job the run can be traced to.
//
// A test that only asserted the job reached Completed would pass against the
// fabricated success these replaced, which is the whole reason that class of
// bug survived.

func itemJobPipeline(actType, idKey, itemID, wsID string) string {
	return fmt.Sprintf(`{"properties":{"activities":[
      {"name":"Step","type":%q,"typeProperties":{%q:%q,"workspaceId":%q}}]}}`,
		actType, idKey, itemID, wsID)
}

// InvokeCopyJob must MOVE THE BYTES, not merely finish. Same assertion the
// direct Copy job run makes, reached through a pipeline activity instead.
func TestInvokeCopyJobActivityReallyCopies(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := seedLakehouse(t, st, ws.ID, "src")
	dst := seedLakehouse(t, st, ws.ID, "dst")
	payload := []byte("orders bytes")
	seedFile(t, st, ws.ID, src.ID, "Tables/orders/part-0.parquet", payload)

	cj := createCopyJob(t, st, ws.ID, lakehouseBatchDef(ws.ID, src.ID, dst.ID, ""))
	pl := createPipeline(t, st, ws.ID, itemJobPipeline("InvokeCopyJob", "copyJobId", cj.ID, ws.ID))

	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("pipeline job = %s, want Completed", s)
	}

	got, err := st.GetOneLakePath(dst.ID, "Tables/bronze_orders/part-0.parquet")
	if err != nil {
		t.Fatalf("destination table missing — the activity did not copy anything: %v", err)
	}
	if string(got.Content) != string(payload) {
		t.Fatalf("destination content = %q, want %q", got.Content, payload)
	}

	// And it went through a real Copy job run, not the stub: the activity's
	// output names the job it created, and the stub's shape is absent.
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	out := outputOf(runs, "Step")
	if out["activityType"] != nil {
		t.Fatalf("the stub answered, not the activity: %+v", out)
	}
	if out["copyJobId"] != cj.ID || out["jobId"] == nil || out["jobId"] == "" {
		t.Fatalf("activity output does not name the copy job it ran: %+v", out)
	}
}

// The zero GUID is Fabric's own "this pipeline's workspace" sentinel, so it has
// to resolve to the running workspace rather than be looked up literally.
func TestInvokeCopyJobAcceptsTheZeroWorkspaceSentinel(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := seedLakehouse(t, st, ws.ID, "src")
	dst := seedLakehouse(t, st, ws.ID, "dst")
	seedFile(t, st, ws.ID, src.ID, "Tables/orders/part-0.parquet", []byte("x"))

	cj := createCopyJob(t, st, ws.ID, lakehouseBatchDef(ws.ID, src.ID, dst.ID, ""))
	pl := createPipeline(t, st, ws.ID,
		itemJobPipeline("InvokeCopyJob", "copyJobId", cj.ID, "00000000-0000-0000-0000-000000000000"))

	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("pipeline job = %s — the zero GUID should mean this workspace", s)
	}
}

// A reference to the wrong item type fails by name rather than running
// something else or reporting success.
func TestItemJobActivitiesRefuseTheWrongItemType(t *testing.T) {
	for _, tc := range []struct{ actType, idKey string }{
		{"InvokeCopyJob", "copyJobId"},
		{"SparkJobDefinition", "sparkJobDefinitionId"},
	} {
		t.Run(tc.actType, func(t *testing.T) {
			a, st := newAPI(t)
			ws := seedWorkspace(t, st)
			lake := seedLakehouse(t, st, ws.ID, "not-the-right-type")
			pl := createPipeline(t, st, ws.ID, itemJobPipeline(tc.actType, tc.idKey, lake.ID, ws.ID))
			_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
			if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
				t.Fatalf("job = %s, want Failed — a Lakehouse is not a %s", s, tc.actType)
			}
		})
	}
}

// A missing required id fails with the documented property named, rather than
// resolving to nothing and proceeding.
func TestItemJobActivitiesRequireTheirDocumentedId(t *testing.T) {
	for _, tc := range []struct{ actType, idKey string }{
		{"InvokeCopyJob", "copyJobId"},
		{"SparkJobDefinition", "sparkJobDefinitionId"},
	} {
		t.Run(tc.actType, func(t *testing.T) {
			a, st := newAPI(t)
			ws := seedWorkspace(t, st)
			pl := createPipeline(t, st, ws.ID, typedPipeline(tc.actType))
			_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
			if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
				t.Fatalf("job = %s, want Failed — %s is required", s, tc.idKey)
			}
			_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
			if out := outputOf(runs, "Step"); out["activityType"] != nil {
				t.Fatalf("the stub answered instead of the activity: %+v", out)
			}
		})
	}
}

// With NO Spark engine attached the activity must not report success: the job
// is submitted and left Pending for an external engine, and the activity says
// so. Reporting Succeeded here is exactly the fabrication this replaced.
func TestSparkJobDefinitionActivityWithoutAnEngineDoesNotClaimSuccess(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	// A VALID definition on purpose. An SJD with no parts fails at the parse,
	// which makes the pipeline fail for a reason that has nothing to do with
	// the engine — the test would then pass while proving nothing about the
	// no-engine path. Caught by mutation: claiming Completed here still passed
	// the earlier version of this test.
	sjd := seedRunnableSparkJobDefinition(t, st, ws.ID)
	pl := createPipeline(t, st, ws.ID,
		itemJobPipeline("SparkJobDefinition", "sparkJobDefinitionId", sjd.ID, ws.ID))

	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed — with no engine the Spark job is Pending, "+
			"and reporting it as run is the fabrication this replaced", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if out := outputOf(runs, "Step"); out["activityType"] != nil {
		t.Fatalf("the stub answered instead of the activity: %+v", out)
	}
	// The job really was submitted and is unfinished — the activity did not
	// simply refuse before creating anything, which would be a different (and
	// less useful) behaviour than "submitted, awaiting an engine".
	jobs, err := st.ListJobInstances(50)
	if err != nil {
		t.Fatal(err)
	}
	submitted := 0
	for _, j := range jobs {
		if j.ItemID == sjd.ID && j.JobType == "sparkjob" {
			submitted++
		}
	}
	if submitted != 1 {
		t.Fatalf("expected exactly one submitted Spark job for the SJD, got %d", submitted)
	}
}

// seedRunnableSparkJobDefinition is an SJD whose definition PARSES, so a test
// exercises the engine path rather than the parse failure.
func seedRunnableSparkJobDefinition(t *testing.T, st *store.Store, wid string) *store.Item {
	t.Helper()
	lake := seedLakehouse(t, st, wid, fmt.Sprintf("lake-%d", pipelineSeq.Add(1)))
	it := &store.Item{WorkspaceID: wid, Type: "SparkJobDefinition",
		DisplayName: fmt.Sprintf("sjd-%d", pipelineSeq.Add(1))}
	config := `{"executableFile":"main.py","defaultLakehouseArtifactId":"` +
		lake.ID + `","defaultLakehouseWorkspaceId":"` + wid + `"}`
	if err := st.CreateItem(it, []store.DefinitionPart{
		sparkPart("SparkJobDefinitionV1.json", config),
		sparkPart("main.py", "print('done')"),
	}); err != nil {
		t.Fatal(err)
	}
	return it
}

// THE POSITIVE CASE for Spark Job Definition: with an agent attached the
// emulator is the pool, so the activity must actually run the job and gate the
// pipeline on its real outcome — not merely stop refusing.
func TestSparkJobDefinitionActivityRunsOnTheAgent(t *testing.T) {
	a, st := newAPI(t)
	newFakeAgent(t, a)
	ws := seedWorkspace(t, st)
	lake := seedLakehouse(t, st, ws.ID, "lake")

	sjd := &store.Item{WorkspaceID: ws.ID, Type: "SparkJobDefinition", DisplayName: "job"}
	config := `{"executableFile":"main.py","defaultLakehouseArtifactId":"` +
		lake.ID + `","defaultLakehouseWorkspaceId":"` + ws.ID + `"}`
	if err := st.CreateItem(sjd, []store.DefinitionPart{
		sparkPart("SparkJobDefinitionV1.json", config),
		sparkPart("main.py", "print('done')"),
	}); err != nil {
		t.Fatal(err)
	}

	pl := createPipeline(t, st, ws.ID,
		itemJobPipeline("SparkJobDefinition", "sparkJobDefinitionId", sjd.ID, ws.ID))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("pipeline job = %s, want Completed — the agent should have run the Spark job", s)
	}

	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	out := outputOf(runs, "Step")
	if out["activityType"] != nil {
		t.Fatalf("the stub answered, not the activity: %+v", out)
	}
	if out["sparkJobDefinitionId"] != sjd.ID || out["status"] != "Completed" {
		t.Fatalf("activity output does not report a completed Spark job: %+v", out)
	}
	// A real job instance exists behind it, which is what makes the run
	// traceable rather than asserted.
	jobID, _ := out["jobId"].(string)
	if jobID == "" {
		t.Fatal("activity output carries no jobId")
	}
	if _, err := st.GetJobInstance(sjd.ID, jobID); err != nil {
		t.Fatalf("the activity reported a job that does not exist: %v", err)
	}
}
