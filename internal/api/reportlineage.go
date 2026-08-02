package api

// Reported lineage: an engine says what it moved.
//
// A queued notebook run already reports its read/write set when it finishes
// (notebooks.go). Plenty of real work is not a queued run: an interactive
// Spark session, a local PySpark script, a Python step that exports gold into
// a semantic model. Those move data through OneLake — the emulator sees the
// bytes and the tables — but nothing tells it which read caused which write,
// and it will not infer that from watching.
//
// So this is the same contract as the notebook report, without the job: POST
// what you read and what you wrote, get lineage edges. It is an emulator-native
// extension (real Fabric has no such endpoint) and docs/parity.md records it as
// one, exactly like the workspace-wide GET beside it.
//
// The trust model is unchanged and worth restating: the CALLER is the authority
// on its own data flow. The emulator records the claim and marks it as reported
// (ProducerReported), so a consumer can always tell a report from something the
// emulator watched happen (ProducerNotebookObserved, ProducerWarehouse).

import (
	"encoding/json"
	"net/http"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// reportLineageBody is the wire shape: one step, its inputs and its outputs.
// It mirrors the notebook result's `reads`/`writes` so an engine that already
// speaks one speaks the other.
type reportLineageBody struct {
	// Step names the unit of work — "silver", "star_silver", "semantic_model".
	// It becomes the edge's activity name, which is also part of the store's
	// uniqueness key, so two steps moving the same pair stay two edges.
	Step   string        `json:"step"`
	Reads  []lineageRef2 `json:"reads"`
	Writes []lineageRef2 `json:"writes"`
}

// lineageRef2 addresses one dataset. workspaceId defaults to the workspace in
// the route.
type lineageRef2 struct {
	WorkspaceID string `json:"workspaceId"`
	ItemID      string `json:"itemId"`
	Path        string `json:"path"`
}

// reportLineage records one step's movements: an edge per (read × write) pair,
// the same rule the notebook report uses — a step joining two tables into one
// records both inputs.
func (a *API) reportLineage(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	// Writing lineage is a claim about the workspace's data, so it takes the
	// same role writing the data would.
	if _, _, ok := a.requireRole(w, wid, p, store.RoleContributor); !ok {
		return
	}
	var body reportLineageBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "a JSON body with reads and writes is required.")
		return
	}
	if body.Step == "" {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "step is required: it names the unit of work that moved the data.")
		return
	}
	// A step that read nothing, or wrote nothing, moved nothing. Saying so is
	// clearer than silently recording zero edges and returning success.
	if len(body.Reads) == 0 || len(body.Writes) == 0 {
		writeErr(w, http.StatusBadRequest, "InvalidRequest",
			"both reads and writes are required: lineage describes movement, and a step with only one end did not move anything.")
		return
	}

	recorded := 0
	for _, in := range body.Reads {
		for _, out := range body.Writes {
			if in.ItemID == "" || in.Path == "" || out.ItemID == "" || out.Path == "" {
				writeErr(w, http.StatusBadRequest, "InvalidRequest",
					"every read and write needs an itemId and a path; an incomplete reference is not an exact fact.")
				return
			}
			srcWS, dstWS := in.WorkspaceID, out.WorkspaceID
			if srcWS == "" {
				srcWS = wid
			}
			if dstWS == "" {
				dstWS = wid
			}
			if err := a.Store.CreateLineageEdge(&store.LineageEdge{
				WorkspaceID:       wid,
				ActivityName:      body.Step,
				SourceWorkspaceID: srcWS, SourceItemID: in.ItemID, SourcePath: in.Path,
				TargetWorkspaceID: dstWS, TargetItemID: out.ItemID, TargetPath: out.Path,
				Producer: store.ProducerReported,
			}); err != nil {
				writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
				return
			}
			recorded++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"step": body.Step, "edgesRecorded": recorded})
}
