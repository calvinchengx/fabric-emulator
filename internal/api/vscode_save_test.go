package api

// The VS Code extension's SAVE paths.
//
// The contract test covers create-then-read. What it never did was update an
// artifact's PAYLOAD — the branch a save actually takes — nor either name
// conflict, nor the notebook-content validations. `vscodeUpdateArtifact` was at
// 63.6% with the whole `workloadPayload != nil` block dark, which is the code
// that decides where a saved notebook's bytes are written.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// vscodeArtifactPayload reads back what the extension would next download.
func vscodeArtifactPayload(t *testing.T, a *API, iid string) string {
	t.Helper()
	w := vscodeDo(a.vscodeArtifact, "GET", "/metadata/artifacts/"+iid, "",
		map[string]string{"iid": iid})
	if w.Code != http.StatusOK {
		t.Fatalf("read back artifact = %d %s", w.Code, w.Body.String())
	}
	return w.Body.String()
}

func vscodeCreate(t *testing.T, a *API, wid, artifactType, name, payload string) string {
	t.Helper()
	body := `{"artifactType":"` + artifactType + `","displayName":"` + name + `"`
	if payload != "" {
		body += `,"workloadPayload":` + string(mustJSON(t, payload))
	}
	body += `}`
	w := vscodeDo(a.vscodeCreateArtifact, "POST", "/metadata/workspaces/x/artifacts",
		body, map[string]string{"wid": wid})
	if w.Code != http.StatusCreated {
		t.Fatalf("create %s = %d %s", name, w.Code, w.Body.String())
	}
	it, err := a.Store.GetItemByName(wid, name, vscodeItemType(artifactType))
	if err != nil {
		t.Fatalf("created artifact %q not found: %v", name, err)
	}
	return it.ID
}

// TestVSCodeUpdateWritesANotebookPayloadAsNotebookParts is the save path. A
// notebook's payload becomes notebook parts, NOT the opaque workload part used
// for everything else — get that wrong and the file saves "successfully" into a
// shape no notebook reader can open.
func TestVSCodeUpdateWritesANotebookPayloadAsNotebookParts(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	iid := vscodeCreate(t, a, ws.ID, "SynapseNotebook", "nb", vscodeNotebook)

	edited := `{"cells":[{"cell_type":"code","metadata":{},"source":["print(2)\n"],` +
		`"outputs":[],"execution_count":null}],"metadata":{},"nbformat":4,"nbformat_minor":5}`
	w := vscodeDo(a.vscodeUpdateArtifact, "PATCH", "/metadata/artifacts/"+iid,
		`{"WorkloadPayload":`+string(mustJSON(t, edited))+`}`,
		map[string]string{"iid": iid})
	if w.Code != http.StatusOK {
		t.Fatalf("update = %d %s", w.Code, w.Body.String())
	}

	// The edit is what comes back, and it comes back through the NOTEBOOK
	// reader — vscodeArtifact tries vscodeNotebookJSON first.
	if got := vscodeArtifactPayload(t, a, iid); !strings.Contains(got, "print(2)") {
		t.Fatalf("the saved edit was not returned:\n%s", got)
	}
	// The definition is stored as notebook parts, which is what makes the
	// notebook openable by everything else in the stack.
	parts, err := st.GetDefinition(iid)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, p := range parts {
		paths = append(paths, p.Path)
	}
	joined := strings.Join(paths, ",")
	if !strings.Contains(joined, "notebook-content") {
		t.Fatalf("a saved notebook was not stored as notebook parts: %v", paths)
	}
}

// TestVSCodeUpdateWritesANonNotebookPayloadAsAWorkloadPart is the other side of
// the same branch: anything that is not a notebook keeps the opaque payload.
func TestVSCodeUpdateWritesANonNotebookPayloadAsAWorkloadPart(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	iid := vscodeCreate(t, a, ws.ID, "SparkJobDefinition", "job", `{"a":1}`)

	w := vscodeDo(a.vscodeUpdateArtifact, "PATCH", "/metadata/artifacts/"+iid,
		`{"WorkloadPayload":`+string(mustJSON(t, `{"a":2}`))+`}`,
		map[string]string{"iid": iid})
	if w.Code != http.StatusOK {
		t.Fatalf("update = %d %s", w.Code, w.Body.String())
	}
	if got := vscodeArtifactPayload(t, a, iid); !strings.Contains(got, `a\":2`) &&
		!strings.Contains(got, `"a":2`) {
		t.Fatalf("the saved payload was not returned:\n%s", got)
	}
	parts, err := st.GetDefinition(iid)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range parts {
		if strings.Contains(p.Path, "notebook-content") {
			t.Fatalf("a non-notebook was stored as notebook parts: %v", p.Path)
		}
	}
}

// TestVSCodeUpdateRenamesAndKeepsDescription: the metadata half of the same
// handler, asserted separately so a payload bug and a rename bug cannot mask
// each other.
func TestVSCodeUpdateRenamesAndKeepsDescription(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	iid := vscodeCreate(t, a, ws.ID, "SynapseNotebook", "before", vscodeNotebook)

	w := vscodeDo(a.vscodeUpdateArtifact, "PATCH", "/metadata/artifacts/"+iid,
		`{"DisplayName":"after","Description":"why"}`, map[string]string{"iid": iid})
	if w.Code != http.StatusOK {
		t.Fatalf("rename = %d %s", w.Code, w.Body.String())
	}
	it, err := st.GetItemByID(iid)
	if err != nil {
		t.Fatal(err)
	}
	if it.DisplayName != "after" || it.Description != "why" {
		t.Fatalf("item = %q / %q; want after / why", it.DisplayName, it.Description)
	}
}

