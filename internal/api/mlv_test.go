package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// commitDelta fakes one Delta commit for a lakehouse table, which is what the
// staleness comparison reads. Versions are what move, so the tests move them.
func commitDelta(t *testing.T, st *store.Store, wsID, lhID, table string, version int) {
	t.Helper()
	seedFile(t, st, wsID, lhID,
		fmt.Sprintf("Tables/%s/_delta_log/%020d.json", table, version),
		[]byte(`{"commitInfo":{}}`))
}

// mlvAgentThatWrites stands in for the engine: it answers the statement AND
// commits the table, because a real engine's write lands in OneLake through the
// storage layer. A fake that only answered would let the refresh claim a
// materialisation that never happened — which is the case asserted separately
// below.
func mlvAgentThatWrites(t *testing.T, a *API, st *store.Store, wsID, lhID, table string, version int) *fakeAgent {
	t.Helper()
	agent := newFakeAgent(t, a)
	agent.reply = func(code string) map[string]any {
		if strings.Contains(code, "spark.sql(") {
			commitDelta(t, st, wsID, lhID, table, version)
		}
		return map[string]any{"status": "ok", "data": map[string]any{"text/plain": `{"rowCount":3}`}}
	}
	return agent
}

func mlvDo(t *testing.T, a *API, h handler, method, body string, vals map[string]string) (int, map[string]any) {
	t.Helper()
	w := do(h, admin, method, body, vals)
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// TestMLVRefreshReallyMaterialises is the load-bearing one: the view's own
// query reaches the engine, addressed at the lakehouse's OneLake table path,
// and the refresh is only reported successful because a Delta table actually
// landed. Asserting the status alone would pass on an implementation that ran
// nothing.
func TestMLVRefreshReallyMaterialises(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	commitDelta(t, st, ws.ID, lh.ID, "orders", 0)
	agent := mlvAgentThatWrites(t, a, st, ws.ID, lh.ID, "daily_totals", 0)

	vals := map[string]string{"wid": ws.ID, "iid": lh.ID}
	code, out := mlvDo(t, a, a.createMLV, "POST",
		`{"name":"daily_totals","query":"SELECT day, sum(amount) FROM orders GROUP BY day",
		  "dependsOn":["orders"]}`, vals)
	if code != http.StatusCreated {
		t.Fatalf("create = %d %+v", code, out)
	}
	// A definition is not a table. Saying otherwise would be the whole bug.
	if out["state"] != "NeverRefreshed" || out["isStale"] != false {
		t.Fatalf("a view that has never refreshed is not materialised and not stale: %+v", out)
	}

	vals["name"] = "daily_totals"
	code, out = mlvDo(t, a, a.refreshMLV, "POST", "", vals)
	if code != http.StatusOK {
		t.Fatalf("refresh = %d %+v", code, out)
	}
	if out["state"] != "Materialized" || out["isStale"] != false {
		t.Fatalf("after a refresh against unchanged sources: %+v", out)
	}

	sent := strings.Join(agent.statements(), "\n")
	if !strings.Contains(sent, "sum(amount)") {
		t.Fatalf("the view's own query never reached the engine: %s", sent)
	}
	want := fmt.Sprintf("abfs://%s@onelake.dfs.fabric.microsoft.com/%s/Tables/daily_totals",
		ws.ID, lh.ID)
	if !strings.Contains(sent, want) {
		t.Fatalf("the write was not addressed at the lakehouse's table path (%s): %s", want, sent)
	}
	if !strings.Contains(sent, `format("delta")`) {
		t.Fatalf("the result was not written as Delta: %s", sent)
	}
	// The version read at refresh time is what staleness is measured against.
	versions, _ := out["sourceVersions"].(map[string]any)
	if v, ok := versions["orders"]; !ok || fmt.Sprint(v) != "0" {
		t.Fatalf("the source version was not recorded: %+v", out)
	}
}

// TestMLVGoesStaleWhenASourceMoves: the point of tracking versions at all. A
// view whose source has advanced is stale, and the answer names WHICH source —
// a caller with several dependencies otherwise has to diff versions by hand.
func TestMLVGoesStaleWhenASourceMoves(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	commitDelta(t, st, ws.ID, lh.ID, "orders", 0)
	commitDelta(t, st, ws.ID, lh.ID, "customers", 0)
	mlvAgentThatWrites(t, a, st, ws.ID, lh.ID, "joined", 0)

	vals := map[string]string{"wid": ws.ID, "iid": lh.ID}
	if code, out := mlvDo(t, a, a.createMLV, "POST",
		`{"name":"joined","query":"SELECT * FROM orders JOIN customers USING (id)",
		  "dependsOn":["orders","customers"]}`, vals); code != http.StatusCreated {
		t.Fatalf("create = %d %+v", code, out)
	}
	vals["name"] = "joined"
	if code, out := mlvDo(t, a, a.refreshMLV, "POST", "", vals); code != http.StatusOK ||
		out["isStale"] != false {
		t.Fatalf("refresh = %d %+v", code, out)
	}

	// One source advances; the other does not.
	commitDelta(t, st, ws.ID, lh.ID, "orders", 1)

	_, out := mlvDo(t, a, a.getMLV, "GET", "", vals)
	if out["isStale"] != true {
		t.Fatalf("a view whose source advanced is stale: %+v", out)
	}
	because, _ := out["staleBecause"].([]any)
	if len(because) != 1 || because[0] != "orders" {
		t.Fatalf("staleBecause = %+v, want exactly [orders] — the table that moved", because)
	}
}

// TestMLVRefreshThatWritesNothingFails is the false-green guard, and the reason
// the refresh checks OneLake rather than trusting the engine's exit code. A
// statement can succeed while writing no table — a typo'd path, a silently
// swallowed error — and reporting that as Materialized would leave a view whose
// rows do not exist.
func TestMLVRefreshThatWritesNothingFails(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	agent := newFakeAgent(t, a)
	agent.reply = func(string) map[string]any {
		// Cheerful, and wrote nothing.
		return map[string]any{"status": "ok", "data": map[string]any{"text/plain": `{"rowCount":9}`}}
	}

	vals := map[string]string{"wid": ws.ID, "iid": lh.ID}
	mlvDo(t, a, a.createMLV, "POST", `{"name":"ghost","query":"SELECT 1"}`, vals)
	vals["name"] = "ghost"
	code, out := mlvDo(t, a, a.refreshMLV, "POST", "", vals)
	if code == http.StatusOK {
		t.Fatalf("a refresh that committed no table was reported successful: %+v", out)
	}
	if msg := fmt.Sprint(out); !strings.Contains(msg, "no Delta table") {
		t.Fatalf("the error does not say what was missing: %s", msg)
	}
	// And the recorded state must say so too, not stay at NeverRefreshed.
	_, got := mlvDo(t, a, a.getMLV, "GET", "", vals)
	if got["state"] != "RefreshFailed" || got["isStale"] != true {
		t.Fatalf("a failed refresh is recorded as failed: %+v", got)
	}
}

// TestMLVEngineFailureIsRecorded: the engine's own error is the refresh's
// error, and it survives into the view's state for a later reader.
func TestMLVEngineFailureIsRecorded(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	agent := newFakeAgent(t, a)
	agent.reply = func(string) map[string]any {
		return map[string]any{"status": "error", "ename": "AnalysisException",
			"evalue": "Table or view not found: orders"}
	}
	vals := map[string]string{"wid": ws.ID, "iid": lh.ID}
	mlvDo(t, a, a.createMLV, "POST", `{"name":"v","query":"SELECT * FROM orders"}`, vals)
	vals["name"] = "v"
	if code, _ := mlvDo(t, a, a.refreshMLV, "POST", "", vals); code == http.StatusOK {
		t.Fatal("an engine error was reported as a successful refresh")
	}
	_, got := mlvDo(t, a, a.getMLV, "GET", "", vals)
	if e, _ := got["lastError"].(string); !strings.Contains(e, "Table or view not found") {
		t.Fatalf("the engine's message did not survive into the view's state: %+v", got)
	}
}

// TestMLVNoEngineIsHonest: with nothing to run the query, say so — never a
// materialisation that did not happen.
func TestMLVNoEngineIsHonest(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	vals := map[string]string{"wid": ws.ID, "iid": lh.ID}
	mlvDo(t, a, a.createMLV, "POST", `{"name":"v","query":"SELECT 1"}`, vals)
	vals["name"] = "v"
	code, out := mlvDo(t, a, a.refreshMLV, "POST", "", vals)
	if code == http.StatusOK {
		t.Fatalf("refresh succeeded with no engine: %+v", out)
	}
	if msg := fmt.Sprint(out); !strings.Contains(msg, "no Spark agent is configured") {
		t.Fatalf("the error does not name the missing engine: %s", msg)
	}
}

// TestMLVDefinitionSurface: the shapes a caller can get wrong, and the
// lifecycle. The name becomes a path segment under Tables/, so it is validated
// rather than allowed to produce a table nobody can address.
func TestMLVDefinitionSurface(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	vals := map[string]string{"wid": ws.ID, "iid": lh.ID}

	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"no name", `{"query":"SELECT 1"}`, http.StatusBadRequest},
		{"name with a slash", `{"name":"a/b","query":"SELECT 1"}`, http.StatusBadRequest},
		{"name with a space", `{"name":"a b","query":"SELECT 1"}`, http.StatusBadRequest},
		{"no query", `{"name":"v"}`, http.StatusBadRequest},
		{"not JSON", `nope`, http.StatusBadRequest},
		{"valid", `{"name":"v","query":"SELECT 1"}`, http.StatusCreated},
		{"duplicate name", `{"name":"v","query":"SELECT 2"}`, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, out := mlvDo(t, a, a.createMLV, "POST", tc.body, vals); code != tc.want {
				t.Fatalf("create = %d, want %d: %+v", code, tc.want, out)
			}
		})
	}

	if code, out := mlvDo(t, a, a.listMLV, "GET", "", vals); code != http.StatusOK ||
		len(out["value"].([]any)) != 1 {
		t.Fatalf("list = %d %+v", code, out)
	}
	del := map[string]string{"wid": ws.ID, "iid": lh.ID, "name": "v"}
	if code, _ := mlvDo(t, a, a.deleteMLV, "DELETE", "", del); code != http.StatusOK {
		t.Fatalf("delete = %d", code)
	}
	if code, _ := mlvDo(t, a, a.getMLV, "GET", "", del); code != http.StatusNotFound {
		t.Fatalf("get after delete = %d, want 404", code)
	}
	if code, _ := mlvDo(t, a, a.deleteMLV, "DELETE", "", del); code != http.StatusNotFound {
		t.Fatalf("delete twice = %d, want 404", code)
	}
}

