package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func hcPV(wid, lid string, extra map[string]string) map[string]string {
	m := map[string]string{"wid": wid, "lid": lid, "ver": "2023-12-01"}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

type hcResp struct {
	ID, State, SessionId, ReplId, WorkspaceId, ArtifactId, CreatorId, SessionTag string
}

func acquire(t *testing.T, a *API, wid, lid, tag string) hcResp {
	t.Helper()
	body := "{}"
	if tag != "" {
		body = `{"sessionTag":"` + tag + `"}`
	}
	w := do(a.acquireHC, admin, "POST", body, hcPV(wid, lid, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("acquire = %d %s", w.Code, w.Body.Bytes())
	}
	var r hcResp
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// TestHCSessionPacking covers the Fabric-specific packing layer with no Spark
// backend: sessionTag packing, the 5-REPL cap, spill-to-new-session, non-
// idempotency, and slot reuse after release.
func TestHCSessionPacking(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lh"}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}

	// Five acquires with the same tag pack into one underlying Livy session.
	ids, repls := map[string]bool{}, map[string]bool{}
	var group string
	var first []hcResp
	for i := 0; i < maxReplsPerLivySession; i++ {
		r := acquire(t, a, ws.ID, lake.ID, "etl")
		first = append(first, r)
		if r.SessionTag != "etl" {
			t.Fatalf("sessionTag = %q", r.SessionTag)
		}
		if group == "" {
			group = r.SessionId
		} else if r.SessionId != group {
			t.Fatalf("REPL %d packed into %s, want shared %s", i, r.SessionId, group)
		}
		ids[r.ID] = true
		repls[r.ReplId] = true
	}
	// Non-idempotent: distinct HC ids and distinct REPL ids across the five.
	if len(ids) != 5 || len(repls) != 5 {
		t.Fatalf("expected 5 distinct HC ids and REPLs, got %d/%d", len(ids), len(repls))
	}

	// The sixth same-tag acquire spills to a NEW underlying session (cap is 5).
	sixth := acquire(t, a, ws.ID, lake.ID, "etl")
	if sixth.SessionId == group {
		t.Fatalf("sixth REPL should spill to a new session, still %s", group)
	}

	// A different tag never packs into the first group.
	other := acquire(t, a, ws.ID, lake.ID, "reports")
	if other.SessionId == group || other.SessionId == sixth.SessionId {
		t.Fatalf("different tag packed into an existing group")
	}

	// No tag → each acquire opens its own session.
	n1 := acquire(t, a, ws.ID, lake.ID, "")
	n2 := acquire(t, a, ws.ID, lake.ID, "")
	if n1.SessionId == n2.SessionId {
		t.Fatalf("untagged acquires shared a session")
	}

	// Release one REPL from the full first group, then a same-tag acquire
	// repacks into it (the freed slot), not a brand-new session.
	if w := do(a.deleteHC, admin, "DELETE", "", hcPV(ws.ID, lake.ID, map[string]string{"hcid": first[0].ID})); w.Code != http.StatusOK {
		t.Fatalf("delete = %d", w.Code)
	}
	repacked := acquire(t, a, ws.ID, lake.ID, "etl")
	if repacked.SessionId != group {
		t.Fatalf("after releasing a slot, acquire should repack into %s, got %s", group, repacked.SessionId)
	}

	// get returns a live session; a deleted one 404s.
	if w := do(a.getHC, admin, "GET", "", hcPV(ws.ID, lake.ID, map[string]string{"hcid": sixth.ID})); w.Code != http.StatusOK {
		t.Fatalf("get = %d", w.Code)
	}
	if w := do(a.getHC, admin, "GET", "", hcPV(ws.ID, lake.ID, map[string]string{"hcid": first[0].ID})); w.Code != http.StatusNotFound {
		t.Fatalf("get deleted = %d; want 404", w.Code)
	}
	if w := do(a.getHC, admin, "GET", "", hcPV(ws.ID, lake.ID, map[string]string{"hcid": "nope"})); w.Code != http.StatusNotFound {
		t.Fatalf("get unknown = %d; want 404", w.Code)
	}
}

func TestHCRBACAndScope(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lh"}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}
	// Viewer cannot acquire (Contributor) but can read; ungranted 403; unknown lakehouse 404.
	if w := do(a.acquireHC, viewer, "POST", "{}", hcPV(ws.ID, lake.ID, nil)); w.Code != http.StatusForbidden {
		t.Fatalf("viewer acquire = %d; want 403", w.Code)
	}
	r := acquire(t, a, ws.ID, lake.ID, "t")
	if w := do(a.getHC, viewer, "GET", "", hcPV(ws.ID, lake.ID, map[string]string{"hcid": r.ID})); w.Code != http.StatusOK {
		t.Fatalf("viewer read = %d", w.Code)
	}
	if w := do(a.acquireHC, &authNobody, "POST", "{}", hcPV(ws.ID, lake.ID, nil)); w.Code != http.StatusForbidden {
		t.Fatalf("ungranted acquire = %d; want 403", w.Code)
	}
	if w := do(a.acquireHC, admin, "POST", "{}", hcPV(ws.ID, "missing", nil)); w.Code != http.StatusNotFound {
		t.Fatalf("unknown lakehouse = %d; want 404", w.Code)
	}
}

// TestHCStatementProxy drives statement execution through a REPL onto a real
// (stub) Livy backend: the backend session is opened lazily and statements
// proxy to it; teardown deletes it; without a backend it is an honest 501.
func TestHCStatementProxy(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lh"}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}
	r := acquire(t, a, ws.ID, lake.ID, "run")

	// No backend → statements 501 (honest), management already worked.
	post := hcPV(ws.ID, lake.ID, map[string]string{"sid": r.ID, "replid": r.ReplId})
	if w := do(a.hcStatement, admin, "POST", `{"code":"1+1"}`, post); w.Code != http.StatusNotImplemented {
		t.Fatalf("statement without backend = %d; want 501", w.Code)
	}

	var mu sync.Mutex
	seen := []string{}
	livy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		seen = append(seen, req.Method+" "+req.URL.Path)
		mu.Unlock()
		switch {
		case req.Method == "POST" && req.URL.Path == "/sessions":
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"id":42,"state":"starting"}`))
		case req.URL.Path == "/sessions/42/statements":
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"id":0,"state":"waiting"}`))
		default:
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"id":0,"state":"available"}`))
		}
	}))
	defer livy.Close()
	if err := a.SetLivyBackend(livy.URL); err != nil {
		t.Fatal(err)
	}

	// POST a statement: opens the backend session (lazy) then proxies.
	if w := do(a.hcStatement, admin, "POST", `{"code":"1+1"}`, post); w.Code != http.StatusCreated {
		t.Fatalf("statement = %d %s", w.Code, w.Body.Bytes())
	}
	// GET the statement result proxies to the same backend session.
	get := hcPV(ws.ID, lake.ID, map[string]string{"sid": r.ID, "replid": r.ReplId, "stid": "0"})
	if w := do(a.hcStatement, viewer, "GET", "", get); w.Code != http.StatusOK {
		t.Fatalf("statement get = %d", w.Code)
	}
	mu.Lock()
	got := append([]string{}, seen...)
	mu.Unlock()
	want := []string{"POST /sessions", "POST /sessions/42/statements", "GET /sessions/42/statements/0"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("backend calls = %v; want %v", got, want)
	}

	// Delete tears down the backend session.
	if w := do(a.deleteHC, admin, "DELETE", "", hcPV(ws.ID, lake.ID, map[string]string{"hcid": r.ID})); w.Code != http.StatusOK {
		t.Fatalf("delete = %d", w.Code)
	}
	mu.Lock()
	last := seen[len(seen)-1]
	mu.Unlock()
	if last != "DELETE /sessions/42" {
		t.Fatalf("expected backend session teardown, got %q", last)
	}
}

// TestHCStatementGetBeforeExecute: reading a statement before any was submitted
// (no backend session yet) is a 404, not a proxy error.
func TestHCStatementGetBeforeExecute(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lh"}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}
	livy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer livy.Close()
	if err := a.SetLivyBackend(livy.URL); err != nil {
		t.Fatal(err)
	}
	r := acquire(t, a, ws.ID, lake.ID, "")
	get := hcPV(ws.ID, lake.ID, map[string]string{"sid": r.ID, "replid": r.ReplId, "stid": "0"})
	if w := do(a.hcStatement, admin, "GET", "", get); w.Code != http.StatusNotFound {
		t.Fatalf("statement get before execute = %d; want 404", w.Code)
	}
	// An unknown REPL 404s.
	bad := hcPV(ws.ID, lake.ID, map[string]string{"sid": r.ID, "replid": "nope", "stid": "0"})
	if w := do(a.hcStatement, admin, "GET", "", bad); w.Code != http.StatusNotFound {
		t.Fatalf("unknown repl = %d; want 404", w.Code)
	}
}
