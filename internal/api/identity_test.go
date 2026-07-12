package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/entra"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// failingEntra always 500s — for the 502 branches.
func failingEntra(t *testing.T) *entra.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return entra.New(srv.URL, false, srv.Client())
}

// okEntra returns a minimal healthy identity API.
func okEntra(t *testing.T) *entra.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/api/workspace-identities", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"sp-9","appId":"app-9","state":"Active"}`))
	})
	mux.HandleFunc("PATCH /admin/api/workspace-identities/{id}", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("DELETE /admin/api/workspace-identities/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return entra.New(srv.URL, false, srv.Client())
}

func TestIdentityHandlerErrors(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	wid := map[string]string{"wid": ws.ID}

	// Viewer cannot provision; no client configured → 503.
	if w := do(a.provisionIdentity, viewer, "POST", "", wid); w.Code != http.StatusForbidden {
		t.Fatalf("viewer provision = %d", w.Code)
	}
	if w := do(a.provisionIdentity, admin, "POST", "", wid); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("provision with nil entra = %d", w.Code)
	}
	// Deprovision with no identity → 404.
	if w := do(a.deprovisionIdentity, admin, "POST", "", wid); w.Code != http.StatusNotFound {
		t.Fatalf("deprovision without identity = %d", w.Code)
	}

	// Failing entra → 502 on provision.
	a.Entra = failingEntra(t)
	if w := do(a.provisionIdentity, admin, "POST", "", wid); w.Code != http.StatusBadGateway {
		t.Fatalf("provision vs failing entra = %d", w.Code)
	}

	// Healthy entra: provision succeeds and grants the SP Admin.
	a.Entra = okEntra(t)
	if w := do(a.provisionIdentity, admin, "POST", "", wid); w.Code != http.StatusAccepted {
		t.Fatalf("provision = %d %s", w.Code, w.Body.Bytes())
	}
	role, err := st.RoleOf(ws.ID, "app-9")
	if err != nil || role != store.RoleAdmin {
		t.Fatalf("identity role = %q, %v; want Admin", role, err)
	}

	// Rename-follows via a failing entra → 502 on workspace PATCH.
	a.Entra = failingEntra(t)
	if w := do(a.updateWorkspace, admin, "PATCH", `{"displayName":"new-name"}`, wid); w.Code != http.StatusBadGateway {
		t.Fatalf("rename with failing entra = %d", w.Code)
	}
	// Description-only patch does not touch entra.
	if w := do(a.updateWorkspace, admin, "PATCH", `{"description":"d"}`, wid); w.Code != http.StatusOK {
		t.Fatalf("description patch = %d", w.Code)
	}
	// Cascade delete via failing entra → 502; workspace survives.
	if w := do(a.deleteWorkspace, admin, "DELETE", "", wid); w.Code != http.StatusBadGateway {
		t.Fatalf("delete with failing entra = %d", w.Code)
	}
	if _, err := st.GetWorkspace(ws.ID); err != nil {
		t.Fatal("workspace deleted despite failed identity cascade")
	}
	// Deprovision via failing entra → 502.
	if w := do(a.deprovisionIdentity, admin, "POST", "", wid); w.Code != http.StatusBadGateway {
		t.Fatalf("deprovision vs failing entra = %d", w.Code)
	}

	// Healthy entra again: deprovision revokes the grant.
	a.Entra = okEntra(t)
	if w := do(a.deprovisionIdentity, admin, "POST", "", wid); w.Code != http.StatusAccepted {
		t.Fatalf("deprovision = %d %s", w.Code, w.Body.Bytes())
	}
	if role, _ := st.RoleOf(ws.ID, "app-9"); role != "" {
		t.Fatalf("identity grant survived deprovision: %q", role)
	}
	// Rename with no identity: entra untouched, PATCH succeeds.
	if w := do(a.updateWorkspace, admin, "PATCH", `{"displayName":"final"}`, wid); w.Code != http.StatusOK {
		t.Fatalf("rename without identity = %d", w.Code)
	}
}

func TestIdentityDeprovisionNilClientWithRow(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	if err := st.SetWorkspaceIdentity(&store.WorkspaceIdentity{WorkspaceID: ws.ID, IdentityID: "sp", AppID: "app"}); err != nil {
		t.Fatal(err)
	}
	if w := do(a.deprovisionIdentity, admin, "POST", "", map[string]string{"wid": ws.ID}); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("deprovision nil client = %d", w.Code)
	}
}
