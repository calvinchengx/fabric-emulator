package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// Event triggers — the emulator's Data Activator (Reflex) execution engine.
//
// # What Fabric does
//
// Adding a Trigger to a pipeline creates a **Reflex** item fed by an
// **Eventstream**. The stream carries OneLake file events (among other
// sources), the Reflex filters them on `subject`, and a match starts the
// pipeline with the event exposed as `@pipeline()?.TriggerEvent?.FileName`.
//
// # What the emulator does
//
// The same thing, without a broker — because it does not need one. Every byte
// written to OneLake passes through this emulator's own storage layer, so a
// file event is observable **at the source** whoever wrote it: an ADLS client,
// azcopy, delta-rs, a Copy activity, the mirror writer. `store.FileEvents`
// is that hook; DispatchFileEvent below is what the server subscribes with.
//
// The **binding** has no public REST in Fabric (it is portal-only), so the
// control surface here is emulator-native and labelled as such in
// docs/parity.md. What is faithful is everything downstream of the binding:
// the filter, the invocation, the `TriggerEvent` parameters, and the fact that
// a real pipeline really runs.
//
// # Cycles
//
// Dispatch is synchronous and reentrant: a triggered pipeline writes files,
// which emit events, which may fire further triggers. That is a feature — it
// is how a bronze→silver→gold chain behaves — but a cycle would recurse
// forever. `firing` holds the triggers currently on the stack, and a trigger
// already on it does not fire again, so any cycle is cut at its first repeat
// while genuine chains still run.

func (a *API) registerTriggers(mux *http.ServeMux) {
	base := "/v1/workspaces/{wid}/reflexes/{iid}/triggers"
	mux.HandleFunc("POST "+base, a.withAuth(a.createEventTrigger))
	mux.HandleFunc("GET "+base, a.withAuth(a.listEventTriggers))
	mux.HandleFunc("GET "+base+"/{tid}", a.withAuth(a.getEventTrigger))
	mux.HandleFunc("PATCH "+base+"/{tid}", a.withAuth(a.updateEventTrigger))
	mux.HandleFunc("DELETE "+base+"/{tid}", a.withAuth(a.deleteEventTrigger))
}

// triggerRequest is the create/update payload.
type triggerRequest struct {
	DisplayName *string `json:"displayName"`
	Enabled     *bool   `json:"enabled"`
	EventType   *string `json:"eventType"`
	Source      *struct {
		ItemID     string `json:"itemId"`
		PathPrefix string `json:"pathPrefix"`
	} `json:"source"`
	Action *struct {
		WorkspaceID string `json:"workspaceId"`
		ItemID      string `json:"itemId"`
		JobType     string `json:"jobType"`
	} `json:"action"`
}

var knownEventTypes = map[string]bool{
	store.EventFileCreated: true,
	store.EventFileDeleted: true,
	store.EventFileRenamed: true,
}

func triggerBody(t *store.EventTrigger) map[string]any {
	return map[string]any{
		"id":              t.ID,
		"displayName":     t.DisplayName,
		"enabled":         t.Enabled,
		"eventType":       t.EventType,
		"createdDateTime": time.Unix(t.CreatedAt, 0).UTC().Format(time.RFC3339),
		"source":          map[string]any{"itemId": t.SourceItemID, "pathPrefix": t.PathPrefix},
		"action": map[string]any{
			"workspaceId": t.TargetWorkspaceID, "itemId": t.TargetItemID, "jobType": t.TargetJobType,
		},
	}
}

// reflexTarget resolves and authorises the Reflex the trigger hangs off.
func (a *API) reflexTarget(w http.ResponseWriter, r *http.Request, p *auth.Principal, min string) (*store.Item, bool) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, min); !ok {
		return nil, false
	}
	it, err := a.Store.GetItem(wid, r.PathValue("iid"))
	if err != nil || !strings.EqualFold(it.Type, "Reflex") {
		writeErr(w, http.StatusNotFound, "ItemNotFound", "No such Reflex in this workspace.")
		return nil, false
	}
	return it, true
}

