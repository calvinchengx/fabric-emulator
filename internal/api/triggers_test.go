package api

// Event triggers: the control surface, the OneLake→Reflex→pipeline path, the
// TriggerEvent binding, and the cycle guard.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

type trigFixture struct {
	a      *API
	st     *store.Store
	ws     *store.Workspace
	reflex *store.Item
	lake   *store.Item
	pipe   *store.Item
	vals   map[string]string
}

// newTrigFixture builds workspace + Reflex + Lakehouse (the event source) +
// DataPipeline (the action), and subscribes the dispatcher to file events the
// way the server does.
func newTrigFixture(t *testing.T, pipelineDef string) *trigFixture {
	t.Helper()
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	mk := func(name, typ string) *store.Item {
		it := &store.Item{WorkspaceID: ws.ID, DisplayName: name, Type: typ}
		if err := st.CreateItem(it, nil); err != nil {
			t.Fatal(err)
		}
		return it
	}
	reflex, lake := mk("watcher", "Reflex"), mk("bronze", "Lakehouse")
	pipe := createPipeline(t, st, ws.ID, pipelineDef)
	st.FileEvents = func(ev store.FileEvent) { a.DispatchFileEvent(ev) }
	return &trigFixture{a: a, st: st, ws: ws, reflex: reflex, lake: lake, pipe: pipe,
		vals: map[string]string{"wid": ws.ID, "iid": reflex.ID}}
}

// waitDef is a pipeline that always succeeds, for tests about firing rather
// than about what runs.
const waitDef = `{"properties":{"activities":[
	{"name":"Noop","type":"Wait","typeProperties":{"waitTimeInSeconds":1}}]}}`

func (f *trigFixture) createTrigger(p *auth.Principal, body string) *httptest.ResponseRecorder {
	return do(f.a.createEventTrigger, p, "POST", body, f.vals)
}

// trigBody is a well-formed create payload watching the lakehouse's landing
// folder and running the pipeline.
func (f *trigFixture) trigBody(prefix string) string {
	return `{"displayName":"on-landing","eventType":"` + store.EventFileCreated + `",
		"source":{"itemId":"` + f.lake.ID + `","pathPrefix":"` + prefix + `"},
		"action":{"itemId":"` + f.pipe.ID + `","jobType":"Pipeline"}}`
}

// write puts a file into the lakehouse through the store, exactly as any
// OneLake client would.
func (f *trigFixture) write(t *testing.T, rel, content string) {
	t.Helper()
	err := f.st.CreateOneLakePath(&store.OneLakePath{
		WorkspaceID: f.ws.ID, ItemID: f.lake.ID, RelPath: rel, Content: []byte(content)}, false)
	if err != nil {
		t.Fatal(err)
	}
}

func (f *trigFixture) pipelineRuns(t *testing.T) []*store.JobInstance {
	t.Helper()
	jobs, err := f.st.ListItemJobInstances(f.pipe.ID)
	if err != nil {
		t.Fatal(err)
	}
	return jobs
}

