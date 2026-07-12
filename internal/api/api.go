// Package api serves the Fabric control plane: /v1 workspaces, items, role
// assignments, and long-running operations, with Fabric-shaped errors and
// workspace RBAC enforced from the validated bearer principal.
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"

	"github.com/calvinchengx/fabric-emulator/internal/akv"
	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/entra"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// API bundles the dependencies of the /v1 surface.
type API struct {
	Store *store.Store
	Auth  *auth.Validator
	// Entra drives workspace-identity provisioning in entra-emulator (nil
	// disables the identity endpoints with a 503).
	Entra *entra.Client
	// AKV resolves AzureKeyVaultReference connection credentials against a
	// Key Vault data plane (azure-keyvault-emulator in the family compose).
	AKV *akv.Client
	// RetryAfterSeconds is advertised on 202 responses.
	RetryAfterSeconds int
	// LRODelaySeconds is virtual seconds an operation stays Running.
	LRODelaySeconds int64

	// livy reverse-proxies the Livy endpoint to a real Spark backend
	// (nil = Livy routes 501). livyBackend is the same backend as a base URL,
	// used to open/tear down backend sessions for HC REPLs directly.
	livy        *httputil.ReverseProxy
	livyBackend *url.URL
	hcHTTP      *http.Client
	// hc holds high-concurrency Livy session-packing state (lazily created).
	hc *hcManager

	// Fault switches (set via the /_emulator control surface).
	mu        sync.Mutex
	failNext  int   // force the next N operations to Failed
	lroDelay  int64 // -1 = unset, otherwise overrides LRODelaySeconds
	rejectAll int   // force the next N requests to 500
}

// New constructs the API.
func New(st *store.Store, v *auth.Validator, retryAfter int, lroDelay int64) *API {
	return &API{Store: st, Auth: v, RetryAfterSeconds: retryAfter, LRODelaySeconds: lroDelay, lroDelay: -1}
}

// Register mounts the /v1 routes on mux.
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/workspaces", a.withAuth(a.listWorkspaces))
	mux.HandleFunc("POST /v1/workspaces", a.withAuth(a.createWorkspace))
	mux.HandleFunc("GET /v1/workspaces/{wid}", a.withAuth(a.getWorkspace))
	mux.HandleFunc("PATCH /v1/workspaces/{wid}", a.withAuth(a.updateWorkspace))
	mux.HandleFunc("DELETE /v1/workspaces/{wid}", a.withAuth(a.deleteWorkspace))

	mux.HandleFunc("GET /v1/workspaces/{wid}/roleAssignments", a.withAuth(a.listRoleAssignments))
	mux.HandleFunc("POST /v1/workspaces/{wid}/roleAssignments", a.withAuth(a.createRoleAssignment))
	mux.HandleFunc("PATCH /v1/workspaces/{wid}/roleAssignments/{raid}", a.withAuth(a.updateRoleAssignment))
	mux.HandleFunc("DELETE /v1/workspaces/{wid}/roleAssignments/{raid}", a.withAuth(a.deleteRoleAssignment))

	mux.HandleFunc("GET /v1/workspaces/{wid}/items", a.withAuth(a.listItems))
	mux.HandleFunc("POST /v1/workspaces/{wid}/items", a.withAuth(a.createItem))
	mux.HandleFunc("GET /v1/workspaces/{wid}/items/{iid}", a.withAuth(a.getItem))
	mux.HandleFunc("PATCH /v1/workspaces/{wid}/items/{iid}", a.withAuth(a.updateItem))
	mux.HandleFunc("DELETE /v1/workspaces/{wid}/items/{iid}", a.withAuth(a.deleteItem))

	mux.HandleFunc("POST /v1/workspaces/{wid}/items/{iid}/getDefinition", a.withAuth(a.getDefinition))
	mux.HandleFunc("POST /v1/workspaces/{wid}/items/{iid}/updateDefinition", a.withAuth(a.updateDefinition))

	mux.HandleFunc("POST /v1/workspaces/{wid}/items/{iid}/jobs/instances", a.withAuth(a.createJobInstance))
	mux.HandleFunc("GET /v1/workspaces/{wid}/items/{iid}/jobs/instances/{jid}", a.withAuth(a.getJobInstance))
	mux.HandleFunc("POST /v1/workspaces/{wid}/items/{iid}/jobs/instances/{jid}/cancel", a.withAuth(a.cancelJobInstance))
	mux.HandleFunc("POST /v1/workspaces/{wid}/items/{iid}/jobs/instances/{jid}/queryactivityruns", a.withAuth(a.queryActivityRuns))

	mux.HandleFunc("POST /v1/workspaces/{wid}/git/connect", a.withAuth(a.gitConnect))
	mux.HandleFunc("POST /v1/workspaces/{wid}/git/initializeConnection", a.withAuth(a.gitInitializeConnection))
	mux.HandleFunc("GET /v1/workspaces/{wid}/git/status", a.withAuth(a.gitStatus))
	mux.HandleFunc("POST /v1/workspaces/{wid}/git/commitToGit", a.withAuth(a.gitCommitToGit))
	mux.HandleFunc("POST /v1/workspaces/{wid}/git/updateFromGit", a.withAuth(a.gitUpdateFromGit))
	mux.HandleFunc("POST /v1/workspaces/{wid}/git/disconnect", a.withAuth(a.gitDisconnect))
	mux.HandleFunc("GET /v1/workspaces/{wid}/git/myGitCredentials", a.withAuth(a.gitMyCredentials))

	mux.HandleFunc("GET /v1/connections", a.withAuth(a.listConnections))
	mux.HandleFunc("POST /v1/connections", a.withAuth(a.createConnection))

	mux.HandleFunc("GET /v1/workspaces/{wid}/folders", a.withAuth(a.listFolders))
	mux.HandleFunc("POST /v1/workspaces/{wid}/folders", a.withAuth(a.createFolder))

	mux.HandleFunc("GET /v1/capacities", a.withAuth(a.listCapacities))
	mux.HandleFunc("POST /v1/workspaces/{wid}/assignToCapacity", a.withAuth(a.assignToCapacity))
	mux.HandleFunc("POST /v1/workspaces/{wid}/unassignFromCapacity", a.withAuth(a.unassignFromCapacity))

	mux.HandleFunc("POST /v1/workspaces/{wid}/provisionIdentity", a.withAuth(a.provisionIdentity))
	mux.HandleFunc("POST /v1/workspaces/{wid}/deprovisionIdentity", a.withAuth(a.deprovisionIdentity))

	a.registerTyped(mux)
	a.registerLivy(mux)
	a.registerShortcuts(mux)

	mux.HandleFunc("GET /v1/operations/{oid}", a.withAuth(a.getOperation))
	mux.HandleFunc("GET /v1/operations/{oid}/result", a.withAuth(a.getOperationResult))
}

