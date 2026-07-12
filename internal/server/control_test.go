package server

// Control-surface unit tests (no entra needed): clock GET/POST shapes,
// malformed bodies, and fault wiring.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/config"
)

func newControlServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Config{EntraIssuer: "https://unused/t/v2.0"}
	if err := cfg.Finish(); err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func (s *Server) hit(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestControlSurface(t *testing.T) {
	s := newControlServer(t)

	if w := s.hit(t, "GET", "/health", ""); w.Code != http.StatusOK {
		t.Fatalf("health = %d", w.Code)
	}
	if w := s.hit(t, "GET", "/_emulator/clock", ""); w.Code != http.StatusOK {
		t.Fatalf("clock get = %d", w.Code)
	}
	// Offset + freeze + advance in one body.
	w := s.hit(t, "POST", "/_emulator/clock", `{"offset":100,"freeze":true,"advance":5}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"frozen":true`) {
		t.Fatalf("clock post = %d %s", w.Code, w.Body.String())
	}
	// Unfreeze.
	w = s.hit(t, "POST", "/_emulator/clock", `{"freeze":false}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"frozen":false`) {
		t.Fatalf("unfreeze = %d %s", w.Code, w.Body.String())
	}
	// Malformed bodies 400.
	if w := s.hit(t, "POST", "/_emulator/clock", `{nope`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad clock body = %d", w.Code)
	}
	if w := s.hit(t, "POST", "/_emulator/faults", `{nope`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad faults body = %d", w.Code)
	}
	// Faults accept partial fields.
	if w := s.hit(t, "POST", "/_emulator/faults", `{"failNextOperations":1}`); w.Code != http.StatusOK {
		t.Fatalf("faults = %d", w.Code)
	}
	if w := s.hit(t, "POST", "/_emulator/faults", `{"rejectNextRequests":1,"lroDelaySeconds":5}`); w.Code != http.StatusOK {
		t.Fatalf("faults combo = %d", w.Code)
	}
	// The injected rejection fires on the next authenticated route.
	if w := s.hit(t, "GET", "/v1/workspaces", ""); w.Code != http.StatusInternalServerError {
		t.Fatalf("rejected request = %d; want 500", w.Code)
	}
}
