package api

// A lakehouse's SQL analytics endpoint, as the ITEM real Fabric makes it.
//
// MEASURED ON A REAL TENANT, 2026-08-11. Creating one lakehouse and one warehouse
// left THREE items in the workspace:
//
//	SQLEndpoint  803c8e33-c35c-4f1b-9b44-f40dce69e75e  lake
//	Warehouse    c6c4ba99-0f1d-4514-b715-2be7c8e900c2  dw
//	Lakehouse    450d5027-f17e-454e-b9ca-dbe424c494b6  lake
//
// The SQLEndpoint carries the lakehouse's display name and its OWN id — and that
// id is exactly what `GET /lakehouses/{id}` reports as
// `properties.sqlEndpointProperties.id`. It is a first-class item, listed like
// any other.
//
// WHY THIS CHANGES AN EARLIER DECISION, deliberately. The emulator used to OMIT
// `sqlEndpointProperties.id`, on the reasoning that it had no such item and
// reporting the lakehouse's own id there would let a consumer use it as a
// database name — green locally, addressing the wrong thing on a tenant. That was
// the right call for the information available. The measurement makes it
// obsolete: the honest fix is not to withhold the field but to HAVE the item, so
// the id is a different GUID here exactly as it is there.
//
// What that unlocks is `refreshMetadata`, which is addressed by the endpoint id:
//
//	POST /v1/workspaces/{ws}/sqlEndpoints/{id}/refreshMetadata  ->  200 {"value":[…]}
//
// Measured against the tenant: a plain 200 with a per-table array (empty for a
// lakehouse with no tables), no LRO and no Location header. That is the lever a
// client uses when the analytics endpoint has not caught up with Delta yet, and
// its absence here is why `dq_gate.py` used to re-sync by opening a throwaway
// connection — true locally, a silent no-op on a tenant.

import (
	"net/http"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/warehouse"
)

// propParentLakehouse links a SQLEndpoint item back to the lakehouse it serves,
// the same way a KQLDatabase names its Eventhouse.
const propParentLakehouse = "parentLakehouseItemId"

// ensureSQLEndpointItem creates the SQLEndpoint item that accompanies a
// lakehouse. Idempotent by search: a second call finds the existing one, so a
// re-created lakehouse of the same name does not accumulate endpoints.
func (a *API) ensureSQLEndpointItem(lakehouse *store.Item) string {
	if id := a.sqlEndpointItemFor(lakehouse); id != "" {
		return id
	}
	ep := &store.Item{
		WorkspaceID: lakehouse.WorkspaceID,
		Type:        "SQLEndpoint",
		// Fabric names it after the lakehouse, which is why two items in one
		// workspace can share a display name — the pair is not a collision.
		DisplayName: lakehouse.DisplayName,
	}
	if err := a.Store.CreateItem(ep, nil); err != nil {
		return ""
	}
	_ = a.Store.SetItemProperties(ep.ID, map[string]string{propParentLakehouse: lakehouse.ID})
	return ep.ID
}

// sqlEndpointItemFor returns the id of the SQLEndpoint serving `lakehouse`, or "".
func (a *API) sqlEndpointItemFor(lakehouse *store.Item) string {
	eps, err := a.Store.ListItems(lakehouse.WorkspaceID, "SQLEndpoint")
	if err != nil {
		return ""
	}
	for _, ep := range eps {
		if props, err := a.Store.ItemProperties(ep.ID); err == nil &&
			props[propParentLakehouse] == lakehouse.ID {
			return ep.ID
		}
	}
	return ""
}

// tableSync is one row of the refreshMetadata report. Field names are the ones
// Microsoft's announcement documents ("detailed synchronization status for each
// table, including start and end times, status, last successful sync time"); the
// ENVELOPE (`{"value": […]}`, plain 200) is measured against a tenant.
type tableSync struct {
	TableName                  string `json:"tableName"`
	Status                     string `json:"status"`
	StartDateTime              string `json:"startDateTime"`
	EndDateTime                string `json:"endDateTime"`
	LastSuccessfulSyncDateTime string `json:"lastSuccessfulSyncDateTime"`
}

