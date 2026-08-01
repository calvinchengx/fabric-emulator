package api

// The native Job Scheduler API: contract, validation, the documented limit,
// and — the part that makes it more than a CRUD table — schedules that really
// start job instances when the controllable clock reaches them.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// schedFixture is a workspace + item ready to carry schedules, with the path
// values the handlers read.
type schedFixture struct {
	a    *API
	st   *store.Store
	ws   *store.Workspace
	item *store.Item
	vals map[string]string
}

func newSchedFixture(t *testing.T) *schedFixture {
	t.Helper()
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	it := &store.Item{WorkspaceID: ws.ID, DisplayName: "nb", Type: "Notebook"}
	if err := st.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	// A frozen clock: every assertion below is about virtual time, and a
	// second of wall clock creeping in would make firing counts flaky.
	st.Clock.Freeze()
	return &schedFixture{a: a, st: st, ws: ws, item: it,
		vals: map[string]string{"wid": ws.ID, "iid": it.ID}}
}

// cfg builds a Cron ScheduleConfig relative to the fixture's frozen clock:
// startOffset seconds from now, running for a day.
func (f *schedFixture) cfg(startOffset int64, intervalMin int) string {
	now := f.st.Now()
	start := time.Unix(now+startOffset, 0).UTC().Format(time.RFC3339)
	end := time.Unix(now+86400, 0).UTC().Format(time.RFC3339)
	return fmt.Sprintf(`{"type":"Cron","interval":%d,"startDateTime":%q,"endDateTime":%q,"localTimeZoneId":"UTC"}`,
		intervalMin, start, end)
}

// create POSTs a schedule and returns the recorder.
func (f *schedFixture) create(p *auth.Principal, body string) *httptest.ResponseRecorder {
	return doSched(f.a.createItemSchedule, p, "POST", body, f.vals, "RunNotebook", "")
}

// doSched invokes a schedule handler, which takes the jobType and schedule id
// from the subtree dispatcher rather than from mux path values.
func doSched(h func(http.ResponseWriter, *http.Request, *auth.Principal, string, string),
	p *auth.Principal, method, body string, pathVals map[string]string, jobType, sid string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/x", strings.NewReader(body))
	for k, v := range pathVals {
		r.SetPathValue(k, v)
	}
	w := httptest.NewRecorder()
	h(w, r, p, jobType, sid)
	return w
}

func decodeSchedule(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("not JSON: %s", w.Body.Bytes())
	}
	return body
}