// TestMLVDeleteLeavesTheData: dropping a definition is not a licence to delete
// rows a reader may still be using. Fabric's own drop semantics are uncaptured,
// so the emulator takes the conservative side and says which side it took.
func TestMLVDeleteLeavesTheData(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	mlvAgentThatWrites(t, a, st, ws.ID, lh.ID, "v", 0)
	vals := map[string]string{"wid": ws.ID, "iid": lh.ID}
	mlvDo(t, a, a.createMLV, "POST", `{"name":"v","query":"SELECT 1"}`, vals)
	vals["name"] = "v"
	if code, _ := mlvDo(t, a, a.refreshMLV, "POST", "", vals); code != http.StatusOK {
		t.Fatal("refresh failed")
	}
	if code, _ := mlvDo(t, a, a.deleteMLV, "DELETE", "", vals); code != http.StatusOK {
		t.Fatal("delete failed")
	}
	if _, ok := st.DeltaTableVersion(lh.ID, "v"); !ok {
		t.Fatal("deleting the definition removed the materialised table")
	}
}

// TestMLVWrongItemTypeIsRefused: the surface hangs off a lakehouse, and a
// warehouse id must not quietly create a view somewhere it cannot materialise.
func TestMLVWrongItemTypeIsRefused(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	wh := &store.Item{WorkspaceID: ws.ID, Type: "Warehouse", DisplayName: "wh"}
	if err := st.CreateItem(wh, nil); err != nil {
		t.Fatal(err)
	}
	vals := map[string]string{"wid": ws.ID, "iid": wh.ID}
	if code, _ := mlvDo(t, a, a.createMLV, "POST", `{"name":"v","query":"SELECT 1"}`, vals); code != http.StatusNotFound {
		t.Fatalf("create against a warehouse = %d, want 404", code)
	}
}