// refreshSQLEndpointMetadata re-reads the lakehouse's Delta and rebuilds the SQL
// view of it, reporting per table.
//
// The emulator reflects on connect, so this is not the only way its endpoint
// catches up — but it is the only way a CLIENT can ask, and a client that has to
// open a connection to force a refresh is written against the emulator rather
// than against Fabric.
func (a *API) refreshSQLEndpointMetadata(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleContributor); !ok {
		return
	}
	ep, err := a.Store.GetItem(wid, r.PathValue("epid"))
	if err != nil || ep.Type != "SQLEndpoint" {
		writeErr(w, http.StatusNotFound, "ItemNotFound", "The item is not available.")
		return
	}
	props, _ := a.Store.ItemProperties(ep.ID)
	lakehouseID := props[propParentLakehouse]
	if lakehouseID == "" {
		writeErr(w, http.StatusNotFound, "ItemNotFound",
			"This SQL endpoint is not associated with a lakehouse.")
		return
	}
	if a.LakehouseDB == nil {
		// No SQL engine attached: say so rather than reporting a sync that did
		// not happen. An empty 200 here would be indistinguishable from "the
		// lakehouse has no tables", which is the lie this whole surface exists
		// to avoid.
		writeErr(w, http.StatusNotImplemented, "SQLEndpointNotConfigured",
			"This emulator serves no SQL endpoint: start it with FABRIC_SQL_TDS_ADDR and a warehouse SQL backend.")
		return
	}
	started := time.Now().UTC().Format(time.RFC3339)
	// The reflector needs the lakehouse's own SQL Server database. LakehouseDB
	// prepares one and hands back its pool — a different hook from SQLDB, which
	// refuses a Lakehouse on purpose (see lakehouseDBFor).
	db, err := a.LakehouseDB(r.Context(), lakehouseID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "SQLEndpointRefreshFailed", err.Error())
		return
	}
	if db == nil {
		// A hook that yields no pool and no error is a misconfigured backend, not
		// an empty lakehouse. Saying which costs one branch and saves a reader the
		// wrong hypothesis.
		writeErr(w, http.StatusNotImplemented, "SQLEndpointNotConfigured",
			"The SQL backend produced no connection for this lakehouse.")
		return
	}
	var report []tableSync
	names, rerr := warehouse.Reflect(r.Context(), db, a.Store, lakehouseID)
	done := time.Now().UTC().Format(time.RFC3339)
	for _, n := range names {
		report = append(report, tableSync{
			TableName: n, Status: "Success",
			StartDateTime: started, EndDateTime: done,
			LastSuccessfulSyncDateTime: done,
		})
	}
	if rerr != nil {
		writeErr(w, http.StatusBadGateway, "SQLEndpointRefreshFailed", rerr.Error())
		return
	}
	if report == nil {
		report = []tableSync{} // `{"value": []}`, as the tenant answers
	}
	writeJSON(w, http.StatusOK, map[string]any{"value": report})
}

// oneLakePaths builds a lakehouse's `oneLakeTablesPath` / `oneLakeFilesPath`.
//
// Real Fabric returns absolute `https://onelake.dfs.fabric.microsoft.com/{ws}/{item}/…`
// URLs (measured 2026-08-11). The emulator serves OneLake on its own origin, so
// the host comes from the caller's request for the same reason the SQL address
// does: a container on the compose network and a laptop reach it by different
// names, and one configured hostname would be wrong for one of them.
func (a *API) oneLakePaths(r *http.Request, it *store.Item) map[string]any {
	base := oneLakeBaseURI(r) + "/" + it.WorkspaceID + "/" + it.ID
	return map[string]any{
		"oneLakeTablesPath": base + "/Tables",
		"oneLakeFilesPath":  base + "/Files",
	}
}

// oneLakeBaseURI is the DFS origin the caller reached this emulator on.
func oneLakeBaseURI(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	return scheme + "://" + r.Host + "/onelake"
}
