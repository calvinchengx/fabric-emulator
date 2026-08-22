package onelake

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// principalAccess is the contract a third-party engine builds against, so the
// tests are about what an ENGINE would get wrong if we shaped it loosely: who
// may ask, whose answer it is, and whether "no access" is distinguishable from
// "something went wrong".

func paURL(f *fixture) string {
	return "/v1.0/workspaces/" + f.ws.ID + "/artifacts/" + f.it.ID + "/securityPolicy/principalAccess"
}

func askAccess(t *testing.T, f *fixture, caller, subject, input string) (*principalAccessResponse, int) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"aadObjectId": subject, "inputPath": input})
	if err != nil {
		t.Fatal(err)
	}
	w := f.do("GET", paURL(f), f.storageToken(caller), body)
	if w.Code != http.StatusOK {
		return nil, w.Code
	}
	var out principalAccessResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not the documented shape: %s", w.Body)
	}
	return &out, w.Code
}

// The whole point: an engine asks what a USER may see, and gets the row filter
// as SQL to apply itself.
func TestPrincipalAccessReportsTheSubjectsGrant(t *testing.T) {
	f := newFixture(t)
	grantRole(t, f, "engine-1", store.RoleMember)
	grantRole(t, f, "viewer-1", store.RoleViewer)
	putRole(t, f, "viewer-1", "Tables/dbo/Customers")

	out, code := askAccess(t, f, "engine-1", "viewer-1", "Tables")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(out.Value) != 1 || out.Value[0].Path != "Tables/dbo/Customers" {
		t.Fatalf("value = %+v", out.Value)
	}
	if out.Value[0].Effect != "Permit" || len(out.Value[0].Access) != 1 {
		t.Fatalf("entry = %+v", out.Value[0])
	}
	if out.IdentityETag == "" || out.MetadataETag == "" {
		t.Fatalf("etags missing: %+v", out)
	}
}

// "No access" is an ANSWER, not an error. An engine acts on it by returning no
// rows; a 403 or a 404 would make it retry or fail the query instead.
func TestNoAccessIsAnEmptyListNotAnError(t *testing.T) {
	f := newFixture(t)
	grantRole(t, f, "engine-1", store.RoleMember)
	grantRole(t, f, "viewer-1", store.RoleViewer)

	out, code := askAccess(t, f, "engine-1", "viewer-1", "Tables")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(out.Value) != 0 {
		t.Fatalf("value = %+v, want empty", out.Value)
	}
	// A stranger to the workspace is the same shape of answer.
	out, code = askAccess(t, f, "engine-1", "nobody-1", "Tables")
	if code != http.StatusOK || len(out.Value) != 0 {
		t.Fatalf("stranger: status %d value %+v", code, out.Value)
	}
}

// The CALLER is privileged, the SUBJECT need not be. Letting anyone ask would
// make this an oracle for enumerating other people's access.
func TestOnlyAPrivilegedCallerMayAsk(t *testing.T) {
	f := newFixture(t)
	grantRole(t, f, "viewer-1", store.RoleViewer)
	putRole(t, f, "viewer-1", "Tables/dbo/Customers")

	// A Viewer — even one granted a table — cannot read the policy.
	if _, code := askAccess(t, f, "viewer-1", "viewer-1", "Tables"); code != http.StatusForbidden {
		t.Errorf("viewer asking = %d, want 403", code)
	}
	// And a principal with no role at all certainly cannot.
	if _, code := askAccess(t, f, "nobody-1", "viewer-1", "Tables"); code != http.StatusForbidden {
		t.Errorf("stranger asking = %d, want 403", code)
	}
}

// Admin/Member/Contributor "override any OneLake security Read permissions", so
// the honest answer for them is the whole half — not whatever roles name them.
// An engine told otherwise would filter rows away from someone entitled to all.
func TestASubjectWithReadAllSeesEverything(t *testing.T) {
	f := newFixture(t)
	grantRole(t, f, "engine-1", store.RoleMember)
	grantRole(t, f, "contrib-1", store.RoleContributor)
	// A role that names someone else and narrows one table.
	putRole(t, f, "viewer-1", "Tables/dbo/Customers")

	out, _ := askAccess(t, f, "engine-1", "contrib-1", "Tables")
	if len(out.Value) != 1 || out.Value[0].Path != "Tables" || out.Value[0].Rows != "" {
		t.Fatalf("a ReadAll subject was narrowed: %+v", out.Value)
	}
}

