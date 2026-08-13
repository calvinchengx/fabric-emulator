package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func postJob(a *API, p *auth.Principal, wid, iid, jobType string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/x?jobType="+jobType, strings.NewReader("{}"))
	r.SetPathValue("wid", wid)
	r.SetPathValue("iid", iid)
	w := httptest.NewRecorder()
	a.createJobInstance(w, r, p)
	return w
}

func seedJobItem(t *testing.T, st *store.Store, ws *store.Workspace) *store.Item {
	t.Helper()
	it := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lh"}
	if err := st.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	return it
}

func seedCapacityWorkspace(t *testing.T, st *store.Store) *store.Workspace {
	t.Helper()
	ws := &store.Workspace{DisplayName: "w", CapacityID: store.DefaultCapacityID}
	if err := st.CreateWorkspace(ws, store.Principal{ID: admin.ID, Type: admin.Type}); err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestManualSubmitIsThrottledWhenCapacityIsFull(t *testing.T) {
	a, st := newAPI(t)
	a.LRODelaySeconds = 60
	a.RetryAfterSeconds = 7
	if err := st.SetCapacityMaxConcurrentJobs(store.DefaultCapacityID, 1); err != nil {
		t.Fatal(err)
	}
	ws := seedCapacityWorkspace(t, st)
	it := seedJobItem(t, st, ws)

	first := postJob(a, admin, ws.ID, it.ID, "DefaultJob")
	if first.Code != http.StatusAccepted {
		t.Fatalf("first job = %d %s; want 202", first.Code, first.Body.Bytes())
	}

	second := postJob(a, admin, ws.ID, it.ID, "DefaultJob")
	if second.Code != 430 {
		t.Fatalf("second job = %d %s; want 430", second.Code, second.Body.Bytes())
	}
	if errorCode(t, second) != "CapacityNotAvailable" {
		t.Fatalf("error = %s", errorCode(t, second))
	}
	if second.Header().Get("Retry-After") != "7" {
		t.Fatalf("Retry-After = %q; want 7", second.Header().Get("Retry-After"))
	}

	// A client that ignores Retry-After and retries immediately is refused again.
	again := postJob(a, admin, ws.ID, it.ID, "DefaultJob")
	if again.Code != 430 {
		t.Fatalf("retry without waiting = %d; want 430", again.Code)
	}

	// After the running job completes, the same submit is admitted.
	st.Clock.Advance(60)
	third := postJob(a, admin, ws.ID, it.ID, "DefaultJob")
	if third.Code != http.StatusAccepted {
		t.Fatalf("retry after slot freed = %d %s; want 202", third.Code, third.Body.Bytes())
	}
}

func TestScheduledJobQueuesThenRunsWhenASlotFrees(t *testing.T) {
	a, st := newAPI(t)
	a.LRODelaySeconds = 60
	if err := st.SetCapacityMaxConcurrentJobs(store.DefaultCapacityID, 1); err != nil {
		t.Fatal(err)
	}
	ws := seedCapacityWorkspace(t, st)
	aItem := seedJobItem(t, st, ws)
	bItem := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lh-b"}
	if err := st.CreateItem(bItem, nil); err != nil {
		t.Fatal(err)
	}

	first := postJob(a, admin, ws.ID, aItem.ID, "DefaultJob")
	if first.Code != http.StatusAccepted {
		t.Fatalf("first = %d", first.Code)
	}

	queued, err := a.startJob(ws.ID, bItem, "DefaultJob", store.InvokeScheduled, nil)
	if err != nil {
		t.Fatal(err)
	}
	if queued.StatusAt(st.Now()) != store.JobQueued {
		t.Fatalf("scheduled status = %s; want Queued", queued.StatusAt(st.Now()))
	}

	st.Clock.Advance(60)
	if n := a.DrainCapacityQueues(); n != 1 {
		t.Fatalf("admitted %d; want 1", n)
	}
	got, err := st.GetJobInstance(bItem.ID, queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.StatusAt(st.Now()) != store.JobInProgress && got.StatusAt(st.Now()) != store.JobNotStarted {
		t.Fatalf("after drain status = %s", got.StatusAt(st.Now()))
	}
}

func TestTwoJobsOnTheSameItemBothRunWhenCapacityAllows(t *testing.T) {
	// docs/36: do not serialise same-item jobs. Capacity 2 must admit two
	// Manual runs of one lakehouse, which is the collision Fabric allows.
	a, st := newAPI(t)
	a.LRODelaySeconds = 60
	if err := st.SetCapacityMaxConcurrentJobs(store.DefaultCapacityID, 2); err != nil {
		t.Fatal(err)
	}
	ws := seedCapacityWorkspace(t, st)
	it := seedJobItem(t, st, ws)

	if w := postJob(a, admin, ws.ID, it.ID, "DefaultJob"); w.Code != http.StatusAccepted {
		t.Fatalf("first = %d", w.Code)
	}
	if w := postJob(a, admin, ws.ID, it.ID, "DefaultJob"); w.Code != http.StatusAccepted {
		t.Fatalf("second same item = %d %s; capacity must not serialise by item", w.Code, w.Body.Bytes())
	}
}

func TestEventTriggeredJobQueuesWhenCapacityIsFull(t *testing.T) {
	a, st := newAPI(t)
	a.LRODelaySeconds = 60
	if err := st.SetCapacityMaxConcurrentJobs(store.DefaultCapacityID, 1); err != nil {
		t.Fatal(err)
	}
	ws := seedCapacityWorkspace(t, st)
	it := seedJobItem(t, st, ws)

	if w := postJob(a, admin, ws.ID, it.ID, "DefaultJob"); w.Code != http.StatusAccepted {
		t.Fatalf("manual = %d", w.Code)
	}
	queued, err := a.startJob(ws.ID, it, "DefaultJob", store.InvokeEventTriggered, nil)
	if err != nil {
		t.Fatal(err)
	}
	if queued.StatusAt(st.Now()) != store.JobQueued {
		t.Fatalf("event-triggered status = %s; want Queued", queued.StatusAt(st.Now()))
	}
}

func TestQueuedJobsAdmitOldestFirst(t *testing.T) {
	a, st := newAPI(t)
	a.LRODelaySeconds = 60
	if err := st.SetCapacityMaxConcurrentJobs(store.DefaultCapacityID, 1); err != nil {
		t.Fatal(err)
	}
	ws := seedCapacityWorkspace(t, st)
	firstItem := seedJobItem(t, st, ws)
	olderItem := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lh-older"}
	newerItem := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lh-newer"}
	if err := st.CreateItem(olderItem, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateItem(newerItem, nil); err != nil {
		t.Fatal(err)
	}

	if w := postJob(a, admin, ws.ID, firstItem.ID, "DefaultJob"); w.Code != http.StatusAccepted {
		t.Fatalf("occupying job = %d", w.Code)
	}
	older, err := a.startJob(ws.ID, olderItem, "DefaultJob", store.InvokeScheduled, nil)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := a.startJob(ws.ID, newerItem, "DefaultJob", store.InvokeScheduled, nil)
	if err != nil {
		t.Fatal(err)
	}

	st.Clock.Advance(60)
	if n := a.DrainCapacityQueues(); n != 1 {
		t.Fatalf("first drain admitted %d; want 1", n)
	}
	gotOlder, err := st.GetJobInstance(olderItem.ID, older.ID)
	if err != nil {
		t.Fatal(err)
	}
	gotNewer, err := st.GetJobInstance(newerItem.ID, newer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotOlder.StatusAt(st.Now()) == store.JobQueued {
		t.Fatal("oldest queued job was not admitted first")
	}
	if gotNewer.StatusAt(st.Now()) != store.JobQueued {
		t.Fatalf("newer job status = %s; want still Queued", gotNewer.StatusAt(st.Now()))
	}
}
