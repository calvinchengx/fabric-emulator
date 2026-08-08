package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
)

// THE GAP THIS CLOSES. docs/parity.md carried four Identity & security rows at
// 🟡 for one shared reason: "No tenant-admin gate: the emulator has no
// Fabric-administrator role model, so any authenticated principal may call
// these." Every /v1/admin/* surface was faithful in shape and unguarded in
// authorisation — a viewer could list the whole tenant and create governance
// domains.
//
// The assertions that matter here are the REFUSALS. A gate is only worth
// having if it says no to somebody, and a test that only proves admins are
// allowed would pass against the ungated code it replaced.

var (
	tenantAdminUser = &auth.Principal{ID: "fabric-admin-1", Type: "User"}
	plainUser       = &auth.Principal{ID: "viewer-9", Type: "User"}
	plainSP         = &auth.Principal{ID: "sp-7", Type: "ServicePrincipal"}
	adminByAppID    = &auth.Principal{ID: "oid-abc", Type: "ServicePrincipal", App: "admin-app-1"}
)

func adminAPI(t *testing.T) *API {
	t.Helper()
	a, _ := newAPI(t)
	a.SetTenantAdmins([]string{"fabric-admin-1", "admin-app-1"})
	return a
}

// --- reads: Fabric administrator OR any service principal --------------------

func TestAdminReadAllowsAdministratorAndServicePrincipal(t *testing.T) {
	a := adminAPI(t)
	for _, p := range []*auth.Principal{tenantAdminUser, plainSP, adminByAppID} {
		if !a.requireTenantRead(httptest.NewRecorder(), p) {
			t.Fatalf("read refused for %s/%s; Microsoft's reference allows "+
				"a Fabric administrator OR a service principal", p.Type, p.ID)
		}
	}
}

func TestAdminReadRefusesAnOrdinaryUser(t *testing.T) {
	a := adminAPI(t)
	w := httptest.NewRecorder()
	if a.requireTenantRead(w, plainUser) {
		t.Fatal("an ordinary user was allowed to read the whole tenant")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("refusal status = %d, want 403", w.Code)
	}
	if got := errorCode(t, w); got != "InsufficientPrivileges" {
		t.Fatalf("errorCode = %q, want InsufficientPrivileges (the documented code)", got)
	}
}

// --- writes: Fabric administrator ONLY ---------------------------------------
//
// This is the asymmetry the gate exists for. Real Fabric's create-domain
// reference says "The caller must be a Fabric administrator" with no
// service-principal escape, while list-workspaces explicitly allows one. A
// single combined check would grant every service principal the ability to
// create governance domains and override capacity tenant settings.

func TestAdminWriteRefusesAServicePrincipalThatIsNotAnAdministrator(t *testing.T) {
	a := adminAPI(t)
	if a.requireTenantAdmin(httptest.NewRecorder(), plainSP) {
		t.Fatal("a non-admin service principal was allowed to MUTATE the tenant; " +
			"create-domain requires a Fabric administrator, no SP escape")
	}
	// ...and the same principal may still READ, or the gate has overcorrected.
	if !a.requireTenantRead(httptest.NewRecorder(), plainSP) {
		t.Fatal("a service principal was refused a tenant READ, which real Fabric allows")
	}
}

func TestAdminWriteAllowsOnlyDeclaredAdministrators(t *testing.T) {
	a := adminAPI(t)
	for _, p := range []*auth.Principal{tenantAdminUser, adminByAppID} {
		if !a.requireTenantAdmin(httptest.NewRecorder(), p) {
			t.Fatalf("declared administrator %s/%s refused a write", p.Type, p.ID)
		}
	}
	for _, p := range []*auth.Principal{plainUser, plainSP, nil} {
		if a.requireTenantAdmin(httptest.NewRecorder(), p) {
			t.Fatalf("undeclared principal %+v was allowed to mutate the tenant", p)
		}
	}
}

// An unconfigured emulator must refuse mutation rather than allow everyone.
// The pre-gate behaviour was "everybody is an admin", so defaulting to open
// would reintroduce exactly what this change removes.
func TestWithNoAdministratorsDeclaredEveryWriteIsRefused(t *testing.T) {
	a, _ := newAPI(t)
	a.SetTenantAdmins(nil) // undo the harness default; this test IS the default case
	for _, p := range []*auth.Principal{tenantAdminUser, plainSP, plainUser} {
		if a.requireTenantAdmin(httptest.NewRecorder(), p) {
			t.Fatalf("%s/%s mutated the tenant with no admins configured", p.Type, p.ID)
		}
	}
}

// Membership is configuration, never a property of the token's shape.
func TestAdminMembershipIsNotInferredFromPrincipalType(t *testing.T) {
	a := adminAPI(t)
	// Same TYPE as a declared admin, different id — must not inherit it.
	if a.isTenantAdmin(&auth.Principal{ID: "someone-else", Type: "User"}) {
		t.Fatal("an undeclared User was treated as a Fabric administrator")
	}
	if a.isTenantAdmin(&auth.Principal{ID: "other", Type: "ServicePrincipal", App: "other-app"}) {
		t.Fatal("an undeclared service principal was treated as a Fabric administrator")
	}
}

// THE ROUTE-LEVEL HALF. Everything above proves the gate FUNCTIONS refuse; none
// of it proves any route calls them. That is the same map-vs-route gap that let
// Spark Job Definitions sit 🟡 while its unit tests passed — the mechanism was
// right and nothing wired it up. These go through the registered mux with a
// real validator, so a route registered with plain withAuth fails here.
func TestEveryAdminRouteIsGatedOnTheRegisteredMux(t *testing.T) {
	mux, _, token := newRegisteredAPI(t)

	// The harness's token is principal "route-admin", which is NOT declared an
	// administrator anywhere — so every mutating admin route must refuse it.
	writes := []struct{ method, target string }{
		{"POST", "/v1/admin/domains?preview=false"},
		{"PATCH", "/v1/admin/domains/11111111-1111-1111-1111-111111111111"},
		{"DELETE", "/v1/admin/domains/11111111-1111-1111-1111-111111111111"},
		{"POST", "/v1/admin/domains/11111111-1111-1111-1111-111111111111/assignWorkspaces"},
		{"POST", "/v1/admin/items/bulkSetLabels"},
		{"POST", "/v1/admin/items/bulkRemoveLabels"},
		{"POST", "/v1/admin/capacities/22222222-2222-2222-2222-222222222222/delegatedTenantSettingOverrides/x/update"},
	}
	for _, c := range writes {
		w := serve(mux, c.method, c.target, token, `{"displayName":"x"}`)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s = %d for a non-administrator, want 403 — is the route "+
				"registered with withAuth instead of withTenantAdmin?", c.method, c.target, w.Code)
			continue
		}
		if got := errorCode(t, w); got != "InsufficientPrivileges" {
			t.Errorf("%s %s errorCode = %q, want InsufficientPrivileges", c.method, c.target, got)
		}
	}

	// Reads by the same principal are refused too: it is a User (no appid in
	// the minted token), so neither branch of the read rule admits it.
	for _, target := range []string{"/v1/admin/workspaces", "/v1/admin/items", "/v1/admin/domains", "/v1/admin/labels"} {
		if w := serve(mux, "GET", target, token, ""); w.Code != http.StatusForbidden {
			t.Errorf("GET %s = %d for a non-administrator user, want 403", target, w.Code)
		}
	}
}
