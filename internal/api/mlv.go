package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// Materialized lake views — the definition surface, and the refresh that makes
// one real.
//
// # What Fabric does
//
// A materialized lake view is a query held against a lakehouse whose results
// are kept as a Delta table, re-computed on refresh, with a lineage view over
// the graph of views. Fabric creates them with **Spark SQL DDL run inside a
// notebook**.
//
// # What the emulator does, and where the line is
//
// The refresh is real: the query runs on the engine the emulator already
// hosts, and the rows land in OneLake as a Delta table under `Tables/`, so a
// Delta reader, the SQL endpoint, the flow stream and lineage all see it as
// they see any other write.
//
// The DEFINITION surface is emulator-native, and deliberately so. No capture of
// Fabric's DDL exists here, and this repo's oracle rule is explicit that a
// syntax nobody has observed does not get invented — a `CREATE MATERIALIZED
// LAKE VIEW` parser written from memory would accept spellings Fabric rejects
// and reject ones it accepts, while looking authoritative. The precedent for
// doing it this way is the Reflex trigger binding (internal/api/triggers.go),
// which has no public REST either: the binding is emulator-native, everything
// downstream of it is faithful, and docs/parity.md says which is which.
//
// When a capture arrives, the DDL becomes a second front door onto this same
// model rather than a rewrite of it.
//
// # Staleness
//
// `dependsOn` is DECLARED by the caller. Fabric infers it from the SQL; doing
// that here means parsing a dialect, and a wrong parse does not fail loudly —
// it reports a stale view as fresh, which is the failure family this repo
// exists to avoid. The versions of those tables at the last successful refresh
// are recorded, and `isStale` compares them with what the tables are now. A
// view that has never refreshed is not "stale", it is `NeverRefreshed`: no
// table exists, and calling that a staleness problem would imply there is
// something there to be out of date.

func (a *API) registerMLV(mux *http.ServeMux) {
	base := "/v1/workspaces/{wid}/lakehouses/{iid}/materializedlakeviews"
	mux.HandleFunc("POST "+base, a.withAuth(a.createMLV))
	mux.HandleFunc("GET "+base, a.withAuth(a.listMLV))
	mux.HandleFunc("GET "+base+"/{name}", a.withAuth(a.getMLV))
	mux.HandleFunc("DELETE "+base+"/{name}", a.withAuth(a.deleteMLV))
	mux.HandleFunc("POST "+base+"/{name}/refresh", a.withAuth(a.refreshMLV))
}

type mlvRequest struct {
	Name      string   `json:"name"`
	Query     string   `json:"query"`
	DependsOn []string `json:"dependsOn"`
}

// mlvBody renders a view, including the two derived facts a caller actually
// wants: whether it has ever been materialised, and whether a source has moved
// since it was.
func (a *API) mlvBody(v *store.MaterializedLakeView) map[string]any {
	out := map[string]any{
		"id": v.ID, "name": v.Name, "query": v.Query,
		"lakehouseId": v.LakehouseID, "workspaceId": v.WorkspaceID,
		"dependsOn": v.DependsOn, "createdAt": v.CreatedAt,
		"tablePath": "Tables/" + v.Name,
	}
	if v.LastRefreshStatus == "" {
		out["state"] = "NeverRefreshed"
		out["isStale"] = false
		return out
	}
	out["lastRefreshStatus"] = v.LastRefreshStatus
	if v.LastRefreshedAt > 0 {
		out["lastRefreshedAt"] = v.LastRefreshedAt
	}
	if v.LastError != "" {
		out["lastError"] = v.LastError
	}
	if v.LastRefreshStatus != "Succeeded" {
		out["state"] = "RefreshFailed"
		out["isStale"] = true
		return out
	}
	stale, moved := a.mlvStale(v)
	out["state"] = "Materialized"
	out["isStale"] = stale
	if len(moved) > 0 {
		// Naming WHICH source moved is the difference between a flag and an
		// explanation; a caller with six dependencies otherwise has to diff
		// versions by hand to learn what to re-run.
		out["staleBecause"] = moved
	}
	out["sourceVersions"] = v.SourceVersions
	return out
}

