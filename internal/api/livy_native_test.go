package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// stubAgent is a fake statement-executor agent: /health is idle, /statements
// echoes a canned Spark-ish REPL result and records the code it received.
func stubAgent(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var codes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"state":"idle"}`))
		case "/statements":
			b, _ := io.ReadAll(r.Body)
			var req struct{ Session, Code string }
			_ = json.Unmarshal(b, &req)
			mu.Lock()
			codes = append(codes, req.Session+":"+req.Code)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"status":"ok","execution_count":0,"data":{"text/plain":"5"}}`))
		case "/close":
			_, _ = w.Write([]byte(`{"closed":true}`))
		default:
			w.WriteHeader(404)
		}
	}))
	return srv, &codes
}

// TestLivyNativeInteractiveSession drives the full interactive Livy contract
// through the emulator's native layer against a stub agent: create session →
// poll idle → submit statement → poll available result → delete. This is the
// protocol termination; the real-Spark result is proven by the e2e.
func TestLivyNativeInteractiveSession(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lh"}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}
	agent, codes := stubAgent(t)
	defer agent.Close()
	if err := a.SetLivyAgent(agent.URL); err != nil {
		t.Fatal(err)
	}

	// Create an interactive session.
	w := do(a.livyProxy, admin, "POST", `{"kind":"pyspark"}`, pv(ws.ID, lake.ID, "sessions"))
	if w.Code != http.StatusCreated {
		t.Fatalf("create session = %d %s", w.Code, w.Body.Bytes())
	}
	var sess struct {
		ID    int
		State string
	}
	_ = json.Unmarshal(w.Body.Bytes(), &sess)
	if sess.State != "idle" {
		t.Fatalf("session state = %q", sess.State)
	}

	// Poll the session (a real client waits for idle before submitting).
	if w := do(a.livyProxy, viewer, "GET", "", pv(ws.ID, lake.ID, "sessions/0")); w.Code != http.StatusOK {
		t.Fatalf("get session = %d", w.Code)
	}

	// Submit a statement — the native layer drives the agent's REPL.
	w = do(a.livyProxy, admin, "POST", `{"code":"spark.range(5).count()"}`, pv(ws.ID, lake.ID, "sessions/0/statements"))
	if w.Code != http.StatusCreated {
		t.Fatalf("submit statement = %d %s", w.Code, w.Body.Bytes())
	}

	// Poll the statement result.
	w = do(a.livyProxy, viewer, "GET", "", pv(ws.ID, lake.ID, "sessions/0/statements/0"))
	if w.Code != http.StatusOK {
		t.Fatalf("get statement = %d", w.Code)
	}
	var stmt struct {
		State  string
		Output struct{ Data map[string]string }
	}
	_ = json.Unmarshal(w.Body.Bytes(), &stmt)
	if stmt.State != "available" || stmt.Output.Data["text/plain"] != "5" {
		t.Fatalf("statement result = %+v", stmt)
	}
	// The code reached the agent tagged with the session namespace.
	if len(*codes) != 1 || !strings.HasPrefix((*codes)[0], "0:") || !strings.Contains((*codes)[0], "count()") {
		t.Fatalf("agent received = %v", *codes)
	}

	// Delete the session.
	if w := do(a.livyProxy, admin, "DELETE", "", pv(ws.ID, lake.ID, "sessions/0")); w.Code != http.StatusOK {
		t.Fatalf("delete session = %d", w.Code)
	}
	if w := do(a.livyProxy, viewer, "GET", "", pv(ws.ID, lake.ID, "sessions/0")); w.Code != http.StatusNotFound {
		t.Fatalf("get after delete = %d; want 404", w.Code)
	}
}

func TestSetLivyAgent(t *testing.T) {
	a, _ := newAPI(t)
	if err := a.SetLivyAgent("http://agent:8099"); err != nil || a.livyAgent == nil {
		t.Fatalf("set = %v", err)
	}
	if err := a.SetLivyAgent(""); err != nil || a.livyAgent != nil {
		t.Fatalf("disable = %v", err)
	}
	if err := a.SetLivyAgent("://bad"); err == nil {
		t.Fatal("bad URL accepted")
	}
}

// TestLivyNativeErrors covers the native layer's error paths.
func TestLivyNativeErrors(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lh"}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}
	agent, _ := stubAgent(t)
	if err := a.SetLivyAgent(agent.URL); err != nil {
		t.Fatal(err)
	}

	// Unknown session / bad id / unknown statement.
	if w := do(a.livyProxy, viewer, "GET", "", pv(ws.ID, lake.ID, "sessions/99")); w.Code != http.StatusNotFound {
		t.Fatalf("unknown session = %d", w.Code)
	}
	if w := do(a.livyProxy, viewer, "GET", "", pv(ws.ID, lake.ID, "sessions/notanint")); w.Code != http.StatusBadRequest {
		t.Fatalf("bad session id = %d", w.Code)
	}
	do(a.livyProxy, admin, "POST", `{"kind":"pyspark"}`, pv(ws.ID, lake.ID, "sessions"))
	if w := do(a.livyProxy, viewer, "GET", "", pv(ws.ID, lake.ID, "sessions/0/statements/7")); w.Code != http.StatusNotFound {
		t.Fatalf("unknown statement = %d", w.Code)
	}
	// Batches are not part of the native interactive layer.
	if w := do(a.livyProxy, admin, "POST", "{}", pv(ws.ID, lake.ID, "batches")); w.Code != http.StatusNotImplemented {
		t.Fatalf("batches = %d; want 501", w.Code)
	}
	// List sessions returns the one we created.
	w := do(a.livyProxy, viewer, "GET", "", pv(ws.ID, lake.ID, "sessions"))
	if w.Code != http.StatusOK {
		t.Fatalf("list sessions = %d", w.Code)
	}
	var list struct{ Sessions []map[string]any }
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Sessions) != 1 {
		t.Fatalf("list = %+v", list.Sessions)
	}
	// An unknown Livy path 404s.
	if w := do(a.livyProxy, viewer, "GET", "", pv(ws.ID, lake.ID, "bogus/path")); w.Code != http.StatusNotFound {
		t.Fatalf("unknown path = %d; want 404", w.Code)
	}

	// Agent unreachable → 502 on create.
	agent.Close()
	if w := do(a.livyProxy, admin, "POST", "{}", pv(ws.ID, lake.ID, "sessions")); w.Code != http.StatusBadGateway {
		t.Fatalf("unreachable agent = %d; want 502", w.Code)
	}
}
