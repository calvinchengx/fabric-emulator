package entra

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOriginFromIssuer(t *testing.T) {
	origin, err := OriginFromIssuer("https://entra:8443/tid/v2.0")
	if err != nil || origin != "https://entra:8443" {
		t.Fatalf("origin = %q, %v", origin, err)
	}
	for _, bad := range []string{"", "not-a-url", "/relative/path"} {
		if _, err := OriginFromIssuer(bad); err == nil {
			t.Errorf("OriginFromIssuer(%q) succeeded", bad)
		}
	}
}

// fakeEntra implements just enough of the admin API to unit-test the client.
func fakeEntra(t *testing.T, fail bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/api/workspace-identities", func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["workspaceName"] == "" || body["workspaceId"] == "" {
			t.Errorf("create body missing fields: %v", body)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "sp-1", "appId": "app-1", "state": "Active"})
	})
	mux.HandleFunc("PATCH /admin/api/workspace-identities/{id}", func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("DELETE /admin/api/workspace-identities/{id}", func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestClientHappyPath(t *testing.T) {
	srv := fakeEntra(t, false)
	c := New(srv.URL+"/", false, srv.Client()) // trailing slash trimmed

	wi, err := c.CreateWorkspaceIdentity("ws-1", "My Workspace")
	if err != nil || wi.ID != "sp-1" || wi.AppID != "app-1" {
		t.Fatalf("create = %+v, %v", wi, err)
	}
	if err := c.RenameWorkspaceIdentity("sp-1", "Renamed"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteWorkspaceIdentity("sp-1"); err != nil {
		t.Fatal(err)
	}
}

func TestClientErrors(t *testing.T) {
	srv := fakeEntra(t, true)
	c := New(srv.URL, false, srv.Client())
	if _, err := c.CreateWorkspaceIdentity("w", "n"); err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("create err = %v", err)
	}
	if err := c.RenameWorkspaceIdentity("sp", "n"); err == nil {
		t.Fatal("rename against failing entra succeeded")
	}
	if err := c.DeleteWorkspaceIdentity("sp"); err == nil {
		t.Fatal("delete against failing entra succeeded")
	}

	// Unreachable host, default client construction (nil override).
	dead := New("http://127.0.0.1:1", false, nil)
	if _, err := dead.CreateWorkspaceIdentity("w", "n"); err == nil {
		t.Fatal("unreachable entra succeeded")
	}

	// Non-JSON success body.
	junk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("not json"))
	}))
	defer junk.Close()
	cj := New(junk.URL, false, junk.Client())
	if _, err := cj.CreateWorkspaceIdentity("w", "n"); err == nil {
		t.Fatal("garbage JSON accepted")
	}
}

func TestValidateClientCredentials(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /{tenant}/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostFormValue("client_secret") != "good" || r.PathValue("tenant") != "tid" {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"x"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := New(srv.URL, false, srv.Client())
	if err := c.ValidateClientCredentials("tid", "cid", "good"); err != nil {
		t.Fatal(err)
	}
	if err := c.ValidateClientCredentials("tid", "cid", "bad"); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("bad secret err = %v", err)
	}
	dead := New("http://127.0.0.1:1", false, nil)
	if err := dead.ValidateClientCredentials("t", "c", "s"); err == nil {
		t.Fatal("unreachable entra accepted")
	}
}
