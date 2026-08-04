package api

import (
	"encoding/base64"
	"encoding/json"
	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

const vscodeNotebook = `{"cells":[{"cell_type":"code","metadata":{},"source":["print(1)\n"],"outputs":[],"execution_count":null}],"metadata":{},"nbformat":4,"nbformat_minor":5}`

func vscodeDo(h handler, method, target, body string, pth map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range pth {
		r.SetPathValue(k, v)
	}
	w := httptest.NewRecorder()
	h(w, r, admin)
	return w
}

func TestVSCodeWorkspaceArtifactAndContentContract(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)

	w := vscodeDo(a.vscodeWorkspaces, "GET", "/metadata/v201606/workspaces/", "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"capacityObjectId"`) || !strings.Contains(w.Body.String(), ws.ID) {
		t.Fatalf("workspaces = %d %s", w.Code, w.Body.String())
	}

	create := `{"artifactType":"SynapseNotebook","displayName":"VS Code notebook","description":"d","workloadPayload":` + string(mustJSON(t, vscodeNotebook)) + `}`
	w = vscodeDo(a.vscodeCreateArtifact, "POST", "/metadata/workspaces/x/artifacts", create, map[string]string{"wid": ws.ID})
	if w.Code != http.StatusCreated || w.Header().Get("ETag") == "" {
		t.Fatalf("create = %d %s", w.Code, w.Body.String())
	}
	var artifact map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &artifact); err != nil {
		t.Fatal(err)
	}
	iid := artifact["objectId"].(string)
	if artifact["artifactType"] != "SynapseNotebook" || artifact["folderObjectId"] != ws.ID {
		t.Fatalf("artifact = %#v", artifact)
	}

	w = vscodeDo(a.vscodeArtifacts, "GET", "/metadata/workspaces/x/artifacts", "", map[string]string{"wid": ws.ID})
	if w.Code != 200 || !strings.Contains(w.Body.String(), iid) {
		t.Fatalf("list = %d %s", w.Code, w.Body.String())
	}
	w = vscodeDo(a.vscodeArtifact, "GET", "/metadata/artifacts/x", "", map[string]string{"iid": iid})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"workloadPayload"`) {
		t.Fatalf("get = %d %s", w.Code, w.Body.String())
	}

	path := map[string]string{"wid": ws.ID, "iid": iid}
	w = vscodeDo(a.vscodeNotebookContent, "GET", "/webapi/content", "", path)
	etag := w.Header().Get("ETag")
	if w.Code != 200 || etag == "" || strings.TrimSpace(w.Body.String()) != vscodeNotebook {
		t.Fatalf("content = %d %q", w.Code, w.Body.String())
	}
	w = vscodeDo(a.vscodeNotebookContent, "HEAD", "/webapi/content", "", path)
	if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("ETag") != etag {
		t.Fatalf("head = %d len=%d", w.Code, w.Body.Len())
	}

	r := httptest.NewRequest("PUT", "/webapi/content", strings.NewReader(strings.Replace(vscodeNotebook, "print(1)", "print(2)", 1)))
	for k, v := range path {
		r.SetPathValue(k, v)
	}
	r.Header.Set("If-Match", etag)
	w = httptest.NewRecorder()
	a.vscodeUpdateNotebookContent(w, r, admin)
	if w.Code != 200 || w.Header().Get("ETag") == etag {
		t.Fatalf("update = %d %s", w.Code, w.Body.String())
	}
	parts, _ := st.GetDefinition(iid)
	if len(parts) != 2 || parts[0].Path != "notebook-content.ipynb" || parts[1].Path != "notebook-content.py" {
		t.Fatalf("parts = %#v", parts)
	}
	py, _ := base64.StdEncoding.DecodeString(parts[1].Payload)
	if !strings.Contains(string(py), "print(2)") {
		t.Fatalf("converted source = %s", py)
	}

	r = httptest.NewRequest("PUT", "/webapi/content", strings.NewReader(vscodeNotebook))
	for k, v := range path {
		r.SetPathValue(k, v)
	}
	r.Header.Set("If-Match", etag)
	w = httptest.NewRecorder()
	a.vscodeUpdateNotebookContent(w, r, admin)
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale update = %d", w.Code)
	}

	w = vscodeDo(a.vscodeUpdateArtifact, "PATCH", "/metadata/artifacts/x", `{"displayName":"renamed","description":"new"}`, map[string]string{"iid": iid})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "renamed") {
		t.Fatalf("metadata update = %d %s", w.Code, w.Body.String())
	}
	w = vscodeDo(a.vscodeDeleteArtifact, "DELETE", "/metadata/artifacts/x", "", map[string]string{"iid": iid})
	if w.Code != 200 {
		t.Fatalf("delete = %d", w.Code)
	}
}

func TestVSCodeDiscoveryDatahubTokenAndConversions(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	for _, it := range []*store.Item{{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lake"}, {WorkspaceID: ws.ID, Type: "Environment", DisplayName: "env"}} {
		if err := st.CreateItem(it, nil); err != nil {
			t.Fatal(err)
		}
	}
	w := vscodeDo(a.vscodeCluster, "PUT", "https://api.powerbi.com/spglobalservice/x", "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "api.powerbi.com") {
		t.Fatalf("cluster = %s", w.Body.String())
	}
	w = vscodeDo(a.vscodeDatahubArtifacts, "POST", "/metadata/datahub/V2/artifacts", `{"supportedTypes":["Lakehouse"]}`, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "lake") || strings.Contains(w.Body.String(), "env") {
		t.Fatalf("datahub = %d %s", w.Code, w.Body.String())
	}

	r := httptest.NewRequest("POST", "https://fabric.local/metadata/v201606/generatemwctoken", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer a.b.c")
	w = httptest.NewRecorder()
	a.vscodeMWCToken(w, r, admin)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"Token":"a.b.c"`) || !strings.Contains(w.Body.String(), `"TargetUriHost":"fabric.local"`) {
		t.Fatalf("token = %d %s", w.Code, w.Body.String())
	}

	src := []byte("# Fabric notebook source\n# CELL ****\nprint(7)\n# MARKDOWN ****\n# MAGIC hello\n")
	ipynb := vscodeFabricToIPYNB(src)
	if !json.Valid(ipynb) || !strings.Contains(string(ipynb), "print(7)") || !strings.Contains(string(ipynb), "markdown") {
		t.Fatalf("ipynb = %s", ipynb)
	}
	if got := string(vscodeIPYNBToFabric(ipynb)); !strings.Contains(got, "print(7)") || !strings.Contains(got, "# MAGIC hello") {
		t.Fatalf("roundtrip = %s", got)
	}
}

