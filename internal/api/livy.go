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
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	proxy := httputil.NewSingleHostReverseProxy(u)
	// The default director joins the backend's base path with r.URL.Path, so
	// the handler sets r.URL.Path to just the Livy-native suffix
	// (/sessions, /batches, …) and the director prepends the backend base.
	base := proxy.Director
	proxy.Director = func(r *http.Request) {
		base(r)
		r.Host = u.Host
	}
	a.livy = proxy
	return nil
}

// registerLivy mounts the lakehouse-scoped Livy routes.
func (a *API) registerLivy(mux *http.ServeMux) {
	const p = "/v1/workspaces/{wid}/lakehouses/{lid}/livyapi/versions/{ver}/"
	for _, m := range []string{"GET", "POST", "DELETE"} {
		mux.HandleFunc(m+" "+p+"{livypath...}", a.withAuth(a.livyProxy))
	}
}

// livyProxy validates RBAC then reverse-proxies to the Livy backend. Session
// creation and job submission need write access (Contributor); reads
// (session/statement status) need Viewer.
func (a *API) livyProxy(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	min := store.RoleViewer
	if r.Method == http.MethodPost || r.Method == http.MethodDelete {
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
	if a.livy == nil {
		writeErr(w, http.StatusNotImplemented, "SparkBackendNotConfigured",
			"No Spark/Livy backend is configured; set --spark-livy-url to run Spark for real.")
		return
	}
	// Rewrite to the Livy-native suffix (/sessions|batches/…); the proxy
	// director prepends the backend's base path.
	r.URL.Path = "/" + r.PathValue("livypath")
	r.URL.RawPath = ""
	a.livy.ServeHTTP(w, r)
}