// inputPath picks a half, and a Files rule must not answer a Tables question.
func TestInputPathSelectsTheHalf(t *testing.T) {
	f := newFixture(t)
	grantRole(t, f, "engine-1", store.RoleMember)
	grantRole(t, f, "viewer-1", store.RoleViewer)
	putRole(t, f, "viewer-1", "Files/raw")

	if out, _ := askAccess(t, f, "engine-1", "viewer-1", "Files"); len(out.Value) != 1 {
		t.Fatalf("Files = %+v", out.Value)
	}
	if out, _ := askAccess(t, f, "engine-1", "viewer-1", "Tables"); len(out.Value) != 0 {
		t.Fatalf("a Files rule answered a Tables question: %+v", out.Value)
	}
}

func TestPrincipalAccessRefusesBadRequests(t *testing.T) {
	f := newFixture(t)
	grantRole(t, f, "engine-1", store.RoleMember)
	tok := f.storageToken("engine-1")

	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"no subject", `{"inputPath":"Tables"}`, http.StatusBadRequest},
		{"unknown half", `{"aadObjectId":"x","inputPath":"Everything"}`, http.StatusBadRequest},
		{"malformed", `{`, http.StatusBadRequest},
	} {
		if w := f.do("GET", paURL(f), tok, []byte(tc.body)); w.Code != tc.want {
			t.Errorf("%s = %d, want %d (%s)", tc.name, w.Code, tc.want, w.Body)
		}
	}
	// The bare securityPolicy path says what to call instead of 404ing blankly.
	bare := "/v1.0/workspaces/" + f.ws.ID + "/artifacts/" + f.it.ID + "/securityPolicy"
	if w := f.do("GET", bare, tok, nil); w.Code != http.StatusNotFound ||
		!strings.Contains(w.Body.String(), "principalAccess") {
		t.Errorf("bare securityPolicy = %d %s", w.Code, w.Body)
	}
}

// The ETag is what lets an engine cache policy across a query. It has to change
// when the policy changes and not otherwise, or the cache is either stale or
// pointless.
func TestTheMetadataETagTracksThePolicy(t *testing.T) {
	f := newFixture(t)
	grantRole(t, f, "engine-1", store.RoleMember)
	grantRole(t, f, "viewer-1", store.RoleViewer)
	putRole(t, f, "viewer-1", "Tables/dbo/Customers")

	first, _ := askAccess(t, f, "engine-1", "viewer-1", "Tables")
	again, _ := askAccess(t, f, "engine-1", "viewer-1", "Tables")
	if first.MetadataETag != again.MetadataETag {
		t.Fatal("the etag changed without the policy changing")
	}
	// Conditional: the same tag is 304, so an engine can skip the round trip.
	body, _ := json.Marshal(map[string]any{"aadObjectId": "viewer-1", "inputPath": "Tables"})
	r := f.doWithHeader("GET", paURL(f), f.storageToken("engine-1"), body,
		map[string]string{"If-None-Match": `"` + first.MetadataETag + `"`})
	if r.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match with the current tag = %d, want 304", r.Code)
	}

	putRole(t, f, "viewer-1", "Tables/dbo/Orders")
	changed, _ := askAccess(t, f, "engine-1", "viewer-1", "Tables")
	if changed.MetadataETag == first.MetadataETag {
		t.Fatal("the etag survived a policy change")
	}
}

// doWithHeader is do() plus request headers, which the conditional-request test
// needs and the DFS fixture does not otherwise expose.
func (f *fixture) doWithHeader(method, target, token string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	r := httptest.NewRequest(method, target, rd)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.svc.ServeHTTP(w, r)
	return w
}

func TestPrincipalAccessRefusesUnknownTargetsAndMethods(t *testing.T) {
	f := newFixture(t)
	grantRole(t, f, "engine-1", store.RoleMember)
	tok := f.storageToken("engine-1")
	body := []byte(`{"aadObjectId":"viewer-1","inputPath":"Tables"}`)

	if w := f.do("DELETE", paURL(f), tok, body); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE = %d, want 405", w.Code)
	}
	unknownWS := "/v1.0/workspaces/no-such-ws/artifacts/" + f.it.ID + "/securityPolicy/principalAccess"
	if w := f.do("GET", unknownWS, tok, body); w.Code != http.StatusNotFound {
		t.Errorf("unknown workspace = %d, want 404", w.Code)
	}
	unknownItem := "/v1.0/workspaces/" + f.ws.ID + "/artifacts/no-such-item/securityPolicy/principalAccess"
	if w := f.do("GET", unknownItem, tok, body); w.Code != http.StatusNotFound {
		t.Errorf("unknown item = %d, want 404", w.Code)
	}
}

