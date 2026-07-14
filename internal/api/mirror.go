package api

import (
	"net/http"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// refreshMirror snapshots a Fabric SQL Database's SQL tables to OneLake as Delta
// — the emulator's explicit hook for the mirroring real Fabric performs
// continuously in the background. Contributor+ on the workspace; the item must
// be a SQLDatabase; 501 when no warehouse SQL backend is wired.
func (a *API) refreshMirror(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleContributor); !ok {
		return
	}
	it, err := a.Store.GetItem(wid, r.PathValue("iid"))
	if err != nil || it.Type != "SQLDatabase" {
		writeErr(w, http.StatusNotFound, "ItemNotFound", "The SQL database is not available.")
		return
	}
	if a.MirrorItem == nil {
		writeErr(w, http.StatusNotImplemented, "MirroringUnavailable",
			"OneLake mirroring requires a warehouse SQL backend (--warehouse-sql-url).")
		return
	}
	if err := a.MirrorItem(r.Context(), it.ID); err != nil {
		writeErr(w, http.StatusBadGateway, "MirrorFailed", err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}