// ---- wire shapes ----

// fabricError is the control plane's error envelope.
type fabricError struct {
	ErrorCode string `json:"errorCode"`
	Message   string `json:"message"`
	RequestID string `json:"requestId"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, fabricError{ErrorCode: code, Message: msg, RequestID: store.NewID()})
}

// ---- auth + RBAC plumbing ----

type handler func(w http.ResponseWriter, r *http.Request, p *auth.Principal)

// withAuth validates the bearer token and applies global fault switches.
func (a *API) withAuth(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		if a.rejectAll > 0 {
			a.rejectAll--
			a.mu.Unlock()
			writeErr(w, http.StatusInternalServerError, "InternalError", "Injected fault.")
			return
		}
		a.mu.Unlock()
		p, err := a.Auth.ValidateRequest(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer authorization_uri="`+a.Auth.Issuer+`"`)
			writeErr(w, http.StatusUnauthorized, "TokenInvalid", err.Error())
			return
		}
		h(w, r, p)
	}
}

// requireRole loads the workspace and the caller's role on it, enforcing a
// minimum. It 404s unknown workspaces and 403s principals with no grant —
// Fabric hides workspaces the caller cannot see, but our single-tenant
// emulator favors debuggability. The fetched workspace is returned so
// handlers don't query it twice.
func (a *API) requireRole(w http.ResponseWriter, wid string, p *auth.Principal, min string) (ws *store.Workspace, role string, ok bool) {
	ws, err := a.Store.GetWorkspace(wid)
	if err != nil {
		writeErr(w, http.StatusNotFound, "WorkspaceNotFound", "The workspace is not available.")
		return nil, "", false
	}
	role, err = a.Store.RoleOf(wid, p.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return nil, "", false
	}
	if role == "" || store.RoleRank(role) < store.RoleRank(min) {
		writeErr(w, http.StatusForbidden, "InsufficientPrivileges",
			fmt.Sprintf("The caller requires at least the %s role on the workspace.", min))
		return nil, "", false
	}
	return ws, role, true
}

// ---- fault control (wired to /_emulator/faults) ----

// SetFaults configures fault switches; negative values leave a field as-is.
func (a *API) SetFaults(failNext, rejectNext int, lroDelay int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if failNext >= 0 {
		a.failNext = failNext
	}
	if rejectNext >= 0 {
		a.rejectAll = rejectNext
	}
	a.lroDelay = lroDelay // -1 clears the override
}

// nextOpFate pops fault state for a new operation: its Running window and a
// forced failure code ("" = succeed).
func (a *API) nextOpFate() (delay int64, failWith string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delay = a.LRODelaySeconds
	if a.lroDelay >= 0 {
		delay = a.lroDelay
	}
	if a.failNext > 0 {
		a.failNext--
		failWith = "OperationFailed"
	}
	return delay, failWith
}

// startOperation records an LRO and writes the 202 envelope (both
// x-ms-operation-id — what documented scripts read — and Location).
func (a *API) startOperation(w http.ResponseWriter, r *http.Request, kind, resultRef string) {
	delay, failWith := a.nextOpFate()
	op := &store.Operation{Kind: kind, ResultRef: resultRef, FailWith: failWith}
	op.CompleteAt = a.Store.Now() + delay
	if err := a.Store.CreateOperation(op); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	loc := fmt.Sprintf("https://%s/v1/operations/%s", r.Host, op.ID)
	w.Header().Set("x-ms-operation-id", op.ID)
	w.Header().Set("Location", loc)
	w.Header().Set("Retry-After", fmt.Sprintf("%d", a.RetryAfterSeconds))
	w.WriteHeader(http.StatusAccepted)
}