// TestMLVViewerCannotDefineOrRefresh: reading a definition is a viewer's
// business; creating one and running compute is not.
func TestMLVViewerCannotDefineOrRefresh(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	vals := map[string]string{"wid": ws.ID, "iid": lh.ID}
	mlvDo(t, a, a.createMLV, "POST", `{"name":"v","query":"SELECT 1"}`, vals)

	if w := do(a.createMLV, viewer, "POST", `{"name":"x","query":"SELECT 1"}`, vals); w.Code != http.StatusForbidden {
		t.Fatalf("viewer create = %d, want 403", w.Code)
	}
	vals["name"] = "v"
	if w := do(a.refreshMLV, viewer, "POST", "", vals); w.Code != http.StatusForbidden {
		t.Fatalf("viewer refresh = %d, want 403", w.Code)
	}
	if w := do(a.getMLV, viewer, "GET", "", vals); w.Code != http.StatusOK {
		t.Fatalf("viewer get = %d, want 200", w.Code)
	}
}

// TestMLVSourceDisappearingIsStale: a dependency that is deleted outright is
// not "unchanged". The materialised rows no longer correspond to anything
// readable, and reporting fresh there would be the same lie as missing a new
// commit — quieter, because nothing moved forward to notice.
func TestMLVSourceDisappearingIsStale(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	commitDelta(t, st, ws.ID, lh.ID, "orders", 0)
	mlvAgentThatWrites(t, a, st, ws.ID, lh.ID, "v", 0)

	vals := map[string]string{"wid": ws.ID, "iid": lh.ID}
	mlvDo(t, a, a.createMLV, "POST",
		`{"name":"v","query":"SELECT * FROM orders","dependsOn":["orders"]}`, vals)
	vals["name"] = "v"
	if code, out := mlvDo(t, a, a.refreshMLV, "POST", "", vals); code != http.StatusOK ||
		out["isStale"] != false {
		t.Fatalf("refresh = %d %+v", code, out)
	}

	if err := st.DeleteOneLakePath(lh.ID, "Tables/orders"); err != nil {
		t.Fatal(err)
	}
	_, out := mlvDo(t, a, a.getMLV, "GET", "", vals)
	if out["isStale"] != true {
		t.Fatalf("a view whose source was deleted is stale: %+v", out)
	}
	if because, _ := out["staleBecause"].([]any); len(because) != 1 || because[0] != "orders" {
		t.Fatalf("staleBecause = %+v, want [orders]", because)
	}
}