func TestEventTriggerCRUD(t *testing.T) {
	f := newTrigFixture(t, waitDef)

	w := f.createTrigger(admin, f.trigBody("Files/landing"))
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body.Bytes())
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	id, _ := body["id"].(string)
	if id == "" || body["enabled"] != true || body["eventType"] != store.EventFileCreated {
		t.Fatalf("trigger shape: %v", body)
	}
	src, _ := body["source"].(map[string]any)
	act, _ := body["action"].(map[string]any)
	if src["itemId"] != f.lake.ID || src["pathPrefix"] != "Files/landing" {
		t.Fatalf("source = %v", src)
	}
	// The action's workspace defaults to the Reflex's own.
	if act["itemId"] != f.pipe.ID || act["jobType"] != "Pipeline" || act["workspaceId"] != f.ws.ID {
		t.Fatalf("action = %v", act)
	}

	tid := map[string]string{"wid": f.ws.ID, "iid": f.reflex.ID, "tid": id}
	if w := do(f.a.getEventTrigger, viewer, "GET", "", tid); w.Code != http.StatusOK {
		t.Fatalf("get = %d %s", w.Code, w.Body.Bytes())
	}
	w = do(f.a.listEventTriggers, viewer, "GET", "", f.vals)
	var list struct{ Value []map[string]any }
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Value) != 1 || list.Value[0]["id"] != id {
		t.Fatalf("list = %v", list.Value)
	}

	// Patch leaves unspecified fields alone.
	if w := do(f.a.updateEventTrigger, admin, "PATCH", `{"enabled":false}`, tid); w.Code != http.StatusOK {
		t.Fatalf("patch = %d %s", w.Code, w.Body.Bytes())
	}
	got, _ := f.st.GetEventTrigger(f.reflex.ID, id)
	if got.Enabled || got.SourceItemID != f.lake.ID || got.EventType != store.EventFileCreated {
		t.Fatalf("patch clobbered fields: %+v", got)
	}

	if w := do(f.a.deleteEventTrigger, admin, "DELETE", "", tid); w.Code != http.StatusOK {
		t.Fatalf("delete = %d", w.Code)
	}
	if w := do(f.a.getEventTrigger, admin, "GET", "", tid); w.Code != http.StatusNotFound {
		t.Fatalf("get after delete = %d", w.Code)
	}
	if w := do(f.a.deleteEventTrigger, admin, "DELETE", "", tid); w.Code != http.StatusNotFound {
		t.Fatalf("double delete = %d", w.Code)
	}
	if w := do(f.a.updateEventTrigger, admin, "PATCH", `{"enabled":true}`, tid); w.Code != http.StatusNotFound {
		t.Fatalf("patch after delete = %d", w.Code)
	}
}

func TestEventTriggerValidationAndRBAC(t *testing.T) {
	f := newTrigFixture(t, waitDef)

	// The Reflex must exist and actually be a Reflex — a trigger hung off a
	// lakehouse would never be reachable in the portal either.
	notReflex := map[string]string{"wid": f.ws.ID, "iid": f.lake.ID}
	if w := do(f.a.createEventTrigger, admin, "POST", f.trigBody(""), notReflex); w.Code != http.StatusNotFound {
		t.Fatalf("trigger on a non-Reflex = %d", w.Code)
	}
	missing := map[string]string{"wid": f.ws.ID, "iid": "nope"}
	if w := do(f.a.listEventTriggers, admin, "GET", "", missing); w.Code != http.StatusNotFound {
		t.Fatalf("unknown reflex = %d", w.Code)
	}
	if w := do(f.a.listEventTriggers, admin, "GET", "", map[string]string{"wid": "nope", "iid": f.reflex.ID}); w.Code != http.StatusNotFound {
		t.Fatalf("unknown workspace = %d", w.Code)
	}

	// RBAC: reads need Viewer, writes Contributor.
	if w := f.createTrigger(viewer, f.trigBody("")); w.Code != http.StatusForbidden {
		t.Fatalf("viewer create = %d", w.Code)
	}
	if w := do(f.a.listEventTriggers, nobody, "GET", "", f.vals); w.Code != http.StatusForbidden {
		t.Fatalf("ungranted list = %d", w.Code)
	}

	// Payload validation. Each of these would otherwise produce a trigger that
	// can never fire, or fires at something that does not exist.
	for name, body := range map[string]string{
		"malformed":     `{`,
		"no eventType":  `{"source":{"itemId":"` + f.lake.ID + `"},"action":{"itemId":"` + f.pipe.ID + `","jobType":"Pipeline"}}`,
		"bad eventType": `{"eventType":"Microsoft.Fabric.OneLake.FileSneezed","source":{"itemId":"` + f.lake.ID + `"},"action":{"itemId":"` + f.pipe.ID + `","jobType":"Pipeline"}}`,
		"no source":     `{"eventType":"` + store.EventFileCreated + `","action":{"itemId":"` + f.pipe.ID + `","jobType":"Pipeline"}}`,
		"no action":     `{"eventType":"` + store.EventFileCreated + `","source":{"itemId":"` + f.lake.ID + `"}}`,
		"no jobType":    `{"eventType":"` + store.EventFileCreated + `","source":{"itemId":"` + f.lake.ID + `"},"action":{"itemId":"` + f.pipe.ID + `"}}`,
		"unknown target": `{"eventType":"` + store.EventFileCreated + `","source":{"itemId":"` + f.lake.ID + `"},
			"action":{"itemId":"00000000-0000-0000-0000-000000000000","jobType":"Pipeline"}}`,
	} {
		if w := f.createTrigger(admin, body); w.Code != http.StatusBadRequest {
			t.Fatalf("%s accepted: %d %s", name, w.Code, w.Body.Bytes())
		}
	}
	if n, _ := f.st.ListEventTriggers(f.reflex.ID); len(n) != 0 {
		t.Fatalf("%d invalid triggers stored", len(n))
	}
	// The same validation guards PATCH.
	id := f.mustTrigger(t, f.trigBody(""))
	tid := map[string]string{"wid": f.ws.ID, "iid": f.reflex.ID, "tid": id}
	if w := do(f.a.updateEventTrigger, admin, "PATCH", `{"eventType":"nope"}`, tid); w.Code != http.StatusBadRequest {
		t.Fatalf("patch to a bad eventType = %d", w.Code)
	}
	if w := do(f.a.updateEventTrigger, admin, "PATCH", `{`, tid); w.Code != http.StatusBadRequest {
		t.Fatalf("patch malformed = %d", w.Code)
	}
}

