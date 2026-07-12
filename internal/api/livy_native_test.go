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
	if w := do(a.livyProxy, viewer, "GET", "", pv(ws.ID, lake.ID, "sessions/0/statements/notanint")); w.Code != http.StatusNotFound {
		t.Fatalf("non-int statement id = %d", w.Code)
	}
	// Bad batch id form.
	if w := do(a.livyProxy, viewer, "GET", "", pv(ws.ID, lake.ID, "batches/notanint")); w.Code != http.StatusBadRequest {
		t.Fatalf("bad batch id = %d", w.Code)
	}
	// A batch with no file is a bad request (batches are implemented).
	if w := do(a.livyProxy, admin, "POST", "{}", pv(ws.ID, lake.ID, "batches")); w.Code != http.StatusBadRequest {
		t.Fatalf("batch without file = %d; want 400", w.Code)
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

	// Agent unreachable → 502 on create, and on submitting to a live session.
	agent.Close()
	if w := do(a.livyProxy, admin, "POST", "{}", pv(ws.ID, lake.ID, "sessions")); w.Code != http.StatusBadGateway {
		t.Fatalf("unreachable agent = %d; want 502", w.Code)
	}
	if w := do(a.livyProxy, admin, "POST", `{"code":"x"}`, pv(ws.ID, lake.ID, "sessions/0/statements")); w.Code != http.StatusBadGateway {
		t.Fatalf("statement after agent died = %d; want 502", w.Code)
	}
}

// TestHCNativeStatements: with the agent configured, HC REPL statements run on
// real Spark — each REPL its own namespace (the 5-REPL model made real).
func TestHCNativeStatements(t *testing.T) {
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

	// Acquire an HC session (a REPL slot), then run a statement in it.
	r := acquire(t, a, ws.ID, lake.ID, "etl")
	post := hcPV(ws.ID, lake.ID, map[string]string{"sid": r.ID, "replid": r.ReplId})
	w := do(a.hcStatement, admin, "POST", `{"code":"spark.range(5).count()"}`, post)
	if w.Code != http.StatusCreated {
		t.Fatalf("hc statement = %d %s", w.Code, w.Body.Bytes())
	}
	var stmt struct {
		Output struct{ Data map[string]string }
	}
	_ = json.Unmarshal(w.Body.Bytes(), &stmt)
	if stmt.Output.Data["text/plain"] != "5" {
		t.Fatalf("hc statement result = %+v", stmt)
	}
	// The code reached the agent tagged with this REPL's namespace.
	if len(*codes) != 1 || !strings.HasPrefix((*codes)[0], r.ReplId+":") {
		t.Fatalf("agent codes = %v (want prefix %s)", *codes, r.ReplId)
	}
	// Read it back.
	get := hcPV(ws.ID, lake.ID, map[string]string{"sid": r.ID, "replid": r.ReplId, "stid": "0"})
	if w := do(a.hcStatement, viewer, "GET", "", get); w.Code != http.StatusOK {
		t.Fatalf("hc statement get = %d", w.Code)
	}
	// Delete releases the REPL (and closes its agent namespace).
	if w := do(a.deleteHC, admin, "DELETE", "", hcPV(ws.ID, lake.ID, map[string]string{"hcid": r.ID})); w.Code != http.StatusOK {
		t.Fatalf("delete hc = %d", w.Code)
	}
}