// TestVSCodeCreateRefusesADuplicateName: the extension shows this as
// "name already in use", so it must be a 409 with that code and not a 500 —
// the difference decides whether the user is told to pick another name or to
// file a bug.
func TestVSCodeCreateRefusesADuplicateName(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	vscodeCreate(t, a, ws.ID, "SynapseNotebook", "taken", vscodeNotebook)

	w := vscodeDo(a.vscodeCreateArtifact, "POST", "/metadata/workspaces/x/artifacts",
		`{"artifactType":"SynapseNotebook","displayName":"taken"}`,
		map[string]string{"wid": ws.ID})
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate create = %d %s; want 409", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ArtifactDisplayNameAlreadyInUse") {
		t.Errorf("the conflict is not named: %s", w.Body.String())
	}
}

// TestVSCodeUpdateRefusesARenameOntoAnExistingName is the same conflict on the
// update path, which reports it separately.
func TestVSCodeUpdateRefusesARenameOntoAnExistingName(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	vscodeCreate(t, a, ws.ID, "SynapseNotebook", "first", vscodeNotebook)
	second := vscodeCreate(t, a, ws.ID, "SynapseNotebook", "second", vscodeNotebook)

	w := vscodeDo(a.vscodeUpdateArtifact, "PATCH", "/metadata/artifacts/"+second,
		`{"DisplayName":"first"}`, map[string]string{"iid": second})
	if w.Code != http.StatusConflict {
		t.Fatalf("rename onto an existing name = %d %s; want 409", w.Code, w.Body.String())
	}
	// The rename must not have half-applied.
	it, err := st.GetItemByID(second)
	if err != nil {
		t.Fatal(err)
	}
	if it.DisplayName != "second" {
		t.Errorf("a refused rename still changed the name to %q", it.DisplayName)
	}
}

// TestVSCodeUpdateRejectsAMalformedBody: a broken PATCH is a client error, not
// a partial write.
func TestVSCodeUpdateRejectsAMalformedBody(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	iid := vscodeCreate(t, a, ws.ID, "SynapseNotebook", "nb", vscodeNotebook)

	w := vscodeDo(a.vscodeUpdateArtifact, "PATCH", "/metadata/artifacts/"+iid,
		`{"DisplayName": `, map[string]string{"iid": iid})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed PATCH = %d %s; want 400", w.Code, w.Body.String())
	}
}

// TestVSCodeNotebookContentRejectsNonJSON: notebook content is stored as a
// notebook definition, so content that is not JSON must be refused at the door
// rather than written and failed later by every reader.
func TestVSCodeNotebookContentRejectsNonJSON(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	iid := vscodeCreate(t, a, ws.ID, "SynapseNotebook", "nb", vscodeNotebook)

	w := vscodeDo(a.vscodeUpdateNotebookContent, "PUT",
		"/metadata/workspaces/x/artifacts/y/notebookcontent", "this is not json",
		map[string]string{"wid": ws.ID, "iid": iid})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("non-JSON content = %d %s; want 400", w.Code, w.Body.String())
	}
	// The stored notebook is unchanged.
	if got := vscodeArtifactPayload(t, a, iid); !strings.Contains(got, "print(1)") {
		t.Errorf("a rejected write changed the stored notebook:\n%s", got)
	}
}

// TestVSCodeNotebookContentRefusesANonNotebook: the route is notebook-only, and
// the artifact type is checked against the id rather than trusted from the URL.
func TestVSCodeNotebookContentRefusesANonNotebook(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	iid := vscodeCreate(t, a, ws.ID, "SparkJobDefinition", "job", `{"a":1}`)

	w := vscodeDo(a.vscodeUpdateNotebookContent, "PUT",
		"/metadata/workspaces/x/artifacts/y/notebookcontent", `{"cells":[]}`,
		map[string]string{"wid": ws.ID, "iid": iid})
	if w.Code != http.StatusNotFound {
		t.Fatalf("notebook content on a job = %d %s; want 404", w.Code, w.Body.String())
	}
}

// TestVSCodeNotebookContentRefusesAWorkspaceMismatch: the workspace in the path
// must agree with the artifact's own. Trusting the URL would let a caller
// authorized in one workspace address an artifact by id in another.
func TestVSCodeNotebookContentRefusesAWorkspaceMismatch(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	iid := vscodeCreate(t, a, ws.ID, "SynapseNotebook", "nb", vscodeNotebook)

	other := &store.Workspace{DisplayName: "elsewhere"}
	if err := st.CreateWorkspace(other, store.Principal{ID: admin.ID, Type: admin.Type}); err != nil {
		t.Fatal(err)
	}
	w := vscodeDo(a.vscodeUpdateNotebookContent, "PUT",
		"/metadata/workspaces/x/artifacts/y/notebookcontent", `{"cells":[]}`,
		map[string]string{"wid": other.ID, "iid": iid})
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-workspace notebook write = %d %s; want 404", w.Code, w.Body.String())
	}
}
