// Package api serves the Fabric control plane: /v1 workspaces, items, role
// assignments, and long-running operations, with Fabric-shaped errors and
// workspace RBAC enforced from the validated bearer principal.
package api

import (
	"context"
	"database/sql"
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
	// PBIAuth validates the Power BI-audience tokens the executeQueries endpoint
	// requires (nil disables the endpoint with a 501).
	PBIAuth *auth.Validator
	// Entra drives workspace-identity provisioning in entra-emulator (nil
	// disables the identity endpoints with a 503).
	Entra *entra.Client
	// AKV resolves AzureKeyVaultReference connection credentials against a
	// Key Vault data plane (azure-keyvault-emulator in the family compose).
	AKV *akv.Client
	// MirrorItem snapshots a Fabric SQL Database's SQL tables to OneLake Delta
	// (the mirroring). Wired by the server when a warehouse SQL backend is set;
	// nil → the refresh-mirror endpoint 501s.
	MirrorItem func(ctx context.Context, itemID string) error
	// Airflow runs ApacheAirflowJob DAGs on an attached upstream Airflow
	// instance. Nil preserves an honest AirflowNotConfigured failure.
	Airflow AirflowRuntime
	// WebActivityStub records a Web activity as Succeeded WITHOUT calling
	// anything — what the emulator used to do unconditionally. Off by default,
	// because a fabricated success is the dangerous version: a pipeline
	// branching on the response goes green here and behaves differently in
	// Fabric. On for a CI leg that must not reach the network.
	WebActivityStub bool
	// WebHTTP is the client Web activities use. Nil uses a shared default;
	// tests substitute one.
	WebHTTP *http.Client
	// MLflowURL is an attached real MLflow tracking/model-registry server. The
	// API authenticates and workspace-namespaces traffic before proxying it.
	MLflowURL  *url.URL
	MLflowHTTP *http.Client
	// KQLURL is an attached real Kusto engine (Microsoft's kustainer, or any
	// ADX/Eventhouse cluster) backing the Real-Time Intelligence surface. Nil
	// → the Kusto routes 501. KQLAuth validates the Kusto-audience tokens
	// those routes require.
	KQLURL  *url.URL
	KQLHTTP *http.Client
	KQLAuth *auth.Validator
	// kqlDatabases remembers which engine-side databases have been created,
	// guarded by kqlMu — separate from the fault mutex below so a slow engine
	// call never blocks an unrelated request.
	kqlMu        sync.Mutex
	kqlDatabases map[string]bool
	// SQLDB returns the real SQL Server connection for a Warehouse/SQLDatabase
	// item (preparing its database first). Wired by the server when a warehouse
	// SQL backend is set; nil → the pipeline Script/StoredProcedure activities
	// fail loudly (no SQL engine attached).
	SQLDB func(ctx context.Context, itemID string) (*sql.DB, error)
	// refreshes is per-dataset refresh history for the Power BI refresh
	// endpoints. In memory on purpose — see refreshes.go.
	refreshes refreshLog
	// scans holds completed admin metadata scans — see scanner.go.
	scans scanStore
	// SQLEndpointPort is the port the warehouse TDS listener serves on, used to
	// advertise a Warehouse's connectionString. Empty → a Warehouse reports no
	// connection string, which is honest: there is no SQL endpoint to connect
	// to when FABRIC_SQL_TDS_ADDR is unset.
	SQLEndpointPort string
	// webhookWaits holds parked WebHook activities: callback token -> the
	// channel that resumes the pipeline goroutine. In-memory on purpose — a
	// parked pipeline dies with the process, and pretending otherwise would be
	// a durability claim the emulator cannot honour.
	webhookWaits sync.Map
	// CustomActivityShell enables the Custom (Azure Batch) activity's shell
	// execution; false means it refuses by name. See config.CustomActivityShell.
	CustomActivityShell bool
	// RetryAfterSeconds is advertised on 202 responses.
	RetryAfterSeconds int
	// LRODelaySeconds is virtual seconds an operation stays Running.
	LRODelaySeconds int64
	// ListPageSize is the server's list page size (see pagination.go): 0 uses
	// DefaultListPageSize, negative disables paging.
	ListPageSize int

	// livy reverse-proxies the Livy endpoint to a real Spark backend
	// (nil = Livy routes 501). livyBackend is the same backend as a base URL,
	// used to open/tear down backend sessions for HC REPLs directly.
	livy        *httputil.ReverseProxy
	livyBackend *url.URL
	hcHTTP      *http.Client
	// livyAgent, when set, makes the emulator terminate the Livy protocol
	// itself and drive a Spark statement-executor agent (real interactive
	// sessions without an Apache Livy server). livyNativeState holds its
	// in-memory session/statement state.
	livyAgent *url.URL
	// tenantAdmins are the principal ids (oid or appid) the operator declared
	// Fabric administrators. Configuration rather than inference — see
	// tenantadmin.go for why a claim must not decide this.
	tenantAdmins    []string
	livyNativeState *livyManager
	// hc holds high-concurrency Livy session-packing state (lazily created).
	hc *hcManager

	// tickMu serialises scheduler/event-trigger evaluation so two concurrent
	// evaluations cannot read the same high-water mark and start the same run
	// twice. Separate from the fault mutex: an evaluation can execute a whole
	// pipeline, and must not block unrelated requests for that long.
	tickMu sync.Mutex

	// firing breaks event-trigger cycles — see internal/api/triggers.go.
	firing firingSet

	// Fault switches (set via the /_emulator control surface).
	mu        sync.Mutex
	failNext  int   // force the next N operations to Failed
	lroDelay  int64 // -1 = unset, otherwise overrides LRODelaySeconds
	rejectAll int   // force the next N requests to 500
}