// TestMLVRefreshUnknownView: refreshing something that was never defined is a
// 404, not a materialisation of an empty definition.
func TestMLVRefreshUnknownView(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	newFakeAgent(t, a)
	vals := map[string]string{"wid": ws.ID, "iid": lh.ID, "name": "nope"}
	if code, _ := mlvDo(t, a, a.refreshMLV, "POST", "", vals); code != http.StatusNotFound {
		t.Fatalf("refresh of an undefined view = %d, want 404", code)
	}
}

// TestMLVUnknownLakehouse: every verb hangs off a lakehouse that must exist.
func TestMLVUnknownLakehouse(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	vals := map[string]string{"wid": ws.ID, "iid": "11111111-1111-1111-1111-111111111111", "name": "v"}
	for name, h := range map[string]handler{
		"create": a.createMLV, "list": a.listMLV, "get": a.getMLV,
		"delete": a.deleteMLV, "refresh": a.refreshMLV,
	} {
		if w := do(h, admin, "POST", `{"name":"v","query":"SELECT 1"}`, vals); w.Code != http.StatusNotFound {
			t.Errorf("%s against a missing lakehouse = %d, want 404", name, w.Code)
		}
	}
}

// TestMLVVersionsAreReadBeforeTheQuery forces the interleaving rather than
// hoping for it — the same technique the WebHook deadline needed. The agent
// fake commits a NEW version of the source WHILE the refresh's statement is
// executing, which is the window a real concurrent writer occupies.
//
// Read before the query, the recorded version is the one the view actually
// read, so the view is correctly reported stale afterwards. Read after, the
// refresh records a version that includes a write it never saw, and the view
// claims to reflect data that is not in it — a false fresh, which is worse
// than a false stale because nothing later corrects it.
func TestMLVVersionsAreReadBeforeTheQuery(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	commitDelta(t, st, ws.ID, lh.ID, "orders", 0)

	agent := newFakeAgent(t, a)
	agent.reply = func(code string) map[string]any {
		if strings.Contains(code, "spark.sql(") {
			// The view's own table lands...
			commitDelta(t, st, ws.ID, lh.ID, "v", 0)
			// ...and a concurrent writer advances the SOURCE mid-refresh.
			commitDelta(t, st, ws.ID, lh.ID, "orders", 1)
		}
		return map[string]any{"status": "ok", "data": map[string]any{"text/plain": `{"rowCount":1}`}}
	}

	vals := map[string]string{"wid": ws.ID, "iid": lh.ID}
	mlvDo(t, a, a.createMLV, "POST",
		`{"name":"v","query":"SELECT * FROM orders","dependsOn":["orders"]}`, vals)
	vals["name"] = "v"
	code, out := mlvDo(t, a, a.refreshMLV, "POST", "", vals)
	if code != http.StatusOK {
		t.Fatalf("refresh = %d %+v", code, out)
	}
	versions, _ := out["sourceVersions"].(map[string]any)
	if v := fmt.Sprint(versions["orders"]); v != "0" {
		t.Fatalf("recorded orders at version %s — that write landed DURING the refresh and "+
			"is not in the view, so recording it claims data the query never read", v)
	}
	if out["isStale"] != true {
		t.Fatalf("a source that moved during the refresh leaves the view stale: %+v", out)
	}
}

