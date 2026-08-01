package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/schedule"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// Fabric's native per-item **Job Scheduler** API:
//
//	POST|GET   /v1/workspaces/{wid}/items/{iid}/jobs/{jobType}/schedules
//	GET|PATCH|DELETE  …/schedules/{sid}
//
// distinct from the `ApacheAirflowJob` item, which delegates scheduling to a
// real Airflow sidecar. This one is Fabric's own, and the emulator implements
// it — contract, validation, limits, and actual firing.
//
// # How a schedule fires without a background worker
//
// The emulator's defining property is a **controllable clock**: LROs and job
// status are derived from it, so a test advances virtual time instead of
// sleeping. A background goroutine ticking on wall time would break that — an
// operation's outcome would depend on how long the test took to run.
//
// So schedules are evaluated *on demand*, at the three moments a caller could
// observe the result:
//
//   - when the clock moves (`POST /_emulator/clock`) — the deterministic lever;
//   - when an item's job instances are listed (List Item Job Instances);
//   - when an item's schedules are listed or created.
//
// Each evaluation materialises every occurrence in the half-open window
// (firedThrough, now], so nothing fires twice and nothing is missed. Firing
// goes through the same startJob path a manual run uses, so a scheduled
// DataPipeline really executes; only `invokeType: "Scheduled"` distinguishes it.

func (a *API) registerSchedules(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/workspaces/{wid}/items/{iid}/jobs/instances", a.withAuth(a.listJobInstances))
	// The Job Scheduler routes cannot be registered as patterns.
	// `…/jobs/{jobType}/schedules` and the existing `…/jobs/instances/{jid}`
	// both match `…/jobs/instances/schedules`, and neither is more specific
	// than the other — which net/http rejects at registration time with a
	// panic. (A third, stricter pattern does not rescue it: ServeMux compares
	// patterns pairwise and has no such resolution rule.)
	//
	// So the schedule surface is dispatched from one subtree handler instead.
	// Every specific `…/jobs/…` route registered in api.go is more specific
	// than this prefix and still wins; what reaches here is what those do not
	// claim, which in practice is exactly the Job Scheduler API.
	mux.HandleFunc("/v1/workspaces/{wid}/items/{iid}/jobs/", a.withAuth(a.jobsSubtree))
}

// jobsSubtree routes the Job Scheduler API by hand — see registerSchedules for
// why the mux cannot do it.
func (a *API) jobsSubtree(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	// The pattern guarantees the prefix, so the segments after `jobs` are all
	// that vary: /v1/workspaces/{wid}/items/{iid}/jobs/<rest…>
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	rest := parts[6:]
	scheduleID := ""
	switch {
	case len(rest) == 3 && rest[1] == "schedules":
		scheduleID = rest[2]
	case len(rest) == 2 && rest[1] == "schedules":
	default:
		writeErr(w, http.StatusNotFound, "NotFound", "No such job route on this item.")
		return
	}
	jobType := rest[0]
	type route struct {
		method string
		byID   bool
		h      func(http.ResponseWriter, *http.Request, *auth.Principal, string, string)
	}
	for _, rt := range []route{
		{http.MethodPost, false, a.createItemSchedule},
		{http.MethodGet, false, a.listItemSchedules},
		{http.MethodGet, true, a.getItemSchedule},
		{http.MethodPatch, true, a.updateItemSchedule},
		{http.MethodDelete, true, a.deleteItemSchedule},
	} {
		if rt.method == r.Method && rt.byID == (scheduleID != "") {
			rt.h(w, r, p, jobType, scheduleID)
			return
		}
	}
	writeErr(w, http.StatusMethodNotAllowed, "MethodNotAllowed",
		r.Method+" is not supported on this schedule route.")
}

// scheduleBody renders the documented ItemSchedule resource. The stored
// configuration is spliced back in as JSON so a GET returns exactly the object
// the caller POSTed, not a re-serialised approximation of it.
func (a *API) scheduleBody(sc *store.ItemSchedule) map[string]any {
	body := map[string]any{
		"id":              sc.ID,
		"enabled":         sc.Enabled,
		"createdDateTime": time.Unix(sc.CreatedAt, 0).UTC().Format(time.RFC3339),
		"configuration":   json.RawMessage(sc.Configuration),
		"owner":           map[string]any{"id": sc.OwnerID, "type": sc.OwnerType},
	}
	if sc.ExecutionData != "" {
		body["executionData"] = json.RawMessage(sc.ExecutionData)
	}
	return body
}