func TestVSCodeCompatibilityErrors(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	badCreates := []string{`{`, `{"artifactType":"SynapseNotebook"}`}
	for _, body := range badCreates {
		if w := vscodeDo(a.vscodeCreateArtifact, "POST", "/x", body, map[string]string{"wid": ws.ID}); w.Code != 400 {
			t.Errorf("create %q = %d", body, w.Code)
		}
	}
	if w := vscodeDo(a.vscodeDatahubArtifacts, "POST", "/x", `{}`, nil); w.Code != 400 {
		t.Errorf("datahub invalid = %d", w.Code)
	}
	if w := vscodeDo(a.vscodeArtifact, "GET", "/x", "", map[string]string{"iid": "missing"}); w.Code != 404 {
		t.Errorf("missing = %d", w.Code)
	}
	it := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "empty"}
	if err := st.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	if w := vscodeDo(a.vscodeNotebookContent, "GET", "/x", "", map[string]string{"wid": ws.ID, "iid": it.ID}); w.Code != 500 {
		t.Errorf("empty definition = %d", w.Code)
	}
	if w := vscodeDo(a.vscodeUpdateArtifact, "PATCH", "/x", `{`, map[string]string{"iid": it.ID}); w.Code != 400 {
		t.Errorf("bad patch = %d", w.Code)
	}
}

