package api

import (
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// TestNoJobTypeReportsSuccessForAnItemItCouldNotRead.
//
// THE CLASS, not the instance. Every item type whose job needs a definition
// must refuse to report success when there isn't one — because "Completed" is
// the single signal every caller keys on, and a Completed job that executed
// nothing is indistinguishable from one that did the work.
//
// DataPipeline and SparkJobDefinition always got this right. Notebook did not:
// a missing definition returned no error code, so the clock completed the job,
// no engine was ever asked, and nothing was posted to notebookRunResult. It
// surfaced as a user watching two green RunNotebook jobs in the portal and
// finding no trace of either at the result endpoint.
//
// One type drifting is a bug. Nothing noticing is the reason it could. This is
// the check that makes the next item type fail here rather than in a portal.
func TestNoJobTypeReportsSuccessForAnItemItCouldNotRead(t *testing.T) {
	for _, tc := range []struct{ itemType, jobType string }{
		{"Notebook", "RunNotebook"},
		{"SparkJobDefinition", "sparkjob"},
		{"DataPipeline", "Pipeline"},
	} {
		t.Run(tc.itemType, func(t *testing.T) {
			a, st := newAPI(t)
			ws := seedWorkspace(t, st)
			it := &store.Item{WorkspaceID: ws.ID, Type: tc.itemType, DisplayName: "no-definition"}
			if err := st.CreateItem(it, nil); err != nil {
				t.Fatal(err)
			}
			_, jid := runJob(t, a, ws.ID, it.ID, "jobType="+tc.jobType, "")
			if s := awaitJob(t, a, ws.ID, it.ID, jid); s == "Completed" {
				t.Fatalf("%s/%s with no definition reported Completed — a caller "+
					"cannot tell that from a run that did the work",
					tc.itemType, tc.jobType)
			}
		})
	}
}
