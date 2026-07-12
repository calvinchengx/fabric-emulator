package akv

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeVault(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /secrets/{name}", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api-version") == "" {
			http.Error(w, `{"error":{"code":"BadParameter"}}`, http.StatusBadRequest)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, `{"error":{"code":"Unauthorized"}}`, http.StatusUnauthorized)
			return
		}
		if r.PathValue("name") != "db-password" {
			http.Error(w, `{"error":{"code":"SecretNotFound"}}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"value":"hunter2","id":"https://v/secrets/db-password/1"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestResolveSecret(t *testing.T) {
	srv := fakeVault(t)
	c := New(false, srv.Client())

	v, err := c.ResolveSecret(srv.URL+"/", "db-password", "tok")
	if err != nil || v != "hunter2" {
		t.Fatalf("resolve = %q, %v", v, err)
	}
	if _, err := c.ResolveSecret(srv.URL, "missing", "tok"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("missing secret err = %v", err)
	}
	// Unreachable vault; default client construction.
	dead := New(false, nil)
	if _, err := dead.ResolveSecret("http://127.0.0.1:1", "s", "t"); err == nil {
		t.Fatal("unreachable vault accepted")
	}
	// Non-JSON success body.
	junk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer junk.Close()
	cj := New(false, junk.Client())
	if _, err := cj.ResolveSecret(junk.URL, "s", "t"); err == nil {
		t.Fatal("garbage vault JSON accepted")
	}
}
