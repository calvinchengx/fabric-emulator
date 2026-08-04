package api

// The Spark-agent side of a session opening: what the emulator TELLS the agent
// about a lakehouse. Both functions here are best-effort — they log and return
// rather than failing session creation — which is exactly why they went
// untested: nothing upstream observes them, so a silent regression stays
// silent. `registerLakehouseTables` sat at 32% and `bindDefaultLakehouse` at 0%.
//
// Neither needs a real Spark agent. `agentPost` resolves `a.livyAgent`, a URL,
// so an httptest server is a complete stand-in for the one thing under test:
// the request the emulator decides to send.

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/compute"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// agentStub is a Spark agent that records what it was asked to do and answers
// with reply. Requests are read under a mutex: the production callers run these
// from a session goroutine, and -race is the point of the exercise.
type agentStub struct {
	mu     sync.Mutex
	posts  []agentPostRecord
	reply  map[string]any
	status int
}

type agentPostRecord struct {
	path string
	body map[string]any
}

func newAgentStub(t *testing.T, a *API) *agentStub {
	t.Helper()
	s := &agentStub{reply: map[string]any{"registered": 0}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.posts = append(s.posts, agentPostRecord{path: r.URL.Path, body: body})
		reply, status := s.reply, s.status
		s.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
		}
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(srv.Close)
	if err := a.SetLivyAgent(srv.URL); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *agentStub) answer(reply map[string]any, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reply, s.status = reply, status
}

func (s *agentStub) recorded() []agentPostRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agentPostRecord(nil), s.posts...)
}

// only returns the single request the stub received, failing otherwise. "How
// many" is half of every assertion here — the empty-lakehouse case is defined
// by there being NO request.
func (s *agentStub) only(t *testing.T) agentPostRecord {
	t.Helper()
	got := s.recorded()
	if len(got) != 1 {
		t.Fatalf("agent received %d request(s); want exactly 1", len(got))
	}
	return got[0]
}

// captureLog redirects the standard logger for the duration of a test. These
// functions report failure ONLY to the log, so the log is the observable.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })
	return buf
}

// seedLakehouseWithTables creates a lakehouse holding one Delta file per named
// table, which is how a table becomes a first-level directory under Tables/.
func seedLakehouseWithTables(t *testing.T, st *store.Store, wid, name string, tables ...string) *store.Item {
	t.Helper()
	lake := &store.Item{WorkspaceID: wid, Type: "Lakehouse", DisplayName: name}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range tables {
		p := &store.OneLakePath{
			WorkspaceID: wid, ItemID: lake.ID,
			RelPath: "Tables/" + tbl + "/part-0.parquet",
			Content: []byte("PAR1"),
		}
		if err := st.CreateOneLakePath(p, false); err != nil {
			t.Fatal(err)
		}
	}
	return lake
}

// TestRegisterLakehouseTablesDeclaresEachDeltaTable is the read direction: the
// tables a session can resolve BY NAME. The location must be the
// account-prefixed abfs form a Fabric notebook would itself have written —
// registering a table at a path no client would name is the same as not
// registering it.
func TestRegisterLakehouseTablesDeclaresEachDeltaTable(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := seedLakehouseWithTables(t, st, ws.ID, "sales_lake", "silver_customers", "silver_orders")
	stub := newAgentStub(t, a)

	a.registerLakehouseTables("42", ws.ID, lake.ID)

	got := stub.only(t)
	if got.path != "/register" {
		t.Errorf("posted to %q; want /register", got.path)
	}
	if got.body["session"] != "42" {
		t.Errorf("session = %v; want 42", got.body["session"])
	}
	// The schema is the lakehouse's DISPLAY name, not its id: that is the name a
	// notebook writes in `SELECT * FROM sales_lake.silver_orders`.
	if got.body["schema"] != "sales_lake" {
		t.Errorf("schema = %v; want the lakehouse display name sales_lake", got.body["schema"])
	}
	tables, _ := got.body["tables"].([]any)
	if len(tables) != 2 {
		t.Fatalf("registered %d table(s); want 2", len(tables))
	}
	locs := map[string]string{}
	for _, e := range tables {
		m, _ := e.(map[string]any)
		name, _ := m["name"].(string)
		loc, _ := m["location"].(string)
		locs[name] = loc
	}
	want := "abfs://" + ws.ID + "@onelake.dfs.fabric.microsoft.com/" + lake.ID + "/Tables/silver_orders"
	if locs["silver_orders"] != want {
		t.Errorf("silver_orders location =\n  %q\nwant\n  %q", locs["silver_orders"], want)
	}
	if _, ok := locs["silver_customers"]; !ok {
		t.Errorf("silver_customers was not registered; got %v", locs)
	}
}

