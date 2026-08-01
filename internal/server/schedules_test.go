package server_test

// The native Job Scheduler API driven end to end: real signed JWTs, the real
// mux (including the hand-rolled `…/jobs/…` subtree dispatch), and real job
// instances materialising as the controllable clock reaches each occurrence.

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// scheduleFixture is a workspace + DataPipeline with a frozen clock, so every
// count below is about virtual time only.
type scheduleFixture struct {
	*fixture
	ws   string
	item string
	now  int64
}

func newScheduleFixture(t *testing.T) *scheduleFixture {
	f := newFixture(t)
	var ws struct{ ID string }
	f.mustStatus(f.call("POST", "/v1/workspaces", f.token,
		map[string]string{"displayName": "Sched"}, &ws), http.StatusCreated, "create workspace")

	// A pipeline that really executes, so a scheduled run proves more than a
	// row appearing in a table.
	var item struct{ ID string }
	body := map[string]any{"displayName": "nightly", "type": "DataPipeline"}
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token, body, &item),
		http.StatusCreated, "create pipeline")

	var ck struct{ Now int64 }
	f.mustStatus(f.call("POST", "/_emulator/clock", "", map[string]bool{"freeze": true}, &ck),
		http.StatusOK, "freeze")
	return &scheduleFixture{fixture: f, ws: ws.ID, item: item.ID, now: ck.Now}
}

func (f *scheduleFixture) schedulesURL() string {
	return "/v1/workspaces/" + f.ws + "/items/" + f.item + "/jobs/Pipeline/schedules"
}

// cronConfig is a Cron ScheduleConfig starting startOffset seconds from the
// frozen clock.
func (f *scheduleFixture) cronConfig(startOffset int64, intervalMin int) map[string]any {
	return map[string]any{
		"type":            "Cron",
		"interval":        intervalMin,
		"startDateTime":   time.Unix(f.now+startOffset, 0).UTC().Format(time.RFC3339),
		"endDateTime":     time.Unix(f.now+86400, 0).UTC().Format(time.RFC3339),
		"localTimeZoneId": "UTC",
	}
}

// instances lists the item's job instances through the public API.
func (f *scheduleFixture) instances() []map[string]any {
	f.t.Helper()
	var list struct{ Value []map[string]any }
	f.mustStatus(f.call("GET", "/v1/workspaces/"+f.ws+"/items/"+f.item+"/jobs/instances",
		f.token, nil, &list), http.StatusOK, "list job instances")
	return list.Value
}

func TestJobSchedulerAPIEndToEnd(t *testing.T) {
	f := newScheduleFixture(t)

	// Unauthenticated requests are rejected on the new routes too — the
	// subtree dispatcher sits behind the same bearer validation.
	f.mustStatus(f.call("GET", f.schedulesURL(), "", nil, nil), http.StatusUnauthorized, "no token")

	// Create: enabled by default, starting ten minutes out so nothing fires
	// yet. Every advance below is deliberately small — the emulator's clock
	// also ages the bearer token, and an hour of virtual time would expire it
	// mid-test.
	var sched struct {
		ID              string
		Enabled         bool
		CreatedDateTime string
		Owner           struct{ ID, Type string }
	}
	body := map[string]any{"configuration": f.cronConfig(600, 10)}
	f.mustStatus(f.call("POST", f.schedulesURL(), f.token, body, &sched), http.StatusCreated, "create schedule")
	if sched.ID == "" || !sched.Enabled || sched.CreatedDateTime == "" || sched.Owner.ID == "" {
		t.Fatalf("ItemSchedule shape: %+v", sched)
	}
	if n := len(f.instances()); n != 0 {
		t.Fatalf("a future schedule started %d jobs", n)
	}

	// Get + list through the real routes.
	one := f.schedulesURL() + "/" + sched.ID
	f.mustStatus(f.call("GET", one, f.token, nil, nil), http.StatusOK, "get schedule")
	var list struct{ Value []map[string]any }
	f.mustStatus(f.call("GET", f.schedulesURL(), f.token, nil, &list), http.StatusOK, "list schedules")
	if len(list.Value) != 1 || list.Value[0]["id"] != sched.ID {
		t.Fatalf("list = %+v", list.Value)
	}

	// Advancing the clock past the start fires it, and the response says how
	// many runs the move started.
	var ck struct {
		Now                  int64
		ScheduledJobsStarted int `json:"scheduledJobsStarted"`
	}
	f.mustStatus(f.call("POST", "/_emulator/clock", "", map[string]int64{"advance": 600}, &ck),
		http.StatusOK, "advance ten minutes")
	if ck.ScheduledJobsStarted != 1 {
		t.Fatalf("advance started %d jobs, want 1", ck.ScheduledJobsStarted)
	}
	runs := f.instances()
	if len(runs) != 1 {
		t.Fatalf("instances = %+v", runs)
	}
	if runs[0]["invokeType"] != "Scheduled" || runs[0]["jobType"] != "Pipeline" {
		t.Fatalf("job instance = %+v", runs[0])
	}
	// The scheduled run really executed the pipeline: its activity detail is
	// queryable exactly as a manually-started run's is.
	jid, _ := runs[0]["id"].(string)
	var detail struct {
		Value []struct{ ActivityName, Status string }
	}
	f.mustStatus(f.call("POST", "/v1/workspaces/"+f.ws+"/items/"+f.item+
		"/jobs/instances/"+jid+"/queryactivityruns", f.token, map[string]any{}, &detail),
		http.StatusOK, "queryactivityruns")

	// Twenty more minutes is two more runs — real periodicity, not one
	// catch-up run for the whole gap.
	f.mustStatus(f.call("POST", "/_emulator/clock", "", map[string]int64{"advance": 1200}, &ck),
		http.StatusOK, "advance twenty minutes")
	if ck.ScheduledJobsStarted != 2 {
		t.Fatalf("second advance started %d, want 2", ck.ScheduledJobsStarted)
	}
	if n := len(f.instances()); n != 3 {
		t.Fatalf("after 30 minutes: %d instances, want 3", n)
	}

	// Disabling stops it; the clock keeps moving and nothing new appears.
	upd := map[string]any{"enabled": false, "configuration": f.cronConfig(600, 10)}
	f.mustStatus(f.call("PATCH", one, f.token, upd, nil), http.StatusOK, "disable")
	f.mustStatus(f.call("POST", "/_emulator/clock", "", map[string]int64{"advance": 600}, &ck),
		http.StatusOK, "advance while disabled")
	if ck.ScheduledJobsStarted != 0 {
		t.Fatalf("disabled schedule started %d jobs", ck.ScheduledJobsStarted)
	}
	if n := len(f.instances()); n != 3 {
		t.Fatalf("disabled schedule ran: %d instances", n)
	}

	// Delete, then it is gone from both reads.
	f.mustStatus(f.call("DELETE", one, f.token, nil, nil), http.StatusOK, "delete")
	f.mustStatus(f.call("GET", one, f.token, nil, nil), http.StatusNotFound, "get after delete")
	f.mustStatus(f.call("GET", f.schedulesURL(), f.token, nil, &list), http.StatusOK, "list after delete")
	if len(list.Value) != 0 {
		t.Fatalf("list after delete = %+v", list.Value)
	}
}