// scheduleTarget resolves and authorises the {wid}/{iid}/{jobType} triple every
// handler here starts from.
func (a *API) scheduleTarget(w http.ResponseWriter, r *http.Request, p *auth.Principal, min string) (*store.Item, bool) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, min); !ok {
		return nil, false
	}
	it, err := a.Store.GetItem(wid, r.PathValue("iid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "ItemNotFound", "The item is not available.")
		return nil, false
	}
	return it, true
}

// scheduleRequest is the create/update payload.
type scheduleRequest struct {
	Enabled       *bool           `json:"enabled"`
	Configuration json.RawMessage `json:"configuration"`
	ExecutionData json.RawMessage `json:"executionData"`
}

// readScheduleRequest decodes and validates a payload, writing the Fabric error
// itself when the configuration is rejected.
func readScheduleRequest(w http.ResponseWriter, r *http.Request) (*scheduleRequest, bool) {
	var req scheduleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "The request body is not valid JSON.")
		return nil, false
	}
	if len(req.Configuration) == 0 {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "configuration is required.")
		return nil, false
	}
	if _, err := schedule.Parse(req.Configuration); err != nil {
		var se *schedule.Error
		if errors.As(err, &se) {
			writeErr(w, http.StatusBadRequest, se.Code, se.Message)
		} else {
			writeErr(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		}
		return nil, false
	}
	return &req, true
}

func (a *API) createItemSchedule(w http.ResponseWriter, r *http.Request, p *auth.Principal, jobType, scheduleID string) {
	it, ok := a.scheduleTarget(w, r, p, store.RoleContributor)
	if !ok {
		return
	}
	req, ok := readScheduleRequest(w, r)
	if !ok {
		return
	}
	// The documented ceiling, per item and job type.
	n, err := a.Store.CountItemSchedules(it.ID, jobType)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	if n >= store.MaxSchedulesPerItem {
		writeErr(w, http.StatusBadRequest, "ScheduleExceedsLimit",
			fmt.Sprintf("An item may have at most %d schedules per job type.", store.MaxSchedulesPerItem))
		return
	}
	sc := &store.ItemSchedule{
		WorkspaceID: it.WorkspaceID, ItemID: it.ID, JobType: jobType,
		Enabled: req.Enabled == nil || *req.Enabled, Configuration: string(req.Configuration),
		ExecutionData: string(req.ExecutionData), OwnerID: p.ID, OwnerType: "User",
	}
	if err := a.Store.CreateItemSchedule(sc); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	// "If the start time is in the past, it will trigger a job instantly."
	// Nothing special-cases that: the first evaluation's window opens at the
	// schedule's start, so a start already behind the clock is simply due.
	a.tickItemSchedules(it.ID)
	if fresh, err := a.Store.GetItemSchedule(it.ID, jobType, sc.ID); err == nil {
		sc = fresh
	}
	writeJSON(w, http.StatusCreated, a.scheduleBody(sc))
}

func (a *API) listItemSchedules(w http.ResponseWriter, r *http.Request, p *auth.Principal, jobType, scheduleID string) {
	it, ok := a.scheduleTarget(w, r, p, store.RoleViewer)
	if !ok {
		return
	}
	a.tickItemSchedules(it.ID)
	list, err := a.Store.ListItemSchedules(it.ID, jobType)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, sc := range list {
		out = append(out, a.scheduleBody(sc))
	}
	writePage(a, w, r, out)
}

func (a *API) getItemSchedule(w http.ResponseWriter, r *http.Request, p *auth.Principal, jobType, scheduleID string) {
	it, ok := a.scheduleTarget(w, r, p, store.RoleViewer)
	if !ok {
		return
	}
	sc, err := a.Store.GetItemSchedule(it.ID, jobType, scheduleID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "ScheduleNotFound", "No such schedule on this item and job type.")
		return
	}
	writeJSON(w, http.StatusOK, a.scheduleBody(sc))
}