// TestRegisterLakehouseTablesSkipsLooseFiles: only directories are tables. A
// file sitting directly under Tables/ is not one, and registering it would
// declare a table whose location holds bytes no reader can parse.
func TestRegisterLakehouseTablesSkipsLooseFiles(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := seedLakehouseWithTables(t, st, ws.ID, "lake", "orders")
	loose := &store.OneLakePath{
		WorkspaceID: ws.ID, ItemID: lake.ID,
		RelPath: "Tables/README.md", Content: []byte("not a table"),
	}
	if err := st.CreateOneLakePath(loose, false); err != nil {
		t.Fatal(err)
	}
	stub := newAgentStub(t, a)

	a.registerLakehouseTables("1", ws.ID, lake.ID)

	tables, _ := stub.only(t).body["tables"].([]any)
	if len(tables) != 1 {
		t.Fatalf("registered %d entr(ies); want only the orders directory, got %v", len(tables), tables)
	}
	m, _ := tables[0].(map[string]any)
	if m["name"] != "orders" {
		t.Errorf("registered %v; want orders", m["name"])
	}
}

// TestRegisterLakehouseTablesSaysNothingAboutAnEmptyLakehouse pins the
// documented normal case. At session-open time a lakehouse usually has no
// Tables/ yet, and the function must not chatter at the agent about nothing —
// the early return is the behaviour, not an optimisation.
func TestRegisterLakehouseTablesSaysNothingAboutAnEmptyLakehouse(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := seedLakehouseWithTables(t, st, ws.ID, "empty")
	stub := newAgentStub(t, a)

	a.registerLakehouseTables("1", ws.ID, lake.ID)

	if got := stub.recorded(); len(got) != 0 {
		t.Fatalf("an empty lakehouse produced %d agent request(s); want none: %v", len(got), got)
	}
}

// TestRegisterLakehouseTablesIgnoresAnItemThatIsNotThere: a session naming a
// lakehouse that does not exist registers nothing rather than posting a schema
// with an empty name.
func TestRegisterLakehouseTablesIgnoresAnItemThatIsNotThere(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	stub := newAgentStub(t, a)

	a.registerLakehouseTables("1", ws.ID, "00000000-0000-0000-0000-000000000000")

	if got := stub.recorded(); len(got) != 0 {
		t.Fatalf("an unknown lakehouse produced %d agent request(s); want none", len(got))
	}
}

// TestRegisterLakehouseTablesReportsAPartialFailure is the regression test for
// the bug the source comment describes: the agent answers 200 with
// {"registered": N, "skipped": [...]}, and code that checked only for an
// "error" key treated that as success. The table then surfaced as "table not
// found" much later with nothing pointing back here.
func TestRegisterLakehouseTablesReportsAPartialFailure(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := seedLakehouseWithTables(t, st, ws.ID, "lake", "good", "bad")
	stub := newAgentStub(t, a)
	stub.answer(map[string]any{"registered": 1, "skipped": []any{"bad"}}, 0)
	logs := captureLog(t)

	a.registerLakehouseTables("1", ws.ID, lake.ID)

	out := logs.String()
	if !strings.Contains(out, "did not register") || !strings.Contains(out, "bad") {
		t.Fatalf("a partial registration was not reported; log was:\n%s", out)
	}
	// The count matters: "1 of 2" is what tells a reader the rest DID land.
	if !strings.Contains(out, "1 of 2") {
		t.Errorf("log does not say how many of how many; want \"1 of 2\":\n%s", out)
	}
}

// TestRegisterLakehouseTablesReportsAnAgentError covers the total failure the
// partial case is distinguished from.
func TestRegisterLakehouseTablesReportsAnAgentError(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := seedLakehouseWithTables(t, st, ws.ID, "lake", "orders")
	stub := newAgentStub(t, a)
	stub.answer(map[string]any{"error": "no such session"}, 0)
	logs := captureLog(t)

	a.registerLakehouseTables("1", ws.ID, lake.ID)

	if out := logs.String(); !strings.Contains(out, "could not register") ||
		!strings.Contains(out, "no such session") {
		t.Fatalf("the agent's error was not reported; log was:\n%s", out)
	}
}

// TestRegisterLakehouseTablesSurvivesAnUnreachableAgent: session creation must
// not fail because the agent is down. The requirement is that it is LOGGED —
// an unregistered table is otherwise a "table not found" with no cause.
func TestRegisterLakehouseTablesSurvivesAnUnreachableAgent(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := seedLakehouseWithTables(t, st, ws.ID, "lake", "orders")
	stub := newAgentStub(t, a)
	stub.answer(map[string]any{"error": "boom"}, http.StatusInternalServerError)
	logs := captureLog(t)

	a.registerLakehouseTables("1", ws.ID, lake.ID)

	if out := logs.String(); !strings.Contains(out, "registering 1 table(s)") {
		t.Fatalf("a failing agent was not reported; log was:\n%s", out)
	}
}