// mustTrigger creates a trigger and returns its id.
func (f *trigFixture) mustTrigger(t *testing.T, body string) string {
	t.Helper()
	w := f.createTrigger(admin, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create trigger = %d %s", w.Code, w.Body.Bytes())
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	return got["id"].(string)
}

func TestOneLakeWriteFiresTheTriggeredPipeline(t *testing.T) {
	f := newTrigFixture(t, waitDef)
	f.mustTrigger(t, f.trigBody("Files/landing"))

	// A write anywhere else is not this trigger's business.
	f.write(t, "Files/other/ignored.csv", "x")
	if n := len(f.pipelineRuns(t)); n != 0 {
		t.Fatalf("a write outside the prefix started %d runs", n)
	}
	// A write under the watched prefix runs the pipeline for real.
	f.write(t, "Files/landing/orders.csv", "id\n1\n")
	runs := f.pipelineRuns(t)
	if len(runs) != 1 {
		t.Fatalf("watched write started %d runs, want 1", len(runs))
	}
	if runs[0].InvokeType != store.InvokeEventTriggered {
		t.Fatalf("invokeType = %q", runs[0].InvokeType)
	}
	status, _, err := f.st.GetPipelineRun(runs[0].ID)
	if err != nil || status != "Succeeded" {
		t.Fatalf("the triggered pipeline did not run: %s %v", status, err)
	}
}

func TestTriggerFiltersOnEventTypeAndSource(t *testing.T) {
	f := newTrigFixture(t, waitDef)
	f.mustTrigger(t, f.trigBody(""))

	// A different item's storage: same path, wrong source.
	other := &store.Item{WorkspaceID: f.ws.ID, DisplayName: "silver", Type: "Lakehouse"}
	if err := f.st.CreateItem(other, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.st.CreateOneLakePath(&store.OneLakePath{
		WorkspaceID: f.ws.ID, ItemID: other.ID, RelPath: "Files/x.csv", Content: []byte("x")}, false); err != nil {
		t.Fatal(err)
	}
	if n := len(f.pipelineRuns(t)); n != 0 {
		t.Fatalf("another item's write started %d runs", n)
	}

	// A directory create is not a file event.
	if err := f.st.CreateOneLakePath(&store.OneLakePath{
		WorkspaceID: f.ws.ID, ItemID: f.lake.ID, RelPath: "Files/folder", IsDir: true, Content: []byte{}}, false); err != nil {
		t.Fatal(err)
	}
	if n := len(f.pipelineRuns(t)); n != 0 {
		t.Fatalf("a directory create started %d runs", n)
	}

	// A delete is a different event type, so this FileCreated trigger ignores it.
	f.write(t, "Files/gone.csv", "x")
	before := len(f.pipelineRuns(t))
	if err := f.st.DeleteOneLakePath(f.lake.ID, "Files/gone.csv"); err != nil {
		t.Fatal(err)
	}
	if got := len(f.pipelineRuns(t)); got != before {
		t.Fatalf("a delete fired a FileCreated trigger (%d → %d)", before, got)
	}
}

func TestDisabledTriggerDoesNotFire(t *testing.T) {
	f := newTrigFixture(t, waitDef)
	id := f.mustTrigger(t, f.trigBody(""))
	tid := map[string]string{"wid": f.ws.ID, "iid": f.reflex.ID, "tid": id}
	if w := do(f.a.updateEventTrigger, admin, "PATCH", `{"enabled":false}`, tid); w.Code != http.StatusOK {
		t.Fatalf("disable = %d", w.Code)
	}
	f.write(t, "Files/a.csv", "x")
	if n := len(f.pipelineRuns(t)); n != 0 {
		t.Fatalf("a disabled trigger started %d runs", n)
	}
	// Re-enabled, the next write fires it.
	if w := do(f.a.updateEventTrigger, admin, "PATCH", `{"enabled":true}`, tid); w.Code != http.StatusOK {
		t.Fatalf("re-enable = %d", w.Code)
	}
	f.write(t, "Files/b.csv", "x")
	if n := len(f.pipelineRuns(t)); n != 1 {
		t.Fatalf("re-enabled trigger started %d runs, want 1", n)
	}
}

func TestTriggerEventIsReadableFromTheDefinition(t *testing.T) {
	// The point of the whole feature: the pipeline can see which file arrived,
	// through Fabric's own safe-navigating expression.
	def := `{"properties":{"activities":[
		{"name":"Capture","type":"SetVariable","typeProperties":{
			"variableName":"seen",
			"value":"@concat(pipeline()?.TriggerEvent?.FolderPath,'/',pipeline()?.TriggerEvent?.FileName)"}}],
		"variables":{"seen":{"type":"String"}}}}`
	f := newTrigFixture(t, def)
	f.mustTrigger(t, f.trigBody("Files/landing"))

	f.write(t, "Files/landing/2024/orders.csv", "id\n1\n")
	runs := f.pipelineRuns(t)
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	status, detail, err := f.st.GetPipelineRun(runs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if status != "Succeeded" {
		t.Fatalf("status = %s: %s", status, detail)
	}
	if !strings.Contains(detail, "Files/landing/2024/orders.csv") {
		t.Fatalf("TriggerEvent did not reach the definition: %s", detail)
	}
}

func TestTriggerEventIsNullForAManualRun(t *testing.T) {
	// Safe navigation is what makes one definition work for both: started by
	// hand, `@pipeline()?.TriggerEvent?.FileName` is null rather than an error.
	def := `{"properties":{"activities":[
		{"name":"Capture","type":"SetVariable","typeProperties":{
			"variableName":"seen","value":"@pipeline()?.TriggerEvent?.FileName"}}],
		"variables":{"seen":{"type":"String"}}}}`
	f := newTrigFixture(t, def)
	j, err := f.a.startJob(f.ws.ID, f.pipe, "Pipeline", store.InvokeManual, nil)
	if err != nil {
		t.Fatal(err)
	}
	status, detail, err := f.st.GetPipelineRun(j.ID)
	if err != nil || status != "Succeeded" {
		t.Fatalf("a manual run of a trigger-aware pipeline failed: %s %s %v", status, detail, err)
	}
}

func TestTriggerCycleIsCut(t *testing.T) {
	// A pipeline whose own Copy writes back into the folder that triggered it
	// would recurse forever. The activation COUNT bounds it: the cycle climbs
	// to maxTriggerActivations and is cut there.
	f := newTrigFixture(t, waitDef)
	id := f.mustTrigger(t, f.trigBody("Files/landing"))
	ev := store.FileEvent{Type: store.EventFileCreated, WorkspaceID: f.ws.ID,
		ItemID: f.lake.ID, RelPath: "Files/landing/a.csv"}

	// Fill the budget, as a runaway chain would.
	for i := 0; i < maxTriggerActivations; i++ {
		if !f.a.firing.enter(id) {
			t.Fatalf("activation %d of %d was refused early", i+1, maxTriggerActivations)
		}
	}
	if n := f.a.DispatchFileEvent(ev); n != 0 {
		t.Fatalf("a trigger at its activation cap fired %d times; the cycle is not cut", n)
	}

	// One step back under the cap and it runs again — the bound is a ceiling,
	// not a latch.
	f.a.firing.leave(id)
	if n := f.a.DispatchFileEvent(ev); n != 1 {
		t.Fatalf("dispatch below the cap = %d, want 1", n)
	}
	// And ONE leave releases exactly ONE slot. If it released them all — a
	// delete rather than a decrement — the budget would reset on the first
	// unwind and a cycle would never reach the cap again.
	if !f.a.firing.enter(id) {
		t.Fatal("the slot freed by one leave could not be reclaimed")
	}
	if f.a.firing.enter(id) {
		t.Fatal("one leave released more than one slot; a cycle would run " +
			"forever because the budget resets every time a frame unwinds")
	}
	f.a.firing.leave(id)
	for i := 0; i < maxTriggerActivations; i++ {
		f.a.firing.leave(id)
	}
	if got := len(f.a.firing.on); got != 0 {
		t.Errorf("firing set leaked %d entr(ies) after unwinding", got)
	}
}

// TestIndependentEventsBothFire pins the behaviour real Fabric has and this
// emulator used to lack.
//
// Activator "continues monitoring without waiting for the action to complete",
// which is what "enables scalable workflows that can process many events
// simultaneously" — two files landing in one watched folder are two events and
// two activations. The old identity-based firing SET suppressed the second,
// running the pipeline once, and that suppression was an artifact of this
// emulator's synchronous dispatch rather than anything Fabric does.
func TestIndependentEventsBothFire(t *testing.T) {
	f := newTrigFixture(t, waitDef)
	id := f.mustTrigger(t, f.trigBody("Files/landing"))

	first := store.FileEvent{Type: store.EventFileCreated, WorkspaceID: f.ws.ID,
		ItemID: f.lake.ID, RelPath: "Files/landing/a.csv"}
	second := store.FileEvent{Type: store.EventFileCreated, WorkspaceID: f.ws.ID,
		ItemID: f.lake.ID, RelPath: "Files/landing/b.csv"}

	// The first activation is still in flight when the second event arrives —
	// exactly the state two simultaneous writes produce.
	if !f.a.firing.enter(id) {
		t.Fatal("the first activation was refused")
	}
	if n := f.a.DispatchFileEvent(second); n != 1 {
		t.Fatalf("a second, independent event fired %d times; want 1 — Fabric "+
			"does not wait for the first action to complete", n)
	}
	f.a.firing.leave(id)

	// And the ordinary case still works.
	if n := f.a.DispatchFileEvent(first); n != 1 {
		t.Fatalf("dispatch = %d, want 1", n)
	}
}

func TestDispatchSurvivesADeletedTarget(t *testing.T) {
	f := newTrigFixture(t, waitDef)
	f.mustTrigger(t, f.trigBody(""))
	if err := f.st.DeleteItem(f.ws.ID, f.pipe.ID); err != nil {
		t.Fatal(err)
	}
	ev := store.FileEvent{Type: store.EventFileCreated, WorkspaceID: f.ws.ID,
		ItemID: f.lake.ID, RelPath: "Files/a.csv"}
	if n := f.a.DispatchFileEvent(ev); n != 0 {
		t.Fatalf("dispatch to a deleted target started %d jobs", n)
	}
}

func TestRenameEmitsItsOwnEventType(t *testing.T) {
	f := newTrigFixture(t, waitDef)
	body := strings.Replace(f.trigBody(""), store.EventFileCreated, store.EventFileRenamed, 1)
	f.mustTrigger(t, body)

	f.write(t, "Files/tmp.csv", "x")
	if n := len(f.pipelineRuns(t)); n != 0 {
		t.Fatalf("a create fired the rename trigger (%d)", n)
	}
	if err := f.st.RenameOneLakePath(f.lake.ID, "Files/tmp.csv", "Files/final.csv"); err != nil {
		t.Fatal(err)
	}
	runs := f.pipelineRuns(t)
	if len(runs) != 1 {
		t.Fatalf("rename started %d runs, want 1", len(runs))
	}
}

func TestTriggerEventParamsShape(t *testing.T) {
	got := triggerEventParams(store.FileEvent{
		Type: store.EventFileCreated, WorkspaceID: "w", ItemID: "i", RelPath: "Files/landing/a.csv"})
	for k, want := range map[string]string{
		"EventType": store.EventFileCreated, "Source": "i", "Subject": "Files/landing/a.csv",
		"FileName": "a.csv", "FolderPath": "Files/landing", "WorkspaceId": "w", "ItemId": "i",
	} {
		if got[k] != want {
			t.Fatalf("%s = %v, want %q", k, got[k], want)
		}
	}
	// A file at the item root has no folder, rather than the "." path/filepath
	// would give it.
	if got := triggerEventParams(store.FileEvent{RelPath: "a.csv"}); got["FolderPath"] != "" {
		t.Fatalf("root FolderPath = %v", got["FolderPath"])
	}
}