func (a *API) updateItemSchedule(w http.ResponseWriter, r *http.Request, p *auth.Principal, jobType, scheduleID string) {
	it, ok := a.scheduleTarget(w, r, p, store.RoleContributor)
	if !ok {
		return
	}
	sc, err := a.Store.GetItemSchedule(it.ID, jobType, scheduleID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "ScheduleNotFound", "No such schedule on this item and job type.")
		return
	}
	req, ok := readScheduleRequest(w, r)
	if !ok {
		return
	}
	// A replaced configuration is a new series: the old high-water mark refers
	// to occurrences of a schedule that no longer exists, and keeping it would
	// silently swallow the new one's early occurrences.
	if string(req.Configuration) != sc.Configuration {
		sc.FiredThrough = 0
	}
	sc.Configuration = string(req.Configuration)
	if req.Enabled != nil {
		sc.Enabled = *req.Enabled
	}
	if len(req.ExecutionData) > 0 {
		sc.ExecutionData = string(req.ExecutionData)
	}
	if err := a.Store.UpdateItemSchedule(sc); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	a.tickItemSchedules(it.ID)
	if fresh, err := a.Store.GetItemSchedule(it.ID, jobType, sc.ID); err == nil {
		sc = fresh
	}
	writeJSON(w, http.StatusOK, a.scheduleBody(sc))
}

func (a *API) deleteItemSchedule(w http.ResponseWriter, r *http.Request, p *auth.Principal, jobType, scheduleID string) {
	it, ok := a.scheduleTarget(w, r, p, store.RoleContributor)
	if !ok {
		return
	}
	if err := a.Store.DeleteItemSchedule(it.ID, jobType, scheduleID); err != nil {
		writeErr(w, http.StatusNotFound, "ScheduleNotFound", "No such schedule on this item and job type.")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// ---- evaluation ----

// TickSchedules evaluates every enabled schedule in the tenant and returns how
// many job instances it started. The server calls it whenever the clock moves.
func (a *API) TickSchedules() int {
	list, err := a.Store.ListEnabledSchedules()
	if err != nil {
		return 0
	}
	return a.fireDue(list)
}

// tickItemSchedules is TickSchedules narrowed to one item, so an RBAC-scoped
// read only ever has side effects on the item being read.
func (a *API) tickItemSchedules(itemID string) int {
	list, err := a.Store.ListEnabledSchedulesForItem(itemID)
	if err != nil {
		return 0
	}
	return a.fireDue(list)
}

// fireDue materialises the occurrences each schedule owes, serialised so two
// concurrent evaluations cannot both read the same high-water mark and start
// the same run twice.
func (a *API) fireDue(list []*store.ItemSchedule) int {
	a.tickMu.Lock()
	defer a.tickMu.Unlock()
	started := 0
	now := time.Unix(a.Store.Now(), 0)
	for _, sc := range list {
		cfg, err := schedule.Parse([]byte(sc.Configuration))
		if err != nil {
			continue // validated on the way in; a stored config cannot be invalid
		}
		// A never-fired schedule's window opens just before its start, so the
		// occurrence *at* startDateTime counts.
		after := time.Unix(sc.FiredThrough, 0)
		if sc.FiredThrough == 0 {
			after = cfg.Start().Add(-time.Second)
		}
		// Re-read under the lock: a concurrent evaluation may have advanced it.
		if fresh, err := a.Store.GetItemSchedule(sc.ItemID, sc.JobType, sc.ID); err == nil {
			if fresh.FiredThrough != 0 {
				after = time.Unix(fresh.FiredThrough, 0)
			}
			sc = fresh
		}
		occurrences, _ := cfg.Occurrences(after, now)
		if len(occurrences) == 0 {
			continue
		}
		it, err := a.Store.GetItem(sc.WorkspaceID, sc.ItemID)
		if err != nil {
			continue
		}
		var exec map[string]any
		if sc.ExecutionData != "" {
			_ = json.Unmarshal([]byte(sc.ExecutionData), &exec)
		}
		for range occurrences {
			if _, err := a.startJob(sc.WorkspaceID, it, sc.JobType, store.InvokeScheduled, exec); err != nil {
				break
			}
			started++
		}
		_ = a.Store.SetScheduleFiredThrough(sc.ID, occurrences[len(occurrences)-1].Unix())
	}
	return started
}
