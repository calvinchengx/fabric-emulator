package api

// Fabric admin APIs: domains (fabric-docs governance/domains.md, and the
// `/v1/admin/domains` reference the admin-portal docs link to). Domains are a
// tenant-level grouping of workspaces, one subdomain level deep, with two
// domain-scoped roles beneath the tenant admin.
//
// Tenant-admin gate: the emulator has no tenant-admin role model — every
// authenticated principal is treated as a Fabric admin here. Real Fabric
// requires the Fabric administrator role (or Tenant.ReadWrite.All). This is a
// deliberate, documented simplification; the *domain*-scoped roles below are
// modelled for real because they are part of the API contract.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func (a *API) registerAdminDomains(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/admin/domains", a.withAuth(a.listDomains))
	mux.HandleFunc("POST /v1/admin/domains", a.withAuth(a.createDomain))
	mux.HandleFunc("GET /v1/admin/domains/{did}", a.withAuth(a.getDomain))
	mux.HandleFunc("PATCH /v1/admin/domains/{did}", a.withAuth(a.updateDomain))
	mux.HandleFunc("DELETE /v1/admin/domains/{did}", a.withAuth(a.deleteDomain))
	mux.HandleFunc("GET /v1/admin/domains/{did}/workspaces", a.withAuth(a.listDomainWorkspaces))
	mux.HandleFunc("POST /v1/admin/domains/{did}/assignWorkspaces", a.withAuth(a.assignDomainWorkspaces))
	mux.HandleFunc("POST /v1/admin/domains/{did}/unassignWorkspaces", a.withAuth(a.unassignDomainWorkspaces))
	mux.HandleFunc("POST /v1/admin/domains/{did}/unassignAllWorkspaces", a.withAuth(a.unassignAllDomainWorkspaces))
	mux.HandleFunc("GET /v1/admin/domains/{did}/roleAssignments", a.withAuth(a.listDomainRoles))
	mux.HandleFunc("POST /v1/admin/domains/{did}/roleAssignments/bulkAssign", a.withAuth(a.bulkAssignDomainRole))
	mux.HandleFunc("POST /v1/admin/domains/{did}/roleAssignments/bulkUnassign", a.withAuth(a.bulkUnassignDomainRole))
}

// domainProps builds the operationProperties the domain audit schema
// documents for every domain operation (governance/domains-audit-schema.md):
// DataDomainObjectId, DataDomainDisplayName, and ParentObjectId when the
// domain is a subdomain.
func domainProps(d *store.Domain) map[string]any {
	props := map[string]any{
		"DataDomainObjectId":    d.ID,
		"DataDomainDisplayName": d.DisplayName,
	}
	if d.ParentDomainID != "" {
		props["ParentObjectId"] = d.ParentDomainID
	}
	return props
}

// auditDomainWorkspaces records an assignment change. The schema names the
// counters FoldersToSetCounter / FoldersToUnsetCount — "folders" is the audit
// vocabulary's word for workspaces.
func (a *API) auditDomainWorkspaces(p *auth.Principal, domainID, counter string, n int) {
	d, err := a.Store.GetDomain(domainID)
	if err != nil {
		return
	}
	props := domainProps(d)
	props[counter] = n
	a.audit(p, &store.ActivityEvent{Operation: store.OpUpdateDomainWorkspaces, Properties: props})
}

// domainErr maps a store error onto the admin surface's error envelope.
func domainErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "DomainNotFound", "The domain was not found.")
	case errors.Is(err, store.ErrSubdomainDepth):
		writeErr(w, http.StatusBadRequest, "InvalidRequest",
			"A subdomain cannot have subdomains: the hierarchy is two levels.")
	case errors.Is(err, store.ErrNameConflict):
		writeErr(w, http.StatusConflict, "DomainDisplayNameInUse",
			"A domain with that displayName already exists.")
	default:
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
	}
}

func (a *API) listDomains(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	ds, err := a.Store.ListDomains(r.URL.Query().Get("nonEmptyOnly") == "true")
	if err != nil {
		domainErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"domains": ds})
}

func (a *API) createDomain(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var body store.Domain
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.DisplayName == "" {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "displayName is required.")
		return
	}
	d := &store.Domain{
		DisplayName:       body.DisplayName,
		Description:       body.Description,
		ParentDomainID:    body.ParentDomainID,
		ContributorsScope: body.ContributorsScope,
	}
	if err := a.Store.CreateDomain(d); err != nil {
		domainErr(w, err)
		return
	}
	a.audit(p, &store.ActivityEvent{Operation: store.OpInsertDomain,
		Properties: domainProps(d)})
	writeJSON(w, http.StatusCreated, d)
}