// applyTriggerRequest validates a payload onto a trigger. `create` requires the
// fields a new trigger cannot do without; a PATCH leaves absent fields alone.
func (a *API) applyTriggerRequest(w http.ResponseWriter, r *http.Request, t *store.EventTrigger, create bool) bool {
	var req triggerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "The request body is not valid JSON.")
		return false
	}
	if req.DisplayName != nil {
		t.DisplayName = *req.DisplayName
	}
	if req.Enabled != nil {
		t.Enabled = *req.Enabled
	} else if create {
		t.Enabled = true
	}
	if req.EventType != nil {
		t.EventType = *req.EventType
	}
	if req.Source != nil {
		t.SourceItemID = req.Source.ItemID
		t.PathPrefix = strings.Trim(req.Source.PathPrefix, "/")
	}
	if req.Action != nil {
		t.TargetItemID = req.Action.ItemID
		t.TargetJobType = req.Action.JobType
		if req.Action.WorkspaceID != "" {
			t.TargetWorkspaceID = req.Action.WorkspaceID
		}
	}
	if !knownEventTypes[t.EventType] {
		writeErr(w, http.StatusBadRequest, "InvalidRequest",
			"eventType must be one of "+store.EventFileCreated+", "+store.EventFileDeleted+", "+store.EventFileRenamed+".")
		return false
	}
	if t.SourceItemID == "" {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "source.itemId is required.")
		return false
	}
	if t.TargetItemID == "" || t.TargetJobType == "" {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "action.itemId and action.jobType are required.")
		return false
	}
	// Resolve the target now rather than discovering at fire time that the
	// trigger can never do anything.
	if _, err := a.Store.GetItem(t.TargetWorkspaceID, t.TargetItemID); err != nil {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "action.itemId is not an item in that workspace.")
		return false
	}
	return true
}

func (a *API) createEventTrigger(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	reflex, ok := a.reflexTarget(w, r, p, store.RoleContributor)
	if !ok {
		return
	}
	t := &store.EventTrigger{
		WorkspaceID: reflex.WorkspaceID, ReflexID: reflex.ID,
		TargetWorkspaceID: reflex.WorkspaceID,
	}
	if !a.applyTriggerRequest(w, r, t, true) {
		return
	}
	if err := a.Store.CreateEventTrigger(t); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, triggerBody(t))
}