func TestVSCodeNotebookResources(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	it := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "resources"}
	if err := st.CreateItem(it, vscodeNotebookParts([]byte(vscodeNotebook))); err != nil {
		t.Fatal(err)
	}
	path := map[string]string{"wid": ws.ID, "iid": it.ID, "path": "images/a.txt"}
	w := vscodeDo(a.vscodeResourcePut, "PUT", "/filesystem/workdir/images/a.txt", "hello", path)
	if w.Code != 200 {
		t.Fatalf("put = %d %s", w.Code, w.Body.String())
	}
	w = vscodeDo(a.vscodeResourceGet, "GET", "/filesystem/workdir/images/a.txt", "", path)
	if w.Code != 200 || w.Body.String() != "hello" {
		t.Fatalf("get = %d %q", w.Code, w.Body.String())
	}
	w = vscodeDo(a.vscodeResourceGet, "GET", "/filesystem/workdir/images?recursive=false", "", map[string]string{"wid": ws.ID, "iid": it.ID, "path": "images"})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"a.txt"`) || !strings.Contains(w.Body.String(), `"fileSystemEntryType":"file"`) {
		t.Fatalf("list = %d %s", w.Code, w.Body.String())
	}
	w = vscodeDo(a.vscodeResourceUsage, "GET", "/filesystem/workdirUsage", "", map[string]string{"wid": ws.ID, "iid": it.ID})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"usedBytes":5`) {
		t.Fatalf("usage = %d %s", w.Code, w.Body.String())
	}
	w = vscodeDo(a.vscodeResourceDelete, "DELETE", "/filesystem/workdir/images/a.txt", "", path)
	if w.Code != 200 {
		t.Fatalf("delete = %d", w.Code)
	}
	if w = vscodeDo(a.vscodeResourceGet, "GET", "/filesystem/workdir/images/a.txt", "", path); w.Code != 404 {
		t.Fatalf("deleted get = %d", w.Code)
	}
	unsafe := map[string]string{"wid": ws.ID, "iid": it.ID, "path": "../../escape"}
	for name, h := range map[string]handler{"get": a.vscodeResourceGet, "put": a.vscodeResourcePut, "delete": a.vscodeResourceDelete} {
		if w = vscodeDo(h, "PUT", "/filesystem/workdir/../../escape", "x", unsafe); w.Code != 400 {
			t.Errorf("%s traversal = %d", name, w.Code)
		}
	}
}