func (a *API) getDomain(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	d, err := a.Store.GetDomain(r.PathValue("did"))
	if err != nil {
		domainErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (a *API) updateDomain(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var patch store.Domain
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "Malformed JSON body.")
		return
	}
	d, err := a.Store.UpdateDomain(r.PathValue("did"), &patch)
	if err != nil {
		domainErr(w, err)
		return
	}
	a.audit(p, &store.ActivityEvent{Operation: store.OpUpdateDomain,
		Properties: domainProps(d)})
	writeJSON(w, http.StatusOK, d)
}

func (a *API) deleteDomain(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	// Read it first: the audit record names the domain, and after the delete
	// there is nothing left to name it from.
	deleted, _ := a.Store.GetDomain(r.PathValue("did"))
	if err := a.Store.DeleteDomain(r.PathValue("did")); err != nil {
		domainErr(w, err)
		return
	}
	if deleted != nil {
		a.audit(p, &store.ActivityEvent{Operation: store.OpDeleteDomain,
			Properties: domainProps(deleted)})
	}
	w.WriteHeader(http.StatusOK)
}

func (a *API) listDomainWorkspaces(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	ws, err := a.Store.DomainWorkspaces(r.PathValue("did"))
	if err != nil {
		domainErr(w, err)
		return
	}
	writePage(w, r, ws)
}

// workspaceIDsBody is the shape both assign and unassign take.
type workspaceIDsBody struct {
	WorkspacesIDs []string `json:"workspacesIds"`
}

func (a *API) assignDomainWorkspaces(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var body workspaceIDsBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.WorkspacesIDs) == 0 {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "workspacesIds is required.")
		return
	}
	if err := a.Store.AssignWorkspaces(r.PathValue("did"), body.WorkspacesIDs); err != nil {
		domainErr(w, err)
		return
	}
	a.auditDomainWorkspaces(p, r.PathValue("did"), "FoldersToSetCounter", len(body.WorkspacesIDs))
	w.WriteHeader(http.StatusOK)
}

func (a *API) unassignDomainWorkspaces(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var body workspaceIDsBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.WorkspacesIDs) == 0 {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "workspacesIds is required.")
		return
	}
	if err := a.Store.UnassignWorkspaces(r.PathValue("did"), body.WorkspacesIDs); err != nil {
		domainErr(w, err)
		return
	}
	a.auditDomainWorkspaces(p, r.PathValue("did"), "FoldersToUnsetCount", len(body.WorkspacesIDs))
	w.WriteHeader(http.StatusOK)
}

func (a *API) unassignAllDomainWorkspaces(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	if err := a.Store.UnassignAllWorkspaces(r.PathValue("did")); err != nil {
		domainErr(w, err)
		return
	}
	if d, err := a.Store.GetDomain(r.PathValue("did")); err == nil {
		a.audit(p, &store.ActivityEvent{Operation: store.OpDeleteAllDomainWorkspae,
			Properties: domainProps(d)})
	}
	w.WriteHeader(http.StatusOK)
}

func (a *API) listDomainRoles(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	ras, err := a.Store.DomainRoleAssignments(r.PathValue("did"))
	if err != nil {
		domainErr(w, err)
		return
	}
	writePage(w, r, ras)
}

// domainRoleBody is the bulk role-assignment shape: a role plus the
// principals it applies to.
type domainRoleBody struct {
	Type       string            `json:"type"`
	Principals []store.Principal `json:"principals"`
}

// parseDomainRole validates the bulk body and normalises the role name.
func parseDomainRole(w http.ResponseWriter, r *http.Request) (*domainRoleBody, bool) {
	var body domainRoleBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Principals) == 0 {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "principals is required.")
		return nil, false
	}
	switch body.Type {
	case store.DomainRoleAdmin, store.DomainRoleContributor:
	default:
		writeErr(w, http.StatusBadRequest, "InvalidRequest",
			`type must be "Admins" or "Contributors".`)
		return nil, false
	}
	for _, pr := range body.Principals {
		if pr.ID == "" {
			writeErr(w, http.StatusBadRequest, "InvalidRequest", "Each principal needs an id.")
			return nil, false
		}
	}
	return &body, true
}

func (a *API) bulkAssignDomainRole(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	body, ok := parseDomainRole(w, r)
	if !ok {
		return
	}
	if err := a.Store.AssignDomainRole(r.PathValue("did"), body.Type, body.Principals); err != nil {
		domainErr(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (a *API) bulkUnassignDomainRole(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	body, ok := parseDomainRole(w, r)
	if !ok {
		return
	}
	if err := a.Store.UnassignDomainRole(r.PathValue("did"), body.Type, body.Principals); err != nil {
		domainErr(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}