func (a *API) listEventTriggers(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	reflex, ok := a.reflexTarget(w, r, p, store.RoleViewer)
	if !ok {
		return
	}
	list, err := a.Store.ListEventTriggers(reflex.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, t := range list {
		out = append(out, triggerBody(t))
	}
	writePage(a, w, r, out)
}

func (a *API) getEventTrigger(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	reflex, ok := a.reflexTarget(w, r, p, store.RoleViewer)
	if !ok {
		return
	}
	t, err := a.Store.GetEventTrigger(reflex.ID, r.PathValue("tid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "TriggerNotFound", "No such trigger on this Reflex.")
		return
	}
	writeJSON(w, http.StatusOK, triggerBody(t))
}

func (a *API) updateEventTrigger(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	reflex, ok := a.reflexTarget(w, r, p, store.RoleContributor)
	if !ok {
		return
	}
	t, err := a.Store.GetEventTrigger(reflex.ID, r.PathValue("tid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "TriggerNotFound", "No such trigger on this Reflex.")
		return
	}
	if !a.applyTriggerRequest(w, r, t, false) {
		return
	}
	if err := a.Store.UpdateEventTrigger(t); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, triggerBody(t))
}

func (a *API) deleteEventTrigger(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	reflex, ok := a.reflexTarget(w, r, p, store.RoleContributor)
	if !ok {
		return
	}
	if err := a.Store.DeleteEventTrigger(reflex.ID, r.PathValue("tid")); err != nil {
		writeErr(w, http.StatusNotFound, "TriggerNotFound", "No such trigger on this Reflex.")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// ---- dispatch ----

// maxTriggerActivations bounds how many activations of ONE trigger may be in
// flight at once. Named after Fabric's own limit, and chosen to match it.
//
// Real Fabric bounds a runaway by RATE, not identity: Activator documents
// "Fabric item — Activations/user/minute — 50", and "if an action exceeds the
// limit, Activator might throttle or cancel the action". It also caps input at
// 10,000 events/second/rule, above which "Activator stops your rule". Nothing
// in the Activator docs describes loop detection or dedup — see
// docs/parity.md for the citation.
const maxTriggerActivations = 50

// firingSet counts the activations of each trigger currently in flight, so a
// runaway is bounded while independent events are not suppressed. Its own
// mutex, held only around the map: dispatch runs whole pipelines, and holding a
// lock across that would serialise the whole emulator.
//
// This used to be a SET — one activation per trigger, so a trigger already on
// the stack did not fire at all. That cut cycles at the first repeat, and it
// also suppressed something real Fabric does not: two files landing in one
// watched folder at the same instant ran the pipeline once rather than twice.
// Fabric's Activator "continues monitoring without waiting for the action to
// complete", which is explicitly what "enables scalable workflows that can
// process many events simultaneously" — independent events each activate.
//
// So the bound is now a COUNT rather than an identity, which is the shape
// Fabric's own limits take. A bronze->silver->gold chain is depth 3; a cycle
// climbs to the cap and is cut there.
//
// KNOWN RESIDUAL GAP, stated because it is a trade rather than an oversight:
// this counter cannot tell 50 nested activations (a cycle) from 50 concurrent
// independent ones (a burst), because dispatch is synchronous on the writer's
// goroutine (store.emitFileEvent calls FileEvents inline), Go has no
// goroutine-local storage, and threading a chain token through the store's
// write API to separate them would reach into every OneLake mutation. Raising
// the bound from 1 to 50 shrinks that gap by a factor of 50 without paying for
// the plumbing; it does not close it. A burst of more than 50 simultaneous
// writes matching one trigger still loses the excess, where Fabric would
// throttle at the same number.
type firingSet struct {
	mu sync.Mutex
	on map[string]int
}

func (f *firingSet) enter(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.on == nil {
		f.on = map[string]int{}
	}
	if f.on[id] >= maxTriggerActivations {
		return false
	}
	f.on[id]++
	return true
}

func (f *firingSet) leave(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Decrement, not delete: with a count, deleting would drop the OTHER
	// in-flight activations of the same trigger and let a cycle run forever.
	if f.on[id] <= 1 {
		delete(f.on, id)
		return
	}
	f.on[id]--
}

// DispatchFileEvent starts the item job of every trigger matching ev, and
// returns how many it started. Wired to store.FileEvents by the server.
func (a *API) DispatchFileEvent(ev store.FileEvent) int {
	triggers, err := a.Store.TriggersForItem(ev.ItemID)
	if err != nil {
		return 0
	}
	started := 0
	for _, t := range triggers {
		if !t.Matches(ev) {
			continue
		}
		if !a.firing.enter(t.ID) {
			continue // already on the stack: this is a cycle, cut it here
		}
		if a.fireTrigger(t, ev) {
			started++
		}
		a.firing.leave(t.ID)
	}
	return started
}

// fireTrigger starts one trigger's job with the event bound into it.
func (a *API) fireTrigger(t *store.EventTrigger, ev store.FileEvent) bool {
	target, err := a.Store.GetItem(t.TargetWorkspaceID, t.TargetItemID)
	if err != nil {
		return false // the target was deleted out from under the trigger
	}
	exec := map[string]any{"triggerEvent": triggerEventParams(ev)}
	if _, err := a.startJob(t.TargetWorkspaceID, target, t.TargetJobType,
		store.InvokeEventTriggered, exec); err != nil {
		return false
	}
	return true
}

// triggerEventParams is what `@pipeline()?.TriggerEvent?.…` sees. The field
// names are Fabric's, taken from its own trigger samples — `FileName` and
// `FolderPath` are the two the documentation actually shows being read.
func triggerEventParams(ev store.FileEvent) map[string]any {
	folder := path.Dir(ev.RelPath)
	if folder == "." {
		folder = ""
	}
	return map[string]any{
		"EventType":   ev.Type,
		"Source":      ev.ItemID,
		"Subject":     ev.RelPath,
		"FileName":    path.Base(ev.RelPath),
		"FolderPath":  folder,
		"WorkspaceId": ev.WorkspaceID,
		"ItemId":      ev.ItemID,
	}
}