// mlvStale compares each declared dependency's current Delta version with the
// version read at the last successful refresh.
func (a *API) mlvStale(v *store.MaterializedLakeView) (bool, []string) {
	moved := []string{}
	for _, dep := range v.DependsOn {
		now, ok := a.Store.DeltaTableVersion(v.LakehouseID, dep)
		was, seen := v.SourceVersions[dep]
		switch {
		case !ok:
			// The source is gone, or was never a Delta table. Either way the
			// materialised rows no longer correspond to anything readable.
			moved = append(moved, dep)
		case !seen || now != was:
			moved = append(moved, dep)
		}
	}
	return len(moved) > 0, moved
}

func (a *API) lakehouseFor(w http.ResponseWriter, r *http.Request, p *auth.Principal, need string) (*store.Item, bool) {
	wid, iid := r.PathValue("wid"), r.PathValue("iid")
	if _, _, ok := a.requireRole(w, wid, p, need); !ok {
		return nil, false
	}
	it, err := a.Store.GetItem(wid, iid)
	if err != nil || it.Type != "Lakehouse" {
		writeErr(w, http.StatusNotFound, "ItemNotFound", "The lakehouse was not found.")
		return nil, false
	}
	return it, true
}

func (a *API) createMLV(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	lh, ok := a.lakehouseFor(w, r, p, store.RoleContributor)
	if !ok {
		return
	}
	var req mlvRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "The body is not JSON.")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || strings.ContainsAny(req.Name, "/\\ ") {
		writeErr(w, http.StatusBadRequest, "InvalidRequest",
			"name is required and becomes a table under Tables/, so it cannot contain a path separator or a space.")
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "query is required.")
		return
	}
	v := &store.MaterializedLakeView{
		WorkspaceID: lh.WorkspaceID, LakehouseID: lh.ID,
		Name: req.Name, Query: req.Query, DependsOn: req.DependsOn,
	}
	if err := a.Store.CreateMaterializedLakeView(v); err != nil {
		if err == store.ErrDuplicateMLV {
			writeErr(w, http.StatusConflict, "ItemDisplayNameAlreadyInUse", err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, a.mlvBody(v))
}

func (a *API) listMLV(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	lh, ok := a.lakehouseFor(w, r, p, store.RoleViewer)
	if !ok {
		return
	}
	views, err := a.Store.ListMaterializedLakeViews(lh.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	out := make([]map[string]any, 0, len(views))
	for _, v := range views {
		out = append(out, a.mlvBody(v))
	}
	writeJSON(w, http.StatusOK, map[string]any{"value": out})
}

func (a *API) getMLV(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	lh, ok := a.lakehouseFor(w, r, p, store.RoleViewer)
	if !ok {
		return
	}
	v, err := a.Store.GetMaterializedLakeView(lh.ID, r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "ItemNotFound", "The materialized lake view was not found.")
		return
	}
	writeJSON(w, http.StatusOK, a.mlvBody(v))
}

func (a *API) deleteMLV(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	lh, ok := a.lakehouseFor(w, r, p, store.RoleContributor)
	if !ok {
		return
	}
	if err := a.Store.DeleteMaterializedLakeView(lh.ID, r.PathValue("name")); err != nil {
		writeErr(w, http.StatusNotFound, "ItemNotFound", "The materialized lake view was not found.")
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (a *API) refreshMLV(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	lh, ok := a.lakehouseFor(w, r, p, store.RoleContributor)
	if !ok {
		return
	}
	v, err := a.Store.GetMaterializedLakeView(lh.ID, r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "ItemNotFound", "The materialized lake view was not found.")
		return
	}
	if err := a.RefreshMaterializedLakeView(v); err != nil {
		writeErr(w, http.StatusBadGateway, "MaterializedLakeViewRefreshFailed", err.Error())
		return
	}
	fresh, err := a.Store.GetMaterializedLakeView(lh.ID, v.Name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.mlvBody(fresh))
}

// RefreshMaterializedLakeView re-computes the view on the engine and writes the
// result as a Delta table under `Tables/<name>`.
//
// The versions are read BEFORE the query runs. Reading them afterwards would
// record a version that may already include a write that landed during the
// refresh — the view would claim to reflect data it never read, and the first
// staleness check after that would say "fresh" about rows that are not in it.
// Recorded before, the worst case is a view reported stale slightly too eagerly,
// which costs a refresh rather than a wrong answer.
func (a *API) RefreshMaterializedLakeView(v *store.MaterializedLakeView) error {
	fail := func(format string, args ...any) error {
		err := fmt.Errorf(format, args...)
		_ = a.Store.RecordMaterializedLakeViewRefresh(v.LakehouseID, v.Name, "Failed", err.Error(), nil)
		return err
	}
	if !a.runsNotebooksItself() {
		return fail("materialized lake view %q: no Spark agent is configured, so the query "+
			"was not run and no table was written — start the stack with a Spark engine", v.Name)
	}

	versions := map[string]int{}
	for _, dep := range v.DependsOn {
		if n, ok := a.Store.DeltaTableVersion(v.LakehouseID, dep); ok {
			versions[dep] = n
		}
	}

	session := "mlv-" + v.ID
	defer func() { _, _ = a.agentPost("/close", map[string]any{"session": session}) }()
	a.registerLakehouseTables(session, v.WorkspaceID, v.LakehouseID)

	// The same OneLake address the catalog registers its tables at, so the view
	// lands where a reader already looks for one.
	target := fmt.Sprintf("abfs://%s@onelake.dfs.fabric.microsoft.com/%s/Tables/%s",
		v.WorkspaceID, v.LakehouseID, v.Name)
	qj, _ := json.Marshal(v.Query)
	tj, _ := json.Marshal(target)
	// `qj`/`tj` are JSON string LITERALS, and a JSON string literal is already a
	// valid Python one — so they are interpolated directly. They used to be
	// wrapped in `json.loads(...)`, which is one decode too many: Python parses
	// the literal first, so json.loads received bare SQL and raised
	//   JSONDecodeError: Expecting value: line 1 column 1 (char 0)
	// on every refresh. The Go tests never saw it because their agent records
	// statements without executing them; e2e/sail runs the real agent, and the
	// first real execution of this path is what found it.
	code := fmt.Sprintf(`import json
__df = spark.sql(%s)
__n = __df.count()
__df.write.format("delta").mode("overwrite").save(%s)
print(json.dumps({"rowCount": __n}))
`, string(qj), string(tj))

	out, err := a.agentPost("/statements", map[string]any{
		"session": session, "code": code, "kind": "python",
	})
	if err != nil {
		return fail("materialized lake view %q: the Spark agent is unreachable: %v", v.Name, err)
	}
	if status, _ := out["status"].(string); status != "ok" {
		return fail("materialized lake view %q: %s: %s", v.Name,
			fmt.Sprint(out["ename"]), fmt.Sprint(out["evalue"]))
	}

	// A refresh that wrote no table is a failed refresh, however cheerfully the
	// statement returned. Checking OneLake rather than trusting the engine's
	// exit is the difference between "the code ran" and "the view exists".
	if _, ok := a.Store.DeltaTableVersion(v.LakehouseID, v.Name); !ok {
		return fail("materialized lake view %q: the statement succeeded but no Delta table "+
			"was committed at Tables/%s — the view was not materialised, which is not the "+
			"same as a refresh that produced no rows", v.Name, v.Name)
	}
	return a.Store.RecordMaterializedLakeViewRefresh(v.LakehouseID, v.Name, "Succeeded", "", versions)
}
