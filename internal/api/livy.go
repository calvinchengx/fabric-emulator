package api

// Livy passthrough (R2/B). Fabric exposes Spark through the Apache Livy REST
// API at a lakehouse-scoped endpoint:
//
//   /v1/workspaces/{wid}/lakehouses/{lid}/livyapi/versions/{ver}/{sessions|batches}/…
//
// The emulator validates the bearer token and workspace RBAC (like every /v1
// route), then reverse-proxies the remainder to a real Apache Livy backend —
// so Spark actually executes. Without a backend configured the routes 501,
// honestly (no faked sessions).

import (
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// SetLivyBackend configures the real Livy URL the emulator proxies to (empty
// disables the Livy routes with a 501).
func (a *API) SetLivyBackend(rawURL string) error {
	if rawURL == "" {
		a.livy = nil
		a.livyBackend = nil
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	a.livyBackend = u
	// Rewrite, not Director: Director is deprecated as of Go 1.26, which this
	// module requires. Constructed directly because NewSingleHostReverseProxy
	// installs a Director, and ReverseProxy refuses to have both.
	//
	// SetURL is the replacement for the wrapped default director: it joins the
	// backend's base path with the inbound path, which is what the handler
	// depends on when it sets r.URL.Path to just the Livy-native suffix
	// (/sessions, /batches, …) and lets the proxy prepend the backend base.
	//
	// SetXForwarded is called explicitly because ReverseProxy adds
	// X-Forwarded-For for a Director and NOT for a Rewrite, so omitting it
	// would silently drop a header this proxy used to send.
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetXForwarded()
			pr.SetURL(u)
			pr.Out.Host = u.Host
		},
	}
	a.livy = proxy
	return nil
}

// registerLivy mounts the lakehouse-scoped Livy routes — the classic
// sessions/batches proxy plus the Fabric high-concurrency layer.
func (a *API) registerLivy(mux *http.ServeMux) {
	const p = "/v1/workspaces/{wid}/lakehouses/{lid}/livyapi/versions/{ver}/"
	for _, m := range []string{"GET", "POST", "DELETE"} {
		mux.HandleFunc(m+" "+p+"{livypath...}", a.withAuth(a.livyProxy))
	}
	a.registerHCLivy(mux)
}

// hcClient is the HTTP client used to open/tear down backend Livy sessions for
// HC REPLs (distinct from the reverse proxy, which streams statements).
func (a *API) hcClient() *http.Client {
	if a.hcHTTP != nil {
		return a.hcHTTP
	}
	return http.DefaultClient
}

// livyProxy validates RBAC then reverse-proxies to the Livy backend. Session
// creation and job submission need write access (Contributor); reads
// (session/statement status) need Viewer.
func (a *API) livyProxy(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	// RUNNING A QUERY IS READING, so a Viewer may open a session and submit
	// statements. That is not a relaxation for its own sake: OneLake security
	// exists to filter exactly this caller — "filtering applies to Viewer and
	// to users granted access through OneLake security roles"
	// (data-engineering/spark-onelake-security.md) — and a Viewer refused a
	// session is a Viewer whose row-level security can never apply.
	//
	// What stops a Viewer WRITING is not this gate: the OneLake surface refuses
	// their writes whatever a role grants (docs/54, stage 3), so a statement
	// that tries fails there, where the data is, rather than here.
	//
	// DELETE stays Contributor: sessions are not owner-scoped, so a
	// Viewer-level delete would let one caller close another's session.
	min := store.RoleViewer
	if r.Method == http.MethodDelete {
		min = store.RoleContributor
	}
	if _, _, ok := a.requireRole(w, wid, p, min); !ok {
		return
	}
	// The lakehouse must exist in the workspace (its id anchors the endpoint).
	if _, err := a.Store.GetItem(wid, r.PathValue("lid")); err != nil {
		writeErr(w, http.StatusNotFound, "LakehouseNotFound", "The lakehouse is not available.")
		return
	}
	// With a statement-executor agent configured, terminate Livy natively and
	// drive real Spark ourselves (no Apache Livy server needed).
	if a.livyAgent != nil {
		a.livyNative(w, r, p)
		return
	}
	if a.livy == nil {
		writeErr(w, http.StatusNotImplemented, "SparkBackendNotConfigured",
			"No Spark/Livy backend is configured; set --spark-livy-url (proxy) or --spark-agent-url (native) to run Spark for real.")
		return
	}
	// Rewrite to the Livy-native suffix (/sessions|batches/…); the proxy
	// director prepends the backend's base path.
	r.URL.Path = "/" + r.PathValue("livypath")
	r.URL.RawPath = ""
	a.livy.ServeHTTP(w, r)
}