// jobsFor counts an item's job instances, optionally filtered by invokeType.
func (f *schedFixture) jobsFor(t *testing.T, invokeType string) []*store.JobInstance {
	t.Helper()
	all, err := f.st.ListItemJobInstances(f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if invokeType == "" {
		return all
	}
	var out []*store.JobInstance
	for _, j := range all {
		if j.InvokeType == invokeType {
			out = append(out, j)
		}
	}
	return out
}

func TestScheduleCRUDRoundTrip(t *testing.T) {
	f := newSchedFixture(t)
	// Start an hour out so creation does not immediately fire anything.
	cfg := f.cfg(3600, 60)

	w := f.create(admin, `{"enabled":true,"configuration":`+cfg+`,"executionData":{"parameters":{"k":"v"}}}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body.Bytes())
	}
	body := decodeSchedule(t, w)
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("no id: %v", body)
	}
	if body["enabled"] != true {
		t.Fatalf("enabled = %v", body["enabled"])
	}
	// The owner is the calling principal, in the documented shape.
	owner, _ := body["owner"].(map[string]any)
	if owner["id"] != admin.ID || owner["type"] != "User" {
		t.Fatalf("owner = %v", owner)
	}
	if _, ok := body["createdDateTime"].(string); !ok {
		t.Fatalf("no createdDateTime: %v", body)
	}
	// The configuration comes back as sent, not re-serialised into some
	// normalised shape a client would not recognise.
	gotCfg, err := json.Marshal(body["configuration"])
	if err != nil {
		t.Fatal(err)
	}
	var sent, got map[string]any
	_ = json.Unmarshal([]byte(cfg), &sent)
	_ = json.Unmarshal(gotCfg, &got)
	if fmt.Sprint(sent) != fmt.Sprint(got) {
		t.Fatalf("configuration round-trip:\n got %v\nwant %v", got, sent)
	}
	if body["executionData"] == nil {
		t.Fatalf("executionData dropped: %v", body)
	}

	// Get.
	w = doSched(f.a.getItemSchedule, viewer, "GET", "", f.vals, "RunNotebook", id)
	if w.Code != http.StatusOK || decodeSchedule(t, w)["id"] != id {
		t.Fatalf("get = %d %s", w.Code, w.Body.Bytes())
	}

	// List.
	w = doSched(f.a.listItemSchedules, viewer, "GET", "", f.vals, "RunNotebook", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d %s", w.Code, w.Body.Bytes())
	}
	var list struct{ Value []map[string]any }
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Value) != 1 || list.Value[0]["id"] != id {
		t.Fatalf("list = %v", list.Value)
	}

	// A schedule is scoped to its job type: the same id under another job type
	// is not found, rather than leaking across.
	if w := doSched(f.a.getItemSchedule, viewer, "GET", "", f.vals, "sparkjob", id); w.Code != http.StatusNotFound {
		t.Fatalf("cross-jobType get = %d", w.Code)
	}
	w = doSched(f.a.listItemSchedules, viewer, "GET", "", f.vals, "sparkjob", "")
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Value) != 0 {
		t.Fatalf("cross-jobType list = %v", list.Value)
	}

	// Patch: disable it.
	w = doSched(f.a.updateItemSchedule, admin, "PATCH", `{"enabled":false,"configuration":`+cfg+`}`, f.vals, "RunNotebook", id)
	if w.Code != http.StatusOK || decodeSchedule(t, w)["enabled"] != false {
		t.Fatalf("patch = %d %s", w.Code, w.Body.Bytes())
	}

	// Delete, then the follow-up read 404s.
	if w := doSched(f.a.deleteItemSchedule, admin, "DELETE", "", f.vals, "RunNotebook", id); w.Code != http.StatusOK {
		t.Fatalf("delete = %d", w.Code)
	}
	if w := doSched(f.a.getItemSchedule, admin, "GET", "", f.vals, "RunNotebook", id); w.Code != http.StatusNotFound {
		t.Fatalf("get after delete = %d", w.Code)
	}
	if w := doSched(f.a.deleteItemSchedule, admin, "DELETE", "", f.vals, "RunNotebook", id); w.Code != http.StatusNotFound {
		t.Fatalf("double delete = %d", w.Code)
	}
}

func TestScheduleRBACAndTargetErrors(t *testing.T) {
	f := newSchedFixture(t)
	cfg := f.cfg(3600, 60)

	// Reads need Viewer, writes need Contributor.
	if w := f.create(viewer, `{"configuration":`+cfg+`}`); w.Code != http.StatusForbidden {
		t.Fatalf("viewer create = %d", w.Code)
	}
	if w := f.create(nobody, `{"configuration":`+cfg+`}`); w.Code != http.StatusForbidden {
		t.Fatalf("ungranted create = %d", w.Code)
	}
	if w := doSched(f.a.listItemSchedules, nobody, "GET", "", f.vals, "RunNotebook", ""); w.Code != http.StatusForbidden {
		t.Fatalf("ungranted list = %d", w.Code)
	}
	created := f.create(admin, `{"configuration":`+cfg+`}`)
	id := decodeSchedule(t, created)["id"].(string)
	if w := doSched(f.a.updateItemSchedule, viewer, "PATCH", `{"configuration":`+cfg+`}`, f.vals, "RunNotebook", id); w.Code != http.StatusForbidden {
		t.Fatalf("viewer patch = %d", w.Code)
	}
	if w := doSched(f.a.deleteItemSchedule, viewer, "DELETE", "", f.vals, "RunNotebook", id); w.Code != http.StatusForbidden {
		t.Fatalf("viewer delete = %d", w.Code)
	}

	// Unknown workspace and unknown item.
	bad := map[string]string{"wid": "no-such-ws", "iid": f.item.ID}
	if w := doSched(f.a.listItemSchedules, admin, "GET", "", bad, "RunNotebook", ""); w.Code != http.StatusNotFound {
		t.Fatalf("unknown workspace = %d", w.Code)
	}
	bad = map[string]string{"wid": f.ws.ID, "iid": "no-such-item"}
	w := doSched(f.a.listItemSchedules, admin, "GET", "", bad, "RunNotebook", "")
	if w.Code != http.StatusNotFound || errorCode(t, w) != "ItemNotFound" {
		t.Fatalf("unknown item = %d %s", w.Code, w.Body.Bytes())
	}
	// …on every write path too.
	for name, h := range map[string]func(http.ResponseWriter, *http.Request, *auth.Principal, string, string){
		"create": f.a.createItemSchedule, "get": f.a.getItemSchedule,
		"update": f.a.updateItemSchedule, "delete": f.a.deleteItemSchedule,
	} {
		if w := doSched(h, admin, "POST", `{"configuration":`+cfg+`}`, bad, "RunNotebook", id); w.Code != http.StatusNotFound {
			t.Fatalf("%s on unknown item = %d", name, w.Code)
		}
	}

	// Patching or getting an unknown schedule id.
	if w := doSched(f.a.getItemSchedule, admin, "GET", "", f.vals, "RunNotebook", "no-such"); errorCode(t, w) != "ScheduleNotFound" {
		t.Fatalf("unknown schedule get = %s", w.Body.Bytes())
	}
	if w := doSched(f.a.updateItemSchedule, admin, "PATCH", `{"configuration":`+cfg+`}`, f.vals, "RunNotebook", "no-such"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown schedule patch = %d", w.Code)
	}
}

func TestScheduleRequestValidation(t *testing.T) {
	f := newSchedFixture(t)

	// Malformed body, missing configuration, and an invalid configuration all
	// fail at the boundary — an invalid schedule must never be stored, because
	// a stored one that cannot be parsed would silently never fire.
	if w := f.create(admin, `{`); w.Code != http.StatusBadRequest {
		t.Fatalf("malformed = %d", w.Code)
	}
	if w := f.create(admin, `{"enabled":true}`); w.Code != http.StatusBadRequest || errorCode(t, w) != "InvalidRequest" {
		t.Fatalf("no configuration = %d %s", w.Code, w.Body.Bytes())
	}
	if w := f.create(admin, ``); w.Code != http.StatusBadRequest {
		t.Fatalf("empty body = %d", w.Code)
	}
	bad := `{"configuration":{"type":"Cron","interval":0,"startDateTime":"2024-01-01T00:00:00Z","endDateTime":"2024-02-01T00:00:00Z","localTimeZoneId":"UTC"}}`
	w := f.create(admin, bad)
	if w.Code != http.StatusBadRequest || !strings.Contains(string(w.Body.Bytes()), "interval") {
		t.Fatalf("bad interval = %d %s", w.Code, w.Body.Bytes())
	}
	if n, _ := f.st.CountItemSchedules(f.item.ID, "RunNotebook"); n != 0 {
		t.Fatalf("%d invalid schedules were stored", n)
	}
	// The same validation guards PATCH.
	id := decodeSchedule(t, f.create(admin, `{"configuration":`+f.cfg(3600, 60)+`}`))["id"].(string)
	if w := doSched(f.a.updateItemSchedule, admin, "PATCH", bad, f.vals, "RunNotebook", id); w.Code != http.StatusBadRequest {
		t.Fatalf("patch with bad config = %d", w.Code)
	}
	if w := doSched(f.a.updateItemSchedule, admin, "PATCH", `{`, f.vals, "RunNotebook", id); w.Code != http.StatusBadRequest {
		t.Fatalf("patch malformed = %d", w.Code)
	}
}

func TestScheduleExceedsLimit(t *testing.T) {
	f := newSchedFixture(t)
	cfg := f.cfg(3600, 60)
	for i := 0; i < store.MaxSchedulesPerItem; i++ {
		if w := f.create(admin, `{"configuration":`+cfg+`}`); w.Code != http.StatusCreated {
			t.Fatalf("create %d = %d %s", i, w.Code, w.Body.Bytes())
		}
	}
	w := f.create(admin, `{"configuration":`+cfg+`}`)
	if w.Code != http.StatusBadRequest || errorCode(t, w) != "ScheduleExceedsLimit" {
		t.Fatalf("21st schedule = %d %s", w.Code, w.Body.Bytes())
	}
	// The limit is per job type, so a different job type is unaffected.
	if w := doSched(f.a.createItemSchedule, admin, "POST", `{"configuration":`+cfg+`}`, f.vals, "sparkjob", ""); w.Code != http.StatusCreated {
		t.Fatalf("other jobType = %d %s", w.Code, w.Body.Bytes())
	}
}

func TestPastStartDateTimeTriggersInstantly(t *testing.T) {
	f := newSchedFixture(t)
	// "If the start time is in the past, it will trigger a job instantly."
	w := f.create(admin, `{"configuration":`+f.cfg(-30, 60)+`}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body.Bytes())
	}
	jobs := f.jobsFor(t, store.InvokeScheduled)
	if len(jobs) != 1 {
		t.Fatalf("started %d jobs, want 1 instantly", len(jobs))
	}
	if jobs[0].JobType != "RunNotebook" {
		t.Fatalf("jobType = %q", jobs[0].JobType)
	}
	// A future start fires nothing yet.
	f2 := newSchedFixture(t)
	f2.create(admin, `{"configuration":`+f2.cfg(3600, 60)+`}`)
	if n := len(f2.jobsFor(t, "")); n != 0 {
		t.Fatalf("future schedule started %d jobs", n)
	}
}

func TestClockAdvanceFiresEachDueOccurrenceExactlyOnce(t *testing.T) {
	f := newSchedFixture(t)
	// Hourly, starting now: creation fires the occurrence at the start instant.
	f.create(admin, `{"configuration":`+f.cfg(0, 60)+`}`)
	if n := len(f.jobsFor(t, store.InvokeScheduled)); n != 1 {
		t.Fatalf("at creation: %d jobs, want 1", n)
	}
	// Three hours on: three more, not four (the start instant is not re-fired)
	// and not one (each hour is a separate run).
	f.st.Clock.Advance(3 * 3600)
	if got := f.a.TickSchedules(); got != 3 {
		t.Fatalf("tick started %d, want 3", got)
	}
	if n := len(f.jobsFor(t, store.InvokeScheduled)); n != 4 {
		t.Fatalf("after 3h: %d jobs, want 4", n)
	}
	// Ticking again without moving the clock is a no-op — the high-water mark
	// makes evaluation idempotent, which matters because reads trigger it.
	if got := f.a.TickSchedules(); got != 0 {
		t.Fatalf("repeat tick started %d, want 0", got)
	}
	if n := len(f.jobsFor(t, store.InvokeScheduled)); n != 4 {
		t.Fatalf("repeat tick changed the count to %d", n)
	}
}

func TestDisabledScheduleDoesNotFire(t *testing.T) {
	f := newSchedFixture(t)
	w := f.create(admin, `{"enabled":false,"configuration":`+f.cfg(0, 60)+`}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body.Bytes())
	}
	f.st.Clock.Advance(3 * 3600)
	if got := f.a.TickSchedules(); got != 0 {
		t.Fatalf("disabled schedule started %d jobs", got)
	}
	// Re-enabling starts the series from the schedule's own start, so the
	// runs missed while it was off are not backfilled beyond the cap.
	id := decodeSchedule(t, w)["id"].(string)
	body := `{"enabled":true,"configuration":` + f.cfg(0, 60) + `}`
	if w := doSched(f.a.updateItemSchedule, admin, "PATCH", body, f.vals, "RunNotebook", id); w.Code != http.StatusOK {
		t.Fatalf("re-enable = %d %s", w.Code, w.Body.Bytes())
	}
	if n := len(f.jobsFor(t, store.InvokeScheduled)); n == 0 {
		t.Fatal("re-enabled schedule fired nothing")
	}
}

func TestReplacingTheConfigurationRestartsTheSeries(t *testing.T) {
	f := newSchedFixture(t)
	w := f.create(admin, `{"configuration":`+f.cfg(0, 60)+`}`)
	id := decodeSchedule(t, w)["id"].(string)
	before := len(f.jobsFor(t, store.InvokeScheduled))

	// A new configuration is a new series: the old high-water mark refers to
	// occurrences that no longer exist, so it must not suppress the new ones.
	body := `{"enabled":true,"configuration":` + f.cfg(-120, 30) + `}`
	if w := doSched(f.a.updateItemSchedule, admin, "PATCH", body, f.vals, "RunNotebook", id); w.Code != http.StatusOK {
		t.Fatalf("patch = %d %s", w.Code, w.Body.Bytes())
	}
	if after := len(f.jobsFor(t, store.InvokeScheduled)); after <= before {
		t.Fatalf("new configuration fired nothing new (%d → %d)", before, after)
	}
	// Re-sending the *same* configuration does not restart it.
	same := len(f.jobsFor(t, store.InvokeScheduled))
	if w := doSched(f.a.updateItemSchedule, admin, "PATCH", body, f.vals, "RunNotebook", id); w.Code != http.StatusOK {
		t.Fatalf("re-patch = %d", w.Code)
	}
	if got := len(f.jobsFor(t, store.InvokeScheduled)); got != same {
		t.Fatalf("identical configuration re-fired: %d → %d", same, got)
	}
}

func TestScheduledPipelineReallyRuns(t *testing.T) {
	// The point of routing scheduled runs through startJob: a scheduled
	// DataPipeline executes the interpreter exactly as a manual run does, and
	// only invokeType tells the two apart.
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	st.Clock.Freeze()
	pipe := createPipeline(t, st, ws.ID,
		`{"properties":{"activities":[{"name":"Wait1","type":"Wait","typeProperties":{"waitTimeInSeconds":1}}]}}`)
	now := st.Now()
	cfg := fmt.Sprintf(`{"type":"Cron","interval":60,"startDateTime":%q,"endDateTime":%q,"localTimeZoneId":"UTC"}`,
		time.Unix(now-30, 0).UTC().Format(time.RFC3339), time.Unix(now+86400, 0).UTC().Format(time.RFC3339))
	vals := map[string]string{"wid": ws.ID, "iid": pipe.ID}
	w := doSched(a.createItemSchedule, admin, "POST", `{"configuration":`+cfg+`}`, vals, "Pipeline", "")
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body.Bytes())
	}
	jobs, err := st.ListItemJobInstances(pipe.ID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %v (%v)", jobs, err)
	}
	if jobs[0].InvokeType != store.InvokeScheduled {
		t.Fatalf("invokeType = %q", jobs[0].InvokeType)
	}
	// The interpreter ran and recorded its activity detail.
	status, runs, err := st.GetPipelineRun(jobs[0].ID)
	if err != nil {
		t.Fatalf("no pipeline run recorded: %v", err)
	}
	if status != "Succeeded" || !strings.Contains(runs, "Wait1") {
		t.Fatalf("run = %s %s", status, runs)
	}
}

func TestListItemJobInstances(t *testing.T) {
	f := newSchedFixture(t)
	// A schedule already past its start: listing the instances is itself an
	// evaluation point, so the run shows up without touching the clock API.
	f.create(admin, `{"configuration":`+f.cfg(-30, 60)+`}`)

	w := do(f.a.listJobInstances, viewer, "GET", "", f.vals)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d %s", w.Code, w.Body.Bytes())
	}
	var list struct{ Value []map[string]any }
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Value) != 1 {
		t.Fatalf("list = %v", list.Value)
	}
	got := list.Value[0]
	if got["itemId"] != f.item.ID || got["workspaceId"] != f.ws.ID {
		t.Fatalf("scoping wrong: %v", got)
	}
	if got["invokeType"] != store.InvokeScheduled {
		t.Fatalf("invokeType = %v", got["invokeType"])
	}
	if got["jobType"] != "RunNotebook" || got["status"] == nil {
		t.Fatalf("body = %v", got)
	}

	// Advancing the clock and listing again picks up the new runs — no
	// background worker, and none needed.
	f.st.Clock.Advance(2 * 3600)
	w = do(f.a.listJobInstances, viewer, "GET", "", f.vals)
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Value) != 3 {
		t.Fatalf("after 2h: %d instances, want 3", len(list.Value))
	}

	// RBAC and unknown targets.
	if w := do(f.a.listJobInstances, nobody, "GET", "", f.vals); w.Code != http.StatusForbidden {
		t.Fatalf("ungranted list = %d", w.Code)
	}
	bad := map[string]string{"wid": f.ws.ID, "iid": "no-such-item"}
	if w := do(f.a.listJobInstances, admin, "GET", "", bad); w.Code != http.StatusNotFound {
		t.Fatalf("unknown item = %d", w.Code)
	}
	if w := do(f.a.listJobInstances, admin, "GET", "", map[string]string{"wid": "nope", "iid": f.item.ID}); w.Code != http.StatusNotFound {
		t.Fatalf("unknown workspace = %d", w.Code)
	}
}

func TestJobsSubtreeDispatch(t *testing.T) {
	f := newSchedFixture(t)
	base := "/v1/workspaces/" + f.ws.ID + "/items/" + f.item.ID + "/jobs/"

	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, base+path, strings.NewReader(body))
		r.SetPathValue("wid", f.ws.ID)
		r.SetPathValue("iid", f.item.ID)
		w := httptest.NewRecorder()
		f.a.jobsSubtree(w, r, admin)
		return w
	}

	// A path the scheduler does not own is an honest 404, not a silent accept.
	for _, p := range []string{"RunNotebook", "RunNotebook/bogus", "RunNotebook/schedules/a/b", "instances/x/y"} {
		if w := call("GET", p, ""); w.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", p, w.Code)
		}
	}
	// The right shapes reach the right handlers.
	if w := call("POST", "RunNotebook/schedules", `{"configuration":`+f.cfg(3600, 60)+`}`); w.Code != http.StatusCreated {
		t.Fatalf("POST schedules = %d %s", w.Code, w.Body.Bytes())
	}
	w := call("GET", "RunNotebook/schedules", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET schedules = %d", w.Code)
	}
	var list struct{ Value []map[string]any }
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	id := list.Value[0]["id"].(string)
	if w := call("GET", "RunNotebook/schedules/"+id, ""); w.Code != http.StatusOK {
		t.Fatalf("GET one = %d", w.Code)
	}
	if w := call("PATCH", "RunNotebook/schedules/"+id, `{"configuration":`+f.cfg(3600, 30)+`}`); w.Code != http.StatusOK {
		t.Fatalf("PATCH = %d %s", w.Code, w.Body.Bytes())
	}
	if w := call("DELETE", "RunNotebook/schedules/"+id, ""); w.Code != http.StatusOK {
		t.Fatalf("DELETE = %d", w.Code)
	}
	// Wrong method for the shape → 405, distinguishing "no such route" from
	// "not that verb here".
	if w := call("POST", "RunNotebook/schedules/"+id, ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST on one = %d", w.Code)
	}
	if w := call("DELETE", "RunNotebook/schedules", ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE on collection = %d", w.Code)
	}
}

func TestTickIgnoresSchedulesWhoseItemIsGone(t *testing.T) {
	f := newSchedFixture(t)
	f.create(admin, `{"configuration":`+f.cfg(3600, 60)+`}`)
	// Deleting the item cascades the schedule away; a later tick must not
	// resurrect anything or panic on the missing item.
	if err := f.st.DeleteItem(f.ws.ID, f.item.ID); err != nil {
		t.Fatal(err)
	}
	f.st.Clock.Advance(7200)
	if got := f.a.TickSchedules(); got != 0 {
		t.Fatalf("tick after item delete started %d jobs", got)
	}
}