// TestMLVRoutesAreMounted goes through the REAL MUX, and it exists because of a
// bug another session hit today: `getDefinition` was tested — missing-item 404,
// viewer 403, both green — while its route was never registered, so the URL
// Microsoft's own article prints answered 404 for every typed collection. This
// package's `do()` helper calls a handler directly against a fabricated request
// to `/x`, so all 404 of its handler-test call sites prove the handler and say
// nothing about whether a URL reaches it.
//
// Every other case in this file is one of those. This is the one that fails if
// `registerMLV` is never called from `Register`.
func TestMLVRoutesAreMounted(t *testing.T) {
	mux, st, token := newRegisteredAPI(t)
	ws := &store.Workspace{DisplayName: "w"}
	if err := st.CreateWorkspace(ws, store.Principal{ID: "route-admin", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	lh := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lake"}
	if err := st.CreateItem(lh, nil); err != nil {
		t.Fatal(err)
	}

	base := fmt.Sprintf("/v1/workspaces/%s/lakehouses/%s/materializedlakeviews", ws.ID, lh.ID)

	// Each verb must reach ITS handler, not merely be non-404.
	if w := serve(mux, "POST", base, token, `{"name":"routed","query":"SELECT 1"}`); w.Code != http.StatusCreated {
		t.Fatalf("POST %s = %d %s — is registerMLV wired into Register?", base, w.Code, w.Body.Bytes())
	}
	if w := serve(mux, "GET", base, token, ""); w.Code != http.StatusOK {
		t.Fatalf("GET (list) = %d %s", w.Code, w.Body.Bytes())
	}
	if w := serve(mux, "GET", base+"/routed", token, ""); w.Code != http.StatusOK {
		t.Fatalf("GET (one) = %d %s", w.Code, w.Body.Bytes())
	}
	// The refresh route must RESOLVE even though the refresh cannot succeed
	// with no engine attached: 502 is the handler answering, 404 is the route
	// missing. Distinguishing those is the whole point.
	if w := serve(mux, "POST", base+"/routed/refresh", token, ""); w.Code == http.StatusNotFound {
		t.Fatal("POST .../refresh = 404 — the route is not mounted")
	}
	if w := serve(mux, "DELETE", base+"/routed", token, ""); w.Code != http.StatusOK {
		t.Fatalf("DELETE = %d %s", w.Code, w.Body.Bytes())
	}
}
