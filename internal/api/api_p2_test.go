package api

// P2 coverage tests: the Register/withAuth wiring driven end-to-end with a
// real validator and minted tokens, the typed collection aliases, job body
// shapes, and the git initializeConnection/disconnect paths.

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/clock"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

const testIssuer = "https://login.test/tenant-1/v2.0"

// mintToken signs a minimal RS256 control-plane token for oid.
func mintToken(t *testing.T, key *rsa.PrivateKey, oid string) string {
	t.Helper()
	b64 := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	head := map[string]string{"alg": "RS256", "typ": "JWT", "kid": "test-key"}
	claims := map[string]any{"iss": testIssuer, "aud": auth.ControlPlaneAudiences[0], "exp": int64(2000), "oid": oid}
	signing := b64(head) + "." + b64(claims)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// newRegisteredAPI mounts the full /v1 surface behind a real validator and
// returns the mux plus a valid bearer token for principal "route-admin".
func newRegisteredAPI(t *testing.T) (*http.ServeMux, *store.Store, string) {
	t.Helper()
	st, err := store.Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "kid": "test-key",
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jwks)
	}))
	t.Cleanup(srv.Close)
	v := auth.New(testIssuer, srv.URL, false, func() int64 { return 1000 }, srv.Client())

	a := New(st, v, 1, 0)
	mux := http.NewServeMux()
	a.Register(mux)
	return mux, st, mintToken(t, key, "route-admin")
}

