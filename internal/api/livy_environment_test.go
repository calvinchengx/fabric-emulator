package api

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// The wire docs/37 §1 names: the emulator parsed an Environment item and nothing
// read the answer, so a run REPORTED an environment while the session never
// RECEIVED one. These assert the session now receives it — and, just as
// important, that a session binding NO environment sends nothing.

func seedEnvironment(t *testing.T, st *store.Store, wid, name, requirements string) *store.Item {
	t.Helper()
	it := &store.Item{WorkspaceID: wid, DisplayName: name, Type: "Environment"}
	parts := []store.DefinitionPart{{
		Path:        "Libraries/requirements.txt",
		Payload:     base64.StdEncoding.EncodeToString([]byte(requirements)),
		PayloadType: "InlineBase64",
	}}
	if err := st.CreateItem(it, parts); err != nil {
		t.Fatal(err)
	}
	return it
}

func TestEnvironmentReachesTheAgentOnBind(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	env := seedEnvironment(t, st, ws.ID, "team-env", "great-expectations==0.18.8\nanytree\n")
	stub := newAgentStub(t, a)
	stub.answer(map[string]any{"applied": true}, 0)

	a.applyEnvironment("sess-1", ws.ID, env.ID)

	rec := stub.only(t)
	if rec.path != "/environment" {
		t.Fatalf("posted to %q, want /environment", rec.path)
	}
	if rec.body["environment"] != env.ID || rec.body["session"] != "sess-1" {
		t.Fatalf("body = %+v", rec.body)
	}
	// The packages parsed out of requirements.txt are what the session receives —
	// the whole point, since this list previously had no consumer at all.
	pkgs, _ := rec.body["packages"].([]any)
	var got []string
	for _, p := range pkgs {
		got = append(got, p.(string))
	}
	// SORTED, not file order: ParseEnvironment sorts so the list is stable
	// whatever order the definition parts arrive in.
	if strings.Join(got, ",") != "anytree,great-expectations==0.18.8" {
		t.Fatalf("packages = %v", got)
	}
}

func TestNoEnvironmentMeansNoCall(t *testing.T) {
	// A session that binds nothing must not provoke a request. The runtime image
	// as-is is the correct outcome, and an empty POST would make the agent's
	// conflict bookkeeping think an environment was claimed.
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	stub := newAgentStub(t, a)

	a.applyEnvironment("sess-1", ws.ID, "")

	if n := len(stub.recorded()); n != 0 {
		t.Fatalf("%d agent calls for a session with no environment", n)
	}
}

func TestAnEmptyEnvironmentSendsNothing(t *testing.T) {
	// A real Environment item that declares no packages, config or jars has
	// nothing to apply. Posting anyway would claim the agent's single
	// environment slot and make the NEXT session's bind a false conflict.
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	env := seedEnvironment(t, st, ws.ID, "empty-env", "\n\n")
	stub := newAgentStub(t, a)

	a.applyEnvironment("sess-1", ws.ID, env.ID)

	if n := len(stub.recorded()); n != 0 {
		t.Fatalf("%d agent calls for an environment declaring nothing", n)
	}
}

func TestAnUnreadableEnvironmentDoesNotBlockTheBind(t *testing.T) {
	// Same contract as the lakehouse mount: best-effort, logged, never fatal. A
	// session that cannot get its packages still starts — the failure resurfaces
	// as a ModuleNotFoundError naming the package, which is a better place to
	// meet it than a dead session.
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	stub := newAgentStub(t, a)

	a.applyEnvironment("sess-1", ws.ID, "00000000-0000-0000-0000-000000000000")

	if n := len(stub.recorded()); n != 0 {
		t.Fatalf("an unreadable environment must not reach the agent: %d calls", n)
	}
}

func TestAcquireCarriesTheEnvironmentOntoTheSession(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := seedLakehouse(t, st, ws.ID, "lake")
	env := seedEnvironment(t, st, ws.ID, "team-env", "anytree\n")

	body, _ := json.Marshal(map[string]any{
		"sessionTag": "t", "environmentId": env.ID})
	w := do(a.acquireHC, admin, "POST", string(body),
		map[string]string{"wid": ws.ID, "lid": lake.ID})
	if w.Code != 200 {
		t.Fatalf("acquire = %d %s", w.Code, w.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	hcid, _ := out["id"].(string)

	repl, ok := a.hcMgr().get(hcid)
	if !ok {
		t.Fatal("session not found after acquire")
	}
	if repl.environmentID != env.ID {
		t.Fatalf("session carries environment %q, want %q", repl.environmentID, env.ID)
	}
}
