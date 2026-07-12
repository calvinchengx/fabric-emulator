package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// pv builds the path-value map the Livy routes carry.
func pv(wid, lid, livypath string) map[string]string {
	return map[string]string{"wid": wid, "lid": lid, "ver": "2023-12-01", "livypath": livypath}
}

func TestLivyPassthrough(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lh"}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}
	// viewer-1 (Viewer) is granted by seedWorkspace; nobody-1 has no role.

	// No backend configured → 501 (honest, not a faked session).
	if w := do(a.livyProxy, admin, "GET", "", pv(ws.ID, lake.ID, "sessions")); w.Code != http.StatusNotImplemented {
		t.Fatalf("unconfigured livy = %d; want 501", w.Code)
	}

	// A real (stub) Livy backend at a base path; assert the emulator rewrites
	// the Fabric-prefixed path to Livy's native path and forwards the bearer.
	var gotPath string
	livy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":7,"state":"starting"}`))
	}))
	defer livy.Close()
	if err := a.SetLivyBackend(livy.URL + "/livy"); err != nil {
		t.Fatal(err)
	}

	w := do(a.livyProxy, admin, "POST", `{"file":"nb.py"}`, pv(ws.ID, lake.ID, "batches"))
	if w.Code != http.StatusCreated {
		t.Fatalf("proxied POST = %d %s", w.Code, w.Body.Bytes())
	}
	if gotPath != "/livy/batches" {
		t.Fatalf("proxied path = %q; want /livy/batches", gotPath)
	}
	if !strings.Contains(w.Body.String(), `"id":7`) {
		t.Fatalf("Livy response not returned: %s", w.Body.String())
	}

	// A statements sub-path is forwarded verbatim after the version prefix.
	do(a.livyProxy, admin, "GET", "", pv(ws.ID, lake.ID, "sessions/3/statements"))
	if gotPath != "/livy/sessions/3/statements" {
		t.Fatalf("sub-path proxy path = %q", gotPath)
	}

	// RBAC: Viewer reads but cannot submit; ungranted 403s; unknown lakehouse 404s.
	if w := do(a.livyProxy, viewer, "GET", "", pv(ws.ID, lake.ID, "sessions")); w.Code != http.StatusCreated {
		t.Fatalf("viewer read = %d", w.Code)
	}
	if w := do(a.livyProxy, viewer, "POST", "{}", pv(ws.ID, lake.ID, "batches")); w.Code != http.StatusForbidden {
		t.Fatalf("viewer submit = %d; want 403", w.Code)
	}
	if w := do(a.livyProxy, &authNobody, "GET", "", pv(ws.ID, lake.ID, "sessions")); w.Code != http.StatusForbidden {
		t.Fatalf("ungranted = %d; want 403", w.Code)
	}
	if w := do(a.livyProxy, admin, "GET", "", pv(ws.ID, "missing-lake", "sessions")); w.Code != http.StatusNotFound {
		t.Fatalf("unknown lakehouse = %d; want 404", w.Code)
	}

	// Clearing the backend restores 501.
	if err := a.SetLivyBackend(""); err != nil {
		t.Fatal(err)
	}
	if w := do(a.livyProxy, admin, "GET", "", pv(ws.ID, lake.ID, "sessions")); w.Code != http.StatusNotImplemented {
		t.Fatalf("cleared backend = %d; want 501", w.Code)
	}
}

// authNobody is an ungranted principal (distinct from the seeded viewer).
var authNobody = *nobody

func TestSetLivyBackendBadURL(t *testing.T) {
	a, _ := newAPI(t)
	if err := a.SetLivyBackend("://not a url"); err == nil {
		t.Fatal("bad Livy URL accepted")
	}
}