// serve runs one request through the registered mux.
func serve(mux *http.ServeMux, method, target, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestRegisterWithAuthAndTypedRoutes(t *testing.T) {
	mux, st, token := newRegisteredAPI(t)

	// No bearer → 401 with the issuer advertised.
	w := serve(mux, "GET", "/v1/workspaces", "", "")
	if w.Code != http.StatusUnauthorized || errorCode(t, w) != "TokenInvalid" {
		t.Fatalf("no token = %d %s", w.Code, w.Body.Bytes())
	}
	if !strings.Contains(w.Header().Get("WWW-Authenticate"), testIssuer) {
		t.Fatalf("WWW-Authenticate = %q; want issuer", w.Header().Get("WWW-Authenticate"))
	}
	// Garbage bearer → 401.
	if w := serve(mux, "GET", "/v1/workspaces", "garbage", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("garbage token = %d", w.Code)
	}

	// Valid token flows through withAuth to the handler.
	w = serve(mux, "POST", "/v1/workspaces", token, `{"displayName":"w"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create workspace = %d %s", w.Code, w.Body.Bytes())
	}
	var ws struct{ ID string }
	_ = json.Unmarshal(w.Body.Bytes(), &ws)

	// typedCreate forces the collection's type onto the generic create.
	w = serve(mux, "POST", "/v1/workspaces/"+ws.ID+"/notebooks", token, `{"displayName":"nb"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("typed create = %d %s", w.Code, w.Body.Bytes())
	}
	var nb struct{ ID, Type string }
	_ = json.Unmarshal(w.Body.Bytes(), &nb)
	if nb.Type != "Notebook" {
		t.Fatalf("typed create type = %q; want Notebook", nb.Type)
	}

	// typedList scopes to the collection's type.
	lh := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lh"}
	if err := st.CreateItem(lh, nil); err != nil {
		t.Fatal(err)
	}
	w = serve(mux, "GET", "/v1/workspaces/"+ws.ID+"/notebooks", token, "")
	var list struct{ Value []struct{ ID string } }
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if w.Code != http.StatusOK || len(list.Value) != 1 || list.Value[0].ID != nb.ID {
		t.Fatalf("typed list = %d %s", w.Code, w.Body.Bytes())
	}

	// typedGet returns the member, but 404s items of another type and
	// unknown ids (a lakehouse is not addressable under /notebooks).
	if w := serve(mux, "GET", "/v1/workspaces/"+ws.ID+"/notebooks/"+nb.ID, token, ""); w.Code != http.StatusOK {
		t.Fatalf("typed get = %d", w.Code)
	}
	if w := serve(mux, "GET", "/v1/workspaces/"+ws.ID+"/notebooks/"+lh.ID, token, ""); w.Code != http.StatusNotFound {
		t.Fatalf("typed get cross-type = %d", w.Code)
	}
	if w := serve(mux, "GET", "/v1/workspaces/"+ws.ID+"/notebooks/missing", token, ""); w.Code != http.StatusNotFound {
		t.Fatalf("typed get missing = %d", w.Code)
	}
	// Malformed typed create body.
	if w := serve(mux, "POST", "/v1/workspaces/"+ws.ID+"/lakehouses", token, `{`); w.Code != http.StatusBadRequest {
		t.Fatalf("typed create malformed = %d", w.Code)
	}
}

func TestJobLifecycleAndBody(t *testing.T) {
	a, st := newAPI(t)
	st.Clock.Freeze()
	ws := seedWorkspace(t, st)
	it := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "nb"}
	if err := st.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	pv := map[string]string{"wid": ws.ID, "iid": it.ID}

	// Missing jobType → 400.
	if w := doQuery(a.createJobInstance, "POST", "", pv); w.Code != http.StatusBadRequest {
		t.Fatalf("job without jobType = %d", w.Code)
	}

	// scheduleJob creates a job and returns its id from the Location header.
	scheduleJob := func() string {
		t.Helper()
		w := doQuery(a.createJobInstance, "POST", "jobType=RunNotebook", pv)
		if w.Code != http.StatusAccepted {
			t.Fatalf("schedule = %d %s", w.Code, w.Body.Bytes())
		}
		loc := w.Header().Get("Location")
		return loc[strings.LastIndex(loc, "/")+1:]
	}
	getJob := func(jid string) (int, map[string]any) {
		t.Helper()
		w := do(a.getJobInstance, viewer, "GET", "", map[string]string{"wid": ws.ID, "iid": it.ID, "jid": jid})
		var body map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		return w.Code, body
	}

	// Zero delay → Completed immediately, with an end time.
	code, body := getJob(scheduleJob())
	if code != http.StatusOK || body["status"] != store.JobCompleted || body["endTimeUtc"] == nil {
		t.Fatalf("completed job = %d %v", code, body)
	}

	// Forced failure → Failed with a failureReason.
	a.SetFaults(1, -1, -1)
	code, body = getJob(scheduleJob())
	if code != http.StatusOK || body["status"] != store.JobFailed || body["failureReason"] == nil {
		t.Fatalf("failed job = %d %v", code, body)
	}

	// LRO delay override → NotStarted at creation, InProgress mid-flight,
	// with no end time yet.
	a.SetFaults(-1, -1, 5)
	jid := scheduleJob()
	if _, body = getJob(jid); body["status"] != store.JobNotStarted {
		t.Fatalf("fresh delayed job = %v", body)
	}
	st.Clock.Advance(1)
	if _, body = getJob(jid); body["status"] != store.JobInProgress || body["endTimeUtc"] != nil {
		t.Fatalf("running job = %v", body)
	}

	// Cancel (202 + Location) → Cancelled with the cancel time as end time.
	w := do(a.cancelJobInstance, admin, "POST", "", map[string]string{"wid": ws.ID, "iid": it.ID, "jid": jid})
	if w.Code != http.StatusAccepted || !strings.Contains(w.Header().Get("Location"), jid) {
		t.Fatalf("cancel = %d loc=%q", w.Code, w.Header().Get("Location"))
	}
	if _, body = getJob(jid); body["status"] != store.JobCancelled || body["endTimeUtc"] == nil {
		t.Fatalf("cancelled job = %v", body)
	}
}

func TestListWorkspacesAndRoleAssignments(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	wid := map[string]string{"wid": ws.ID}

	// Listing is scoped to the caller's grants.
	w := do(a.listWorkspaces, viewer, "GET", "", nil)
	var list struct{ Value []struct{ ID string } }
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if w.Code != http.StatusOK || len(list.Value) != 1 || list.Value[0].ID != ws.ID {
		t.Fatalf("viewer workspaces = %d %s", w.Code, w.Body.Bytes())
	}
	if w := do(a.listWorkspaces, nobody, "GET", "", nil); w.Code != http.StatusOK || w.Body.String() != `{"value":[]}`+"\n" {
		t.Fatalf("stranger workspaces = %d %q", w.Code, w.Body.String())
	}

	// Admins see the access list (viewer's 403 is covered elsewhere).
	w = do(a.listRoleAssignments, admin, "GET", "", wid)
	var ras struct{ Value []struct{ Role string } }
	_ = json.Unmarshal(w.Body.Bytes(), &ras)
	if w.Code != http.StatusOK || len(ras.Value) != 2 {
		t.Fatalf("role assignments = %d %s", w.Code, w.Body.Bytes())
	}
}

func TestGetWorkspaceSuccessAndIdentity(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	wid := map[string]string{"wid": ws.ID}

	// Plain read: no workspaceIdentity in the body.
	w := do(a.getWorkspace, viewer, "GET", "", wid)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "workspaceIdentity") {
		t.Fatalf("get = %d %s", w.Code, w.Body.Bytes())
	}

	// A provisioned identity rides along on GET.
	if err := st.SetWorkspaceIdentity(&store.WorkspaceIdentity{WorkspaceID: ws.ID, IdentityID: "sp-1", AppID: "app-1"}); err != nil {
		t.Fatal(err)
	}
	w = do(a.getWorkspace, admin, "GET", "", wid)
	var body struct {
		WorkspaceIdentity *struct{ ApplicationID string } `json:"workspaceIdentity"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != http.StatusOK || body.WorkspaceIdentity == nil {
		t.Fatalf("get with identity = %d %s", w.Code, w.Body.Bytes())
	}
}

func TestGitInitializeConnectionDirections(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	wid := map[string]string{"wid": ws.ID}
	connectGit(t, a, ws.ID)

	initConn := func() (int, map[string]any) {
		t.Helper()
		w := do(a.gitInitializeConnection, userAdmin, "POST", "{}", wid)
		var body map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		return w.Code, body
	}

	// Empty workspace, empty remote → nothing to sync.
	code, body := initConn()
	if code != http.StatusOK || body["requiredAction"] != "None" {
		t.Fatalf("empty init = %d %v", code, body)
	}
	// Workspace has items, remote empty → first sync pushes.
	it := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "nb"}
	if err := st.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	if code, body = initConn(); code != http.StatusOK || body["requiredAction"] != "CommitToGit" {
		t.Fatalf("items-only init = %d %v", code, body)
	}
	// Remote has a head → first sync pulls, and the head is reported.
	hash, err := st.CommitRemoteItems("GitHub|o||r|/", "main", []*store.RemoteItem{
		{Type: "Notebook", DisplayName: "nb"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if code, body = initConn(); code != http.StatusOK || body["requiredAction"] != "UpdateFromGit" || body["remoteCommitHash"] != hash {
		t.Fatalf("remote-head init = %d %v", code, body)
	}
}

func TestGitStatusAndCommitRoundTrip(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	wid := map[string]string{"wid": ws.ID}
	connectGit(t, a, ws.ID)

	type change struct {
		WorkspaceChange string
		RemoteChange    string
		ItemMetadata    struct{ DisplayName string }
	}
	status := func() (int, []change) {
		t.Helper()
		w := do(a.gitStatus, userAdmin, "GET", "", wid)
		var body struct{ Changes []change }
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		return w.Code, body.Changes
	}

	// Fresh connection: no changes either way.
	if code, changes := status(); code != http.StatusOK || len(changes) != 0 {
		t.Fatalf("clean status = %d %+v", code, changes)
	}

	// Workspace-only item shows as a workspace Added change.
	nb := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "nb"}
	parts := []store.DefinitionPart{{Path: "f", Payload: "djE=", PayloadType: "InlineBase64"}}
	if err := st.CreateItem(nb, parts); err != nil {
		t.Fatal(err)
	}
	code, changes := status()
	if code != http.StatusOK || len(changes) != 1 || changes[0].WorkspaceChange != "Added" {
		t.Fatalf("added status = %d %+v", code, changes)
	}

	// Commit pushes it to the remote (202) and the diff goes clean.
	if w := do(a.gitCommitToGit, userAdmin, "POST", `{"mode":"All","comment":"c"}`, wid); w.Code != http.StatusAccepted {
		t.Fatalf("commit = %d %s", w.Code, w.Body.Bytes())
	}
	if code, changes := status(); code != http.StatusOK || len(changes) != 0 {
		t.Fatalf("post-commit status = %d %+v", code, changes)
	}

	// Local edit shows Modified; a remote-only item shows remote Added.
	if err := st.SetDefinition(nb.ID, []store.DefinitionPart{{Path: "f", Payload: "djI=", PayloadType: "InlineBase64"}}); err != nil {
		t.Fatal(err)
	}
	remote, err := st.ListRemoteItems("GitHub|o||r|/", "main")
	if err != nil {
		t.Fatal(err)
	}
	remote = append(remote, &store.RemoteItem{Type: "Lakehouse", DisplayName: "remote-only"})
	if _, err := st.CommitRemoteItems("GitHub|o||r|/", "main", remote); err != nil {
		t.Fatal(err)
	}
	code, changes = status()
	if code != http.StatusOK || len(changes) != 2 {
		t.Fatalf("diff status = %d %+v", code, changes)
	}
	if changes[0].WorkspaceChange != "Modified" || changes[1].RemoteChange != "Added" ||
		changes[1].ItemMetadata.DisplayName != "remote-only" {
		t.Fatalf("diff changes = %+v", changes)
	}
}

func TestGitDisconnect(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	wid := map[string]string{"wid": ws.ID}
	connectGit(t, a, ws.ID)

	if w := do(a.gitDisconnect, userAdmin, "POST", "{}", wid); w.Code != http.StatusOK {
		t.Fatalf("disconnect = %d %s", w.Code, w.Body.Bytes())
	}
	// The binding is gone: git routes report unconnected again.
	if w := do(a.gitStatus, userAdmin, "GET", "", wid); w.Code != http.StatusBadRequest {
		t.Fatalf("status after disconnect = %d", w.Code)
	}
}

func TestGitDisconnectStorageFailure(t *testing.T) {
	a, st, dir := newDiskAPI(t)
	ws := seedWorkspace(t, st)
	connectGit(t, a, ws.ID)
	// A trigger blocks the delete while requireGit's read stays healthy.
	exec(t, dir, `CREATE TRIGGER no_git_del BEFORE DELETE ON git_connections BEGIN SELECT RAISE(ABORT, 'boom'); END`)
	if w := do(a.gitDisconnect, userAdmin, "POST", "{}", map[string]string{"wid": ws.ID}); w.Code != http.StatusInternalServerError {
		t.Fatalf("blocked disconnect = %d; want 500", w.Code)
	}
}