// TestLivyNativeBatch: a batch runs a real script fetched from OneLake through
// the agent, tracking terminal state + log.
func TestLivyNativeBatch(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := seedLakehouse(t, st, ws.ID, "lh")
	seedFile(t, st, ws.ID, lake.ID, "Files/job.py", []byte("print('hi')\nspark.range(3).count()\n"))
	agent, codes := stubAgent(t)
	defer agent.Close()
	if err := a.SetLivyAgent(agent.URL); err != nil {
		t.Fatal(err)
	}

	// Submit the batch by OneLake reference.
	body := `{"file":"` + ws.ID + `/` + lake.ID + `/Files/job.py"}`
	w := do(a.livyProxy, admin, "POST", body, pv(ws.ID, lake.ID, "batches"))
	if w.Code != http.StatusCreated {
		t.Fatalf("create batch = %d %s", w.Code, w.Body.Bytes())
	}
	var batch struct {
		ID    int
		State string
		Log   []string
	}
	_ = json.Unmarshal(w.Body.Bytes(), &batch)
	if batch.State != "success" {
		t.Fatalf("batch state = %q", batch.State)
	}
	// The script content reached the agent under a batch namespace.
	if len(*codes) != 1 || !strings.HasPrefix((*codes)[0], "batch-0:") || !strings.Contains((*codes)[0], "spark.range(3)") {
		t.Fatalf("agent codes = %v", *codes)
	}
	// State + log endpoints.
	if w := do(a.livyProxy, viewer, "GET", "", pv(ws.ID, lake.ID, "batches/0")); w.Code != http.StatusOK {
		t.Fatalf("get batch = %d", w.Code)
	}
	if w := do(a.livyProxy, viewer, "GET", "", pv(ws.ID, lake.ID, "batches/0/log")); w.Code != http.StatusOK {
		t.Fatalf("get batch log = %d", w.Code)
	}
	if w := do(a.livyProxy, admin, "DELETE", "", pv(ws.ID, lake.ID, "batches/0")); w.Code != http.StatusOK {
		t.Fatalf("delete batch = %d", w.Code)
	}

	// A missing file 400s; an unknown batch 404s.
	if w := do(a.livyProxy, admin, "POST", `{"file":"`+ws.ID+`/`+lake.ID+`/Files/nope.py"}`, pv(ws.ID, lake.ID, "batches")); w.Code != http.StatusBadRequest {
		t.Fatalf("missing file = %d", w.Code)
	}
	if w := do(a.livyProxy, viewer, "GET", "", pv(ws.ID, lake.ID, "batches/99")); w.Code != http.StatusNotFound {
		t.Fatalf("unknown batch = %d", w.Code)
	}
}

// TestLivyNativeBatchVariants: an abfss:// reference with name-based resolution,
// and a failing script → batch "dead" with the traceback as its log.
func TestLivyNativeBatchVariants(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st) // DisplayName "w"
	lake := seedLakehouse(t, st, ws.ID, "lh")
	seedFile(t, st, ws.ID, lake.ID, "Files/job.py", []byte("spark.range(3).count()\n"))

	// A stub agent that fails the script (exercises the dead/traceback path).
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"state":"idle"}`))
		case "/statements":
			_, _ = w.Write([]byte(`{"status":"error","ename":"Error","evalue":"boom","traceback":["Traceback","boom"]}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer agent.Close()
	if err := a.SetLivyAgent(agent.URL); err != nil {
		t.Fatal(err)
	}

	// abfss:// reference resolved by workspace + item *name*.
	abfss := `{"file":"abfss://w@onelake.dfs.fabric.microsoft.com/lh.Lakehouse/Files/job.py"}`
	w := do(a.livyProxy, admin, "POST", abfss, pv(ws.ID, lake.ID, "batches"))
	if w.Code != http.StatusCreated {
		t.Fatalf("abfss batch = %d %s", w.Code, w.Body.Bytes())
	}
	var b struct {
		State string
		Log   []string
	}
	_ = json.Unmarshal(w.Body.Bytes(), &b)
	if b.State != "dead" || len(b.Log) == 0 || b.Log[len(b.Log)-1] != "boom" {
		t.Fatalf("failed batch = %+v", b)
	}

	// An unparseable file reference 400s.
	if w := do(a.livyProxy, admin, "POST", `{"file":"garbage"}`, pv(ws.ID, lake.ID, "batches")); w.Code != http.StatusBadRequest {
		t.Fatalf("garbage file = %d", w.Code)
	}
	// Unknown workspace 400s.
	if w := do(a.livyProxy, admin, "POST", `{"file":"nope/lh.Lakehouse/Files/job.py"}`, pv(ws.ID, lake.ID, "batches")); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown ws = %d", w.Code)
	}
}
