package api

import (
	"net/http"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
)

// The tenant-admin gate for `/v1/admin/*`.
//
// Until this existed the emulator had no Fabric-administrator role model at
// all, so ANY authenticated principal could list every workspace in the
// tenant, read every item, create governance domains and override capacity
// tenant settings. The management surfaces were faithful; the authorisation
// was absent, and docs/parity.md said so on four rows.
//
// THE RULE IS GRADED, and flattening it would be wrong in the permissive
// direction. Microsoft's admin reference states two different requirements:
//
//	read  (e.g. admin/workspaces/list-workspaces, admin/items/list-items):
//	      "The caller must be a Fabric administrator OR authenticate using a
//	      service principal." Scopes: Tenant.Read.All or Tenant.ReadWrite.All.
//
//	write (e.g. admin/domains/create-domain):
//	      "The caller must be a Fabric administrator." — no service-principal
//	      escape. Scope: Tenant.ReadWrite.All.
//
// So a service principal may READ the tenant and may not CHANGE it, while a
// Fabric administrator may do both. A single "is admin" check would refuse
// service-principal reads that real Fabric allows; a single "is admin or SP"
// check would allow service-principal writes that real Fabric refuses. Both
// are divergences, and the second is the dangerous one.
//
// The refusal is `InsufficientPrivileges` with 403, matching both the
// documented error code and what requireRole already returns for the
// per-workspace equivalent — one vocabulary for "you may not", whichever
// boundary said no.

// isTenantAdmin reports whether the principal is a Fabric administrator.
//
// Membership is configuration, not inference: a principal is an administrator
// because the operator said so (`-tenant-admins`, FABRIC_TENANT_ADMINS), never
// because of how its token looks. Deriving it from a claim would make the
// emulator's answer depend on the issuer's shape rather than on a decision
// anyone made.
func (a *API) isTenantAdmin(p *auth.Principal) bool {
	if p == nil {
		return false
	}
	for _, id := range a.tenantAdmins {
		if id == "" {
			continue
		}
		if strings.EqualFold(id, p.ID) || (p.App != "" && strings.EqualFold(id, p.App)) {
			return true
		}
	}
	return false
}

// requireTenantRead gates a read-only admin API: Fabric administrator OR any
// service principal.
func (a *API) requireTenantRead(w http.ResponseWriter, p *auth.Principal) bool {
	if a.isTenantAdmin(p) || (p != nil && p.Type == "ServicePrincipal") {
		return true
	}
	writeErr(w, http.StatusForbidden, "InsufficientPrivileges",
		"The caller must be a Fabric administrator or authenticate using a service principal.")
	return false
}

// requireTenantAdmin gates a mutating admin API: Fabric administrator only.
//
// A service principal is deliberately NOT enough here. That asymmetry is the
// whole reason this function is separate from requireTenantRead, and merging
// them would silently grant every service principal the ability to create
// domains and override tenant settings.
func (a *API) requireTenantAdmin(w http.ResponseWriter, p *auth.Principal) bool {
	if a.isTenantAdmin(p) {
		return true
	}
	writeErr(w, http.StatusForbidden, "InsufficientPrivileges",
		"The caller must be a Fabric administrator.")
	return false
}

// withTenantRead / withTenantAdmin wrap a handler with authentication AND the
// tenant gate, so a route cannot be registered with one and not the other.
// Applied at registration by HTTP method: GET reads, everything else mutates.
func (a *API) withTenantRead(h handler) http.HandlerFunc {
	return a.withAuth(func(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
		if !a.requireTenantRead(w, p) {
			return
		}
		h(w, r, p)
	})
}

func (a *API) withTenantAdmin(h handler) http.HandlerFunc {
	return a.withAuth(func(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
		if !a.requireTenantAdmin(w, p) {
			return
		}
		h(w, r, p)
	})
}