func TestScheduleWithPastStartTriggersInstantly(t *testing.T) {
	f := newScheduleFixture(t)
	// "If the start time is in the past, it will trigger a job instantly."
	body := map[string]any{"configuration": f.cronConfig(-60, 60)}
	f.mustStatus(f.call("POST", f.schedulesURL(), f.token, body, nil), http.StatusCreated, "create")
	runs := f.instances()
	if len(runs) != 1 || runs[0]["invokeType"] != "Scheduled" {
		t.Fatalf("instant trigger did not happen: %+v", runs)
	}
}

func TestScheduleLimitAndValidationOverHTTP(t *testing.T) {
	f := newScheduleFixture(t)
	body := map[string]any{"configuration": f.cronConfig(3600, 60)}
	for i := 0; i < 20; i++ {
		f.mustStatus(f.call("POST", f.schedulesURL(), f.token, body, nil),
			http.StatusCreated, fmt.Sprintf("create %d", i))
	}
	var e struct{ ErrorCode string }
	resp := f.call("POST", f.schedulesURL(), f.token, body, &e)
	f.mustStatus(resp, http.StatusBadRequest, "21st schedule")
	if e.ErrorCode != "ScheduleExceedsLimit" {
		t.Fatalf("errorCode = %q, want ScheduleExceedsLimit", e.ErrorCode)
	}
	// Fabric echoes the code in a header as well as the body.
	if got := resp.Header.Get("x-ms-public-api-error-code"); got != "ScheduleExceedsLimit" {
		t.Fatalf("error header = %q", got)
	}

	// An invalid configuration is refused rather than stored to never fire.
	bad := map[string]any{"configuration": map[string]any{
		"type": "Weekly", "times": []string{"09:00"},
		"startDateTime": "2024-01-01T00:00:00Z", "endDateTime": "2024-02-01T00:00:00Z",
		"localTimeZoneId": "UTC", // no weekdays
	}}
	f.mustStatus(f.call("POST", f.schedulesURL(), f.token, bad, nil), http.StatusBadRequest, "weekly without weekdays")
}

func TestJobsSubtreeDoesNotShadowTheInstanceRoutes(t *testing.T) {
	f := newScheduleFixture(t)
	// The schedule routes are dispatched from a `…/jobs/` prefix handler; the
	// pre-existing instance routes are more specific and must still win.
	var loc struct{ ID string }
	resp := f.call("POST", "/v1/workspaces/"+f.ws+"/items/"+f.item+"/jobs/instances?jobType=Pipeline",
		f.token, map[string]any{}, nil)
	f.mustStatus(resp, http.StatusAccepted, "run on demand")
	if resp.Header.Get("Location") == "" {
		t.Fatal("no Location on the on-demand run")
	}
	runs := f.instances()
	if len(runs) != 1 || runs[0]["invokeType"] != "Manual" {
		t.Fatalf("on-demand run = %+v", runs)
	}
	jid, _ := runs[0]["id"].(string)
	f.mustStatus(f.call("GET", "/v1/workspaces/"+f.ws+"/items/"+f.item+"/jobs/instances/"+jid,
		f.token, nil, &loc), http.StatusOK, "get instance")

	// A path under /jobs/ that nothing claims is an honest Fabric 404.
	var e struct{ ErrorCode string }
	resp = f.call("GET", "/v1/workspaces/"+f.ws+"/items/"+f.item+"/jobs/Pipeline/nonsense", f.token, nil, &e)
	f.mustStatus(resp, http.StatusNotFound, "unclaimed job route")
	if e.ErrorCode == "" {
		t.Fatal("404 was not a Fabric error envelope")
	}
}