func TestVSCodeSparkJobsWorkloadMetadataAndLakehouseTables(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	create := `{"artifactType":"SparkJobDefinition","displayName":"sjd","workloadPayload":"{\"executableFile\":\"main.py\"}"}`
	w := vscodeDo(a.vscodeCreateArtifact, "POST", "/x", create, map[string]string{"wid": ws.ID})
	if w.Code != 201 {
		t.Fatalf("create=%d %s", w.Code, w.Body.String())
	}
	var artifact map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &artifact)
	iid := artifact["objectId"].(string)
	w = vscodeDo(a.vscodeArtifact, "GET", "/x", "", map[string]string{"iid": iid})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `executableFile`) {
		t.Fatalf("metadata=%d %s", w.Code, w.Body.String())
	}
	w = vscodeDo(a.vscodeRunSparkJob, "POST", "/x", "", map[string]string{"iid": iid})
	if w.Code != 200 {
		t.Fatalf("run=%d %s", w.Code, w.Body.String())
	}
	var run map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &run)
	jid := run["id"].(string)
	w = vscodeDo(a.vscodeSparkJobs, "GET", "/x", "", map[string]string{"wid": ws.ID, "iid": iid})
	if w.Code != 200 || !strings.Contains(w.Body.String(), jid) {
		t.Fatalf("jobs=%d %s", w.Code, w.Body.String())
	}
	w = vscodeDo(a.vscodeCancelSparkJob, "DELETE", "/x", "", map[string]string{"iid": iid, "jid": jid})
	if w.Code != 200 {
		t.Fatalf("cancel=%d", w.Code)
	}

	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lake"}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateOneLakePath(&store.OneLakePath{WorkspaceID: ws.ID, ItemID: lake.ID, RelPath: "Tables/sales/_delta_log/0.json", Content: []byte("x")}, false); err != nil {
		t.Fatal(err)
	}
	w = vscodeDo(a.vscodeLakehouseTables, "GET", "/x", "", map[string]string{"wid": ws.ID, "iid": lake.ID})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"sales"`) {
		t.Fatalf("tables=%d %s", w.Code, w.Body.String())
	}
}

func mustJSON(t *testing.T, s string) []byte {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// vscodeDoAs is vscodeDo with the principal under test. vscodeDo hardcodes
// admin, which is why every refusal branch on this surface was unexercised.
func vscodeDoAs(h handler, p *auth.Principal, method, target, body string, pth map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range pth {
		r.SetPathValue(k, v)
	}
	w := httptest.NewRecorder()
	h(w, r, p)
	return w
}

// TestVSCodeSurfaceEnforcesRBAC walks every item-scoped handler on the VS Code
// protocol and asserts the two refusals its guard exists for: a principal with
// no role on the workspace is denied, and a Viewer is denied the writes.
//
// This surface is a private Microsoft protocol the real extension speaks, so it
// is easy to think of it as a translation layer rather than a door. It is a
// door: every handler reaches the same item store the public API guards, and
// each one calls vscodeAuthorizedItem for that reason. Nothing exercised those
// refusals, so the whole surface's authorization rested on code that no test
// ran — the same shape as the TDS empty-database hole.
func TestVSCodeSurfaceEnforcesRBAC(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	nb := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "nb"}
	if err := st.CreateItem(nb, nil); err != nil {
		t.Fatal(err)
	}
	iid := map[string]string{"iid": nb.ID}

	// Writes: a Viewer holds a role but not enough of one.
	writes := map[string]handler{
		"vscodeUpdateArtifact":        a.vscodeUpdateArtifact,
		"vscodeDeleteArtifact":        a.vscodeDeleteArtifact,
		"vscodeNotebookContent":       a.vscodeNotebookContent,
		"vscodeUpdateNotebookContent": a.vscodeUpdateNotebookContent,
		"vscodeRunSparkJob":           a.vscodeRunSparkJob,
		"vscodeCancelSparkJob":        a.vscodeCancelSparkJob,
	}
	for name, h := range writes {
		if w := vscodeDoAs(h, viewer, "POST", "/x", "{}", iid); w.Code != http.StatusForbidden {
			t.Errorf("%s as viewer = %d; want 403 — a Viewer must not write", name, w.Code)
		}
	}

	// Reads and writes alike: a principal with NO role on the workspace.
	all := map[string]handler{
		"vscodeArtifact":        a.vscodeArtifact,
		"vscodeSparkJobs":       a.vscodeSparkJobs,
		"vscodeLakehouseTables": a.vscodeLakehouseTables,
	}
	for name, h := range writes {
		all[name] = h
	}
	for name, h := range all {
		if w := vscodeDoAs(h, nobody, "POST", "/x", "{}", iid); w.Code != http.StatusForbidden {
			t.Errorf("%s as an ungranted principal = %d; want 403", name, w.Code)
		}
	}

	// An unknown artifact is 404 before any role is considered, so a probe
	// cannot use the difference to learn which ids exist.
	unknown := map[string]string{"iid": "no-such-item"}
	for name, h := range all {
		if w := vscodeDoAs(h, admin, "POST", "/x", "{}", unknown); w.Code != http.StatusNotFound {
			t.Errorf("%s on an unknown artifact = %d; want 404", name, w.Code)
		}
	}
}