// New constructs the API.
// ListPageSize is the server's list page size: 0 uses DefaultListPageSize,
// negative disables paging. Set it small to force clients through the
// continuation-token loop (see internal/api/pagination.go).
func New(st *store.Store, v *auth.Validator, retryAfter int, lroDelay int64) *API {
	return &API{Store: st, Auth: v, RetryAfterSeconds: retryAfter, LRODelaySeconds: lroDelay, lroDelay: -1}
}

// SetTenantAdmins declares which principals are Fabric administrators. Empty
// means nobody is: every mutating /v1/admin call is then refused, which is the
// honest default for an emulator nobody has configured — the alternative
// (everyone is an admin) is what this gate exists to end.
func (a *API) SetTenantAdmins(ids []string) { a.tenantAdmins = ids }

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

	mux.HandleFunc("POST /v1/workspaces/{wid}/items/{iid}/move", a.withAuth(a.moveItem))
	mux.HandleFunc("POST /v1/workspaces/{wid}/items/{iid}/getDefinition", a.withAuth(a.getDefinition))
	mux.HandleFunc("POST /v1/workspaces/{wid}/items/{iid}/updateDefinition", a.withAuth(a.updateDefinition))

	mux.HandleFunc("POST /v1/workspaces/{wid}/items/{iid}/jobs/instances", a.withAuth(a.createJobInstance))
	mux.HandleFunc("GET /v1/workspaces/{wid}/items/{iid}/jobs/instances/{jid}", a.withAuth(a.getJobInstance))
	mux.HandleFunc("POST /v1/workspaces/{wid}/items/{iid}/jobs/instances/{jid}/cancel", a.withAuth(a.cancelJobInstance))
	mux.HandleFunc("POST /v1/workspaces/{wid}/items/{iid}/jobs/instances/{jid}/queryactivityruns", a.withAuth(a.queryActivityRuns))
	mux.HandleFunc("GET /v1/workspaces/{wid}/items/{iid}/jobs/instances/{jid}/notebookRun", a.withAuth(a.getNotebookRun))
	mux.HandleFunc("POST /v1/workspaces/{wid}/items/{iid}/jobs/instances/{jid}/notebookRunResult", a.withAuth(a.reportNotebookRun))
	// No withAuth: the token in the path IS the credential, as ADF's own
	// callBackUri embeds its — an external receiver has no Fabric token.
	mux.HandleFunc("POST /v1/workspaces/{wid}/items/{iid}/jobs/instances/{jid}/webhookcallbacks/{token}", a.webhookCallbackHandler)
	mux.HandleFunc("GET /v1/workspaces/{wid}/items/{iid}/jobs/instances/{jid}/sparkJobRun", a.withAuth(a.getSparkJobRun))
	mux.HandleFunc("POST /v1/workspaces/{wid}/items/{iid}/jobs/instances/{jid}/sparkJobRunResult", a.withAuth(a.reportSparkJobRun))
	mux.HandleFunc("GET /v1/workspaces/{wid}/lineage", a.withAuth(a.listLineage))
	// Emulator-native: an engine that is not a queued notebook run reports what
	// it moved, so an interactive step reaches the graph too (reportlineage.go).
	mux.HandleFunc("POST /v1/workspaces/{wid}/lineage", a.withAuth(a.reportLineage))
	mux.HandleFunc("POST /v1/workspaces/{wid}/sqlDatabases/{iid}/refreshMirror", a.withAuth(a.refreshMirror))
	mux.HandleFunc("POST /v1/workspaces/{wid}/mirroredDatabases/{iid}/refreshMirror", a.withAuth(a.refreshMirroredDatabase))

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

	// Deployment pipelines (docs/23) — D0 model + read, D1 assignment +
	// pairing, D2 Deploy Stage Content over the existing LRO engine. The
	// role-assignment CRUD (D3).
	mux.HandleFunc("GET /v1/deploymentPipelines", a.withAuth(a.listDeploymentPipelines))
	mux.HandleFunc("POST /v1/deploymentPipelines", a.withAuth(a.createDeploymentPipeline))
	mux.HandleFunc("GET /v1/deploymentPipelines/{pid}", a.withAuth(a.getDeploymentPipeline))
	mux.HandleFunc("PATCH /v1/deploymentPipelines/{pid}", a.withAuth(a.updateDeploymentPipeline))
	mux.HandleFunc("DELETE /v1/deploymentPipelines/{pid}", a.withAuth(a.deleteDeploymentPipeline))
	mux.HandleFunc("GET /v1/deploymentPipelines/{pid}/stages", a.withAuth(a.listDeploymentStages))
	mux.HandleFunc("GET /v1/deploymentPipelines/{pid}/stages/{sid}", a.withAuth(a.getDeploymentStage))
	mux.HandleFunc("PATCH /v1/deploymentPipelines/{pid}/stages/{sid}", a.withAuth(a.updateDeploymentStage))
	mux.HandleFunc("GET /v1/deploymentPipelines/{pid}/stages/{sid}/items", a.withAuth(a.listDeploymentStageItems))
	mux.HandleFunc("POST /v1/deploymentPipelines/{pid}/stages/{sid}/assignWorkspace", a.withAuth(a.assignStageWorkspace))
	mux.HandleFunc("POST /v1/deploymentPipelines/{pid}/stages/{sid}/unassignWorkspace", a.withAuth(a.unassignStageWorkspace))
	mux.HandleFunc("POST /v1/deploymentPipelines/{pid}/deploy", a.withAuth(a.deployStageContent))
	mux.HandleFunc("GET /v1/deploymentPipelines/{pid}/operations", a.withAuth(a.listDeploymentOperations))
	mux.HandleFunc("GET /v1/deploymentPipelines/{pid}/operations/{oid}", a.withAuth(a.getDeploymentOperation))
	mux.HandleFunc("GET /v1/deploymentPipelines/{pid}/roleAssignments", a.withAuth(a.listDeploymentPipelineRoles))
	mux.HandleFunc("POST /v1/deploymentPipelines/{pid}/roleAssignments", a.withAuth(a.addDeploymentPipelineRole))
	mux.HandleFunc("DELETE /v1/deploymentPipelines/{pid}/roleAssignments/{prid}", a.withAuth(a.deleteDeploymentPipelineRole))

	a.registerSchedules(mux)
	a.registerTriggers(mux)
	a.registerTyped(mux)
	a.registerAdminDomains(mux)
	a.registerActivityEvents(mux)
	a.registerLabels(mux)
	a.registerTenantSettings(mux)
	a.registerAdminWorkspaces(mux)
	a.registerAdminItems(mux)
	a.registerCapacityOverrides(mux)
	a.registerLivy(mux)
	a.registerShortcuts(mux)
	a.registerExecuteQueries(mux)
	a.registerDatasets(mux)
	a.registerRefreshes(mux)
	a.registerDatasources(mux)
	a.registerScanner(mux)
	a.registerVSCodeCompatibility(mux)
	a.registerAirflow(mux)
	a.registerMLflow(mux)
	a.registerKQL(mux)

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
	// Real Fabric echoes the error code in a header as well as the body;
	// documented client code branches on it (fabric-docs
	// real-time-intelligence/map/tutorial-create-real-time-map-python.md).
	w.Header().Set("x-ms-public-api-error-code", code)
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
		if a.Auth == nil {
			// Symmetric with withPBIAuth. Without this the nil validator is a
			// method call on nil, which panics: net/http closes the socket and
			// the client sees EOF with nothing naming the cause. A 501 that
			// says "not configured" is the same information, delivered.
			writeErr(w, http.StatusNotImplemented, "AuthNotConfigured",
				"This emulator has no token validator configured.")
			return
		}
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
	a.startOperationWithID(w, r, "", kind, resultRef)
}

// startOperationWithID is startOperation for callers that must know the
// operation id BEFORE the operation exists — a deployment records its
// per-item detail under that id so /operations/{id}/result can serve it.
// An empty id is generated as usual.
func (a *API) startOperationWithID(w http.ResponseWriter, r *http.Request, id, kind, resultRef string) {
	delay, failWith := a.nextOpFate()
	op := &store.Operation{ID: id, Kind: kind, ResultRef: resultRef, FailWith: failWith}
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