// TestBindDefaultLakehousePointsTheCatalogAtOneLake is the write direction the
// source comment calls out: without this, `df.write.saveAsTable("events")`
// succeeds, reads back, goes green — and the bytes land in the engine's own
// warehouse directory rather than in the lakehouse. The assertion is therefore
// on the LOCATION, which is the part that makes the write land somewhere a user
// can find.
func TestBindDefaultLakehousePointsTheCatalogAtOneLake(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	stub := newAgentStub(t, a)

	b := compute.Binding{WorkspaceID: ws.ID, LakehouseID: "lake-id", LakehouseName: "sales_lake"}
	a.bindDefaultLakehouse("7", b)

	got := stub.only(t)
	if got.path != "/statements" {
		t.Errorf("posted to %q; want /statements", got.path)
	}
	if got.body["session"] != "7" {
		t.Errorf("session = %v; want 7", got.body["session"])
	}
	code, _ := got.body["code"].(string)
	wantLoc := "abfs://" + ws.ID + "@onelake.dfs.fabric.microsoft.com/lake-id/Tables"
	if !strings.Contains(code, "LOCATION '"+wantLoc+"'") {
		t.Errorf("code does not bind the database to %q:\n%s", wantLoc, code)
	}
	if !strings.Contains(code, "CREATE DATABASE IF NOT EXISTS `sales_lake`") {
		t.Errorf("code does not create the sales_lake database:\n%s", code)
	}
	// setCurrentDatabase rather than `USE`: Sail rejects the latter outright,
	// which is why the source spells it the Python way.
	if !strings.Contains(code, `setCurrentDatabase("sales_lake")`) {
		t.Errorf("code does not make sales_lake current:\n%s", code)
	}
	if strings.Contains(code, "USE ") {
		t.Errorf("code uses `USE`, which Sail rejects:\n%s", code)
	}
	// The failure must be swallowed inside the notebook, not raised: a notebook
	// addressing tables by full abfs path needs none of this.
	if !strings.Contains(code, "except Exception") {
		t.Errorf("binding failure is not tolerated inside the statement:\n%s", code)
	}
}

// TestBindDefaultLakehouseNamesAnUnnamedLakehouse: the name reaches Spark as an
// identifier, so an empty one would emit `CREATE DATABASE IF NOT EXISTS ``` and
// fail to parse. The fallback keeps a nameless binding working.
func TestBindDefaultLakehouseNamesAnUnnamedLakehouse(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	stub := newAgentStub(t, a)

	a.bindDefaultLakehouse("1", compute.Binding{WorkspaceID: ws.ID, LakehouseID: "lake-id"})

	code, _ := stub.only(t).body["code"].(string)
	if !strings.Contains(code, "CREATE DATABASE IF NOT EXISTS `lakehouse`") {
		t.Errorf("an unnamed lakehouse did not fall back to `lakehouse`:\n%s", code)
	}
	if !strings.Contains(code, `setCurrentDatabase("lakehouse")`) {
		t.Errorf("the fallback name is not made current:\n%s", code)
	}
}

// TestBindDefaultLakehouseSurvivesAFailingAgent: binding is best-effort, so a
// refusing agent is logged and the notebook run continues.
func TestBindDefaultLakehouseSurvivesAFailingAgent(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	stub := newAgentStub(t, a)
	stub.answer(map[string]any{"error": "boom"}, http.StatusInternalServerError)
	logs := captureLog(t)

	a.bindDefaultLakehouse("1", compute.Binding{
		WorkspaceID: ws.ID, LakehouseID: "lake-id", LakehouseName: "lake"})

	if out := logs.String(); !strings.Contains(out, "binding default lakehouse lake") {
		t.Fatalf("a failing agent was not reported; log was:\n%s", out)
	}
}

// TestBindDefaultLakehouseWithNoAgentConfigured: the no-agent stack (Sail
// absent) must not panic here. agentPost returns an error for a nil agent
// precisely so this path stays recoverable.
func TestBindDefaultLakehouseWithNoAgentConfigured(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	logs := captureLog(t)

	a.bindDefaultLakehouse("1", compute.Binding{
		WorkspaceID: ws.ID, LakehouseID: "lake-id", LakehouseName: "lake"})

	if out := logs.String(); !strings.Contains(out, "no Spark agent configured") {
		t.Fatalf("a missing agent was not reported; log was:\n%s", out)
	}
}
