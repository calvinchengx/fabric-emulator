package api

import (
	"net/http"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// listLineage returns exact edges emitted by executed activities. This local
// extension is consumed by governance adapters; Fabric has no equivalent
// public endpoint that exposes all workspace lineage in one call.
func (a *API) listLineage(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleViewer); !ok {
		return
	}
	edges, err := a.Store.ListLineageEdges(wid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writePage(a, w, r, edges)
}