// Every store read behind this endpoint must fail loudly. An engine handed an
// empty policy because the database was unreachable would filter every row away
// and report success — a wrong answer that looks like a correct one.
func TestPrincipalAccessFailsLoudlyOnStoreErrors(t *testing.T) {
	for _, table := range []string{"role_assignments", "onelake_roles"} {
		dir := t.TempDir()
		st, err := store.Open(dir, clock.New())
		if err != nil {
			t.Fatal(err)
		}
		ws := &store.Workspace{DisplayName: "pa-ws"}
		if err := st.CreateWorkspace(ws, store.Principal{ID: "engine-1", Type: "ServicePrincipal"}); err != nil {
			t.Fatal(err)
		}
		it := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lake"}
		if err := st.CreateItem(it, nil); err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", filepath.Join(dir, "fabric-emulator.db"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("DROP TABLE " + table); err != nil {
			t.Fatal(err)
		}
		_ = db.Close()

		f := newFixture(t)
		f.svc.Store = st
		url := "/v1.0/workspaces/" + ws.ID + "/artifacts/" + it.ID + "/securityPolicy/principalAccess"
		w := f.do("GET", url, f.storageToken("engine-1"),
			[]byte(`{"aadObjectId":"viewer-1","inputPath":"Tables"}`))
		if w.Code != http.StatusInternalServerError {
			t.Errorf("with %s dropped = %d, want 500 (%s)", table, w.Code, w.Body)
		}
		_ = st.Close()
	}
}

// The subject lookup has its own store read, reachable only on a transient
// failure once the caller check has passed. Called directly, because that is
// the only way to construct the state deterministically.
func TestSubjectAccessSurfacesAStoreFailure(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir, clock.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	db, err := sql.Open("sqlite", filepath.Join(dir, "fabric-emulator.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("DROP TABLE role_assignments"); err != nil {
		t.Fatal(err)
	}
	s := &Service{Store: st}
	if _, err := s.subjectAccess("ws", "subject", "Tables", nil); err == nil {
		t.Fatal("an unreadable role assignment was reported as no access")
	}
}

// Both OneLake spellings reach the same endpoint. A client picks a surface by
// URL shape, and a policy readable through one but not the other would make
// enforcement depend on how the caller happened to address the account.
func TestPrincipalAccessIsReachableOnBothSurfaces(t *testing.T) {
	f := newFixture(t)
	grantRole(t, f, "engine-1", store.RoleMember)
	grantRole(t, f, "viewer-1", store.RoleViewer)
	putRole(t, f, "viewer-1", "Tables/dbo/Customers")
	body := []byte(`{"aadObjectId":"viewer-1","inputPath":"Tables"}`)
	tok := f.storageToken("engine-1")

	dfs := f.do("GET", paURL(f), tok, body)
	blob := f.doBlob("GET", paURL(f), tok, body, nil)
	if dfs.Code != http.StatusOK || blob.Code != http.StatusOK {
		t.Fatalf("dfs = %d, blob = %d; want 200 from both (%s)", dfs.Code, blob.Code, blob.Body)
	}
	if dfs.Body.String() != blob.Body.String() {
		t.Fatalf("the two surfaces answered differently:\n dfs=%s\nblob=%s", dfs.Body, blob.Body)
	}
}

// The bar is Member, not ReadAll. A Contributor can read the DATA and still
// cannot read the POLICY: the integration guide puts the engine identity in the
// Member role specifically because that is what grants "access to read OneLake
// security role metadata through the authorized engine APIs". Gating on
// Contributor would be laxer than the product, in the direction that leaks
// who-can-see-what.
func TestAContributorCannotReadThePolicy(t *testing.T) {
	f := newFixture(t)
	grantRole(t, f, "contrib-1", store.RoleContributor)
	grantRole(t, f, "member-1", store.RoleMember)
	grantRole(t, f, "viewer-1", store.RoleViewer)
	putRole(t, f, "viewer-1", "Tables/dbo/Customers")

	if _, code := askAccess(t, f, "contrib-1", "viewer-1", "Tables"); code != http.StatusForbidden {
		t.Errorf("contributor = %d, want 403", code)
	}
	if _, code := askAccess(t, f, "member-1", "viewer-1", "Tables"); code != http.StatusOK {
		t.Errorf("member = %d, want 200", code)
	}
	// And the contributor can still read the data it was refused the policy for
	// — the two permissions are genuinely separate.
	seedFile(t, f, "Tables/dbo/Customers/part-0.parquet")
	if w := f.do("GET", "/"+f.ws.ID+"/"+f.it.ID+"/Tables/dbo/Customers/part-0.parquet",
		f.storageToken("contrib-1"), nil); w.Code != http.StatusOK {
		t.Errorf("contributor data read = %d, want 200", w.Code)
	}
}
