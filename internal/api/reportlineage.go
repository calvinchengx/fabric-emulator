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

// reportLineageBody is the wire shape: one step and what it moved.
//
// A step may report SEVERAL movements, and usually should. The obvious shape —
// one list of reads, one of writes — pairs them as a cross product, and that
// overstates: a silver step reading `bronze_customers` and `bronze_orders`
// while writing `silver_customers`, `silver_orders` and `silver_quarantine`
// did NOT derive the quarantine from the customers. Six edges, three of them
// describing movements that never happened.
//
// So `moves` is the precise form, and top-level `reads`/`writes` remain as the
// shorthand for the case where the cross product is genuinely what happened —
// a survivorship join really does read everything to write its output.
type reportLineageBody struct {
	// Step names the unit of work — "silver", "star_silver", "semantic_model".
	// It becomes the edge's activity name, which is also part of the store's
	// uniqueness key, so two steps moving the same pair stay two edges.
	Step   string        `json:"step"`
	Reads  []lineageRef2 `json:"reads"`
	Writes []lineageRef2 `json:"writes"`
	Moves  []lineageMove `json:"moves"`
}

// lineageMove is one derivation: these inputs produced these outputs.
type lineageMove struct {
	Reads  []lineageRef2 `json:"reads"`
	Writes []lineageRef2 `json:"writes"`
}

// lineageRef2 addresses one dataset. workspaceId defaults to the workspace in
// the route.
type lineageRef2 struct {
	WorkspaceID string `json:"workspaceId"`
	ItemID      string `json:"itemId"`
	Path        string `json:"path"`
	// ConnectionID names a SOURCE SYSTEM instead of a Fabric item: the vendor
	// API, database or change stream the bytes came from. Valid on a read only
	// — a write lands somewhere in OneLake by definition — and mutually
	// exclusive with itemId/path, since the source is the system itself and
	// not a path inside it.
	//
	// A connection rather than a URI because the connection is already there:
	// the pipeline created it to hold the credential, it carries a display
	// name, and it is what the client authenticated through. The emulator
	// resolves it to confirm it exists rather than trusting the caller's word
	// for the one part it can check.
	ConnectionID string `json:"connectionId"`
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
	moves := body.Moves
	if len(body.Reads) > 0 || len(body.Writes) > 0 {
		moves = append(moves, lineageMove{Reads: body.Reads, Writes: body.Writes})
	}
	if len(moves) == 0 {
		writeErr(w, http.StatusBadRequest, "InvalidRequest",
			"reads and writes, or moves, are required: lineage describes movement.")
		return
	}

	recorded := 0
	for _, mv := range moves {
		// A movement with only one end did not move anything. Saying so is
		// clearer than silently recording zero edges and returning success.
		if len(mv.Reads) == 0 || len(mv.Writes) == 0 {
			writeErr(w, http.StatusBadRequest, "InvalidRequest",
				"every movement needs both reads and writes: a step with only one end did not move anything.")
			return
		}
		for _, in := range mv.Reads {
			for _, out := range mv.Writes {
				if in.ConnectionID != "" && (in.ItemID != "" || in.Path != "") {
					writeErr(w, http.StatusBadRequest, "InvalidRequest",
						"a read names either a connectionId or an itemId+path, not both: a source system is not a path inside Fabric.")
					return
				}
				if in.ConnectionID == "" && (in.ItemID == "" || in.Path == "") {
					writeErr(w, http.StatusBadRequest, "InvalidRequest",
						"every read needs an itemId and a path, or a connectionId; an incomplete reference is not an exact fact.")
					return
				}
				if out.ItemID == "" || out.Path == "" {
					writeErr(w, http.StatusBadRequest, "InvalidRequest",
						"every write needs an itemId and a path; an incomplete reference is not an exact fact.")
					return
				}
				// Resolve rather than trust. A connection id that names nothing
				// would draw a source node for a system this emulator has never
				// heard of — a graph that looks more complete than the truth,
				// which is the one failure this design refuses everywhere else.
				if in.ConnectionID != "" {
					if _, err := a.Store.GetConnection(in.ConnectionID); err != nil {
						writeErr(w, http.StatusBadRequest, "InvalidRequest",
							"connectionId "+in.ConnectionID+" does not name a connection.")
						return
					}
				}
				srcWS, dstWS := in.WorkspaceID, out.WorkspaceID
				if srcWS == "" {
					srcWS = wid
				}
				if dstWS == "" {
					dstWS = wid
				}
				// A connection source carries no workspace and no path: the
				// system is the node. Putting the connection id in SourceItemID
				// is what keeps the table's UNIQUE key discriminating, so two
				// vendors landing in one table stay two edges.
				edge := &store.LineageEdge{
					WorkspaceID:       wid,
					ActivityName:      body.Step,
					SourceWorkspaceID: srcWS, SourceItemID: in.ItemID, SourcePath: in.Path,
					TargetWorkspaceID: dstWS, TargetItemID: out.ItemID, TargetPath: out.Path,
					Producer: store.ProducerReported, SourceKind: store.SourceKindItem,
				}
				if in.ConnectionID != "" {
					edge.SourceKind = store.SourceKindConnection
					edge.SourceWorkspaceID, edge.SourceItemID, edge.SourcePath = "", in.ConnectionID, ""
				}
				if err := a.Store.CreateLineageEdge(edge); err != nil {
					writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
					return
				}
				recorded++
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"step": body.Step, "edgesRecorded": recorded})
}
